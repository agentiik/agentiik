package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

// TreeFS is a tree as an fs.FS: the tree of a pushed commit, for the validation that reads its
// entry point and what it includes. Nothing is read before it is asked for: each tree is read
// through the lookup the first time a path crosses it, and each file when it is opened, so that
// reading one file of a large tree reads the trees above it and that file alone.
//
// A symbolic link is never followed, and a submodule names a commit of another repository: each
// is listed and stat'ed as what it is, fs.ModeSymlink or fs.ModeIrregular, and opening one, or a
// path through one, is refused. A file's Sys is its TreeEntry, for its mode and its blob's ID.
type TreeFS struct {
	ctx     context.Context
	objects Lookup
	root    ID
	mu      sync.Mutex
	trees   map[ID][]TreeEntry
}

// NewTreeFS reads the tree named root through objects, each read under ctx.
func NewTreeFS(ctx context.Context, objects Lookup, root ID) *TreeFS {
	return &TreeFS{ctx: ctx, objects: objects, root: root, trees: map[ID][]TreeEntry{}}
}

// tree reads a tree, once.
func (t *TreeFS) tree(id ID) ([]TreeEntry, error) {
	t.mu.Lock()
	entries, ok := t.trees[id]
	t.mu.Unlock()
	if ok {
		return entries, nil
	}
	typ, data, err := ReadObject(t.ctx, t.objects, id, maxHeld)
	if err != nil {
		return nil, err
	}
	if typ != TypeTree {
		return nil, fmt.Errorf("repo: %s is named as a tree, and is a %s", id, typ)
	}
	if entries, err = ParseTree(data); err != nil {
		var oe *ObjectError
		if errors.As(err, &oe) {
			oe.ID = id
		}
		return nil, err
	}
	t.mu.Lock()
	t.trees[id] = entries
	t.mu.Unlock()
	return entries, nil
}

