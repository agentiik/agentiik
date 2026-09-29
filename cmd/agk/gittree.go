package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// gitTree is a commit's tree as git listed it, whose files are read out of git when each is first
// read and not before: the validation reads the entry point, what it includes and the schemas its
// inputs name, and a tree of large files is not read into memory to judge a workflow of a few
// kilobytes. Every size is known from the listing, so a reader that asks before it reads, as the
// validation's does, refuses a file past its bound without reading a byte of it.
type gitTree struct {
	ctx   context.Context
	top   string
	files map[string]gitBlob
	dirs  map[string][]fs.DirEntry
}

// gitBlob is one file of the listing: its object, its mode and its size.
type gitBlob struct {
	object string
	mode   fs.FileMode
	size   int64
}

// add lists a file, and every directory above it.
func (t *gitTree) add(name string, b gitBlob) {
	t.files[name] = b
	child := fs.FileInfoToDirEntry(treeInfo{name: path.Base(name), mode: b.mode, size: b.size})
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		_, known := t.dirs[dir]
		t.dirs[dir] = append(t.dirs[dir], child)
		if known || dir == "." {
			return
		}
		child = fs.FileInfoToDirEntry(treeInfo{name: path.Base(dir), mode: fs.ModeDir | 0o555})
	}
}

func (t *gitTree) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if b, ok := t.files[name]; ok {
		return &gitFile{tree: t, name: name, blob: b}, nil
	}
	if entries, ok := t.dirs[name]; ok {
		sorted := slices.Clone(entries)
		slices.SortFunc(sorted, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		return &gitDir{name: name, entries: sorted}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadFile reads one file whole, through Open, so that its size is what bounds it.
func (t *gitTree) ReadFile(name string) ([]byte, error) {
	f, err := t.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// blob reads one object of the listing, of the size the listing gave it.
func (t *gitTree) blob(b gitBlob) ([]byte, error) {
	out, err := output(fetchingNothing(gitCommand(t.ctx, t.top, "cat-file", "blob", b.object)))
	if err != nil {
		return nil, fmt.Errorf("%s could not be read from git: %w", b.object, err)
	}
	if int64(len(out)) != b.size {
		return nil, fmt.Errorf("%s is %d bytes, where git listed it at %d", b.object, len(out), b.size)
	}
	return out, nil
}

// gitFile is a file of the tree, read from git at its first Read.
type gitFile struct {
	tree *gitTree
	name string
	blob gitBlob
	r    *bytes.Reader
}

func (f *gitFile) Stat() (fs.FileInfo, error) {
	return treeInfo{name: path.Base(f.name), mode: f.blob.mode, size: f.blob.size}, nil
}

func (f *gitFile) Read(p []byte) (int, error) {
	if f.r == nil {
		content, err := f.tree.blob(f.blob)
		if err != nil {
			return 0, &fs.PathError{Op: "read", Path: f.name, Err: err}
		}
		f.r = bytes.NewReader(content)
	}
	return f.r.Read(p)
}

func (f *gitFile) Close() error { return nil }

// gitDir is a directory of the tree, its entries in name order, as fs.ReadDir sorts them.
type gitDir struct {
	name    string
	entries []fs.DirEntry
	read    int
}

func (d *gitDir) Stat() (fs.FileInfo, error) {
	return treeInfo{name: path.Base(d.name), mode: fs.ModeDir | 0o555}, nil
}

func (d *gitDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *gitDir) Close() error { return nil }

func (d *gitDir) ReadDir(n int) ([]fs.DirEntry, error) {
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

// treeInfo is what a file or a directory of the tree says of itself.
type treeInfo struct {
	name string
	mode fs.FileMode
	size int64
}

func (i treeInfo) Name() string       { return i.name }
func (i treeInfo) Size() int64        { return i.size }
func (i treeInfo) Mode() fs.FileMode  { return i.mode }
func (i treeInfo) ModTime() time.Time { return time.Time{} }
func (i treeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i treeInfo) Sys() any           { return nil }