// find answers the entry name names, the root being a tree entry of its own named ".".
func (t *TreeFS) find(op, name string) (TreeEntry, error) {
	if !fs.ValidPath(name) {
		return TreeEntry{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	e := TreeEntry{Name: ".", Mode: ModeTree, ID: t.root}
	if name == "." {
		return e, nil
	}
	walked := ""
	for _, part := range strings.Split(name, "/") {
		switch e.Mode {
		case ModeTree:
		case ModeSymlink:
			return TreeEntry{}, &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%s is a symbolic link, which is never followed", walked)}
		case ModeSubmodule:
			return TreeEntry{}, &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%s is a submodule, whose tree is another repository's", walked)}
		default:
			return TreeEntry{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
		entries, err := t.tree(e.ID)
		if err != nil {
			return TreeEntry{}, &fs.PathError{Op: op, Path: name, Err: err}
		}
		i := slices.IndexFunc(entries, func(e TreeEntry) bool { return e.Name == part })
		if i < 0 {
			return TreeEntry{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
		e = entries[i]
		walked = path.Join(walked, part)
	}
	return e, nil
}

// Open opens the file, or the directory, name names.
func (t *TreeFS) Open(name string) (fs.File, error) {
	e, err := t.find("open", name)
	if err != nil {
		return nil, err
	}
	info := &fileInfo{name: path.Base(name), e: e}
	switch {
	case e.Mode == ModeTree:
		entries, err := t.tree(e.ID)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return &treeDir{info: info, entries: t.dirEntries(entries)}, nil
	case e.Mode.IsRegular():
		r, err := t.objects.OpenObject(t.ctx, e.ID)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		if r.Type() != TypeBlob {
			r.Close()
			return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("repo: %s is named as a file, and is a %s", e.ID, r.Type())}
		}
		info.size = r.Size()
		return &treeFile{info: info, r: r}, nil
	case e.Mode == ModeSymlink:
		return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("a symbolic link, which is never followed")}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("a submodule, whose tree is another repository's")}
}

// Stat answers what name is, without following it where it is a symbolic link.
func (t *TreeFS) Stat(name string) (fs.FileInfo, error) {
	e, err := t.find("stat", name)
	if err != nil {
		return nil, err
	}
	info, err := t.info(e, path.Base(name))
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return info, nil
}

// info is what an entry is, with the size of its blob, which a tree does not carry and which is
// read from the object's header.
func (t *TreeFS) info(e TreeEntry, name string) (fs.FileInfo, error) {
	info := &fileInfo{name: name, e: e}
	if e.Mode.IsRegular() || e.Mode == ModeSymlink {
		r, err := t.objects.OpenObject(t.ctx, e.ID)
		if err != nil {
			return nil, err
		}
		info.size = r.Size()
		r.Close()
	}
	return info, nil
}

// ReadDir answers the entries of the directory name names, sorted by name.
func (t *TreeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	e, err := t.find("readdir", name)
	if err != nil {
		return nil, err
	}
	if e.Mode != ModeTree {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	entries, err := t.tree(e.ID)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	return t.dirEntries(entries), nil
}

// ReadFile answers the content of the file name names.
func (t *TreeFS) ReadFile(name string) ([]byte, error) {
	f, err := t.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	file, ok := f.(*treeFile)
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("is a directory")}
	}
	data := make([]byte, file.info.size)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	// Read to the end, which is where the object is checked against its ID.
	if n, err := file.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		if err == nil || err == io.EOF {
			err = errors.New("longer than its size")
		}
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	return data, nil
}

// dirEntries lists a tree as fs.ReadDir does: by name, which is not git's order where a directory
// sorts as if its name ended with a slash.
func (t *TreeFS) dirEntries(entries []TreeEntry) []fs.DirEntry {
	list := make([]fs.DirEntry, len(entries))
	for i, e := range entries {
		list[i] = dirEntry{fs: t, e: e}
	}
	slices.SortFunc(list, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return list
}

type dirEntry struct {
	fs *TreeFS
	e  TreeEntry
}

func (d dirEntry) Name() string               { return d.e.Name }
func (d dirEntry) IsDir() bool                { return d.e.Mode == ModeTree }
func (d dirEntry) Type() fs.FileMode          { return fileMode(d.e.Mode).Type() }
func (d dirEntry) Info() (fs.FileInfo, error) { return d.fs.info(d.e, d.e.Name) }

// fileMode is an entry's mode as fs says it. Nothing in a tree is written to, so no mode carries
// a write bit.
func fileMode(m Mode) fs.FileMode {
	switch m {
	case ModeTree:
		return fs.ModeDir | 0o555
	case ModeExecutable:
		return 0o555
	case ModeSymlink:
		return fs.ModeSymlink | 0o444
	case ModeSubmodule:
		return fs.ModeIrregular | 0o444
	}
	return 0o444
}

type fileInfo struct {
	name string
	e    TreeEntry
	size int64
}

func (i *fileInfo) Name() string       { return i.name }
func (i *fileInfo) Size() int64        { return i.size }
func (i *fileInfo) Mode() fs.FileMode  { return fileMode(i.e.Mode) }
func (i *fileInfo) ModTime() time.Time { return time.Time{} }
func (i *fileInfo) IsDir() bool        { return i.e.Mode == ModeTree }
func (i *fileInfo) Sys() any           { return i.e }

type treeFile struct {
	info *fileInfo
	r    ObjectReader
}

func (f *treeFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *treeFile) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *treeFile) Close() error               { return f.r.Close() }

type treeDir struct {
	info    *fileInfo
	entries []fs.DirEntry
	read    int
}

func (d *treeDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *treeDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}
func (d *treeDir) Close() error { return nil }

// ReadDir is fs.ReadDirFile's: n entries at a time where n > 0, and io.EOF once none are left;
// all of what is left where n <= 0.
func (d *treeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	left := d.entries[d.read:]
	if n <= 0 {
		d.read = len(d.entries)
		return left, nil
	}
	if len(left) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(left))
	d.read += n
	return left[:n], nil
}
