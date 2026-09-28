package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Dir is the local directory that backs agk run --local: an Objects holding one file per
// key under root. It is what lets a run on a laptop take the same code path as a run on
// a fleet, with no object store behind it and nothing to configure.
//
// A key becomes a path under root, so the objects of two namespaces sit in two
// directories and an existence check still cannot reach across one. It is also the
// built-in store of a server, AGK_OBJECTS_DIR, which is why it can remove an object, why it
// is Walkable, for the collector to find the objects nothing names, and why it is Ranged, for
// the packs of a workflow repository to be read one entry at a time.
func Dir(root string) Removable {
	return dir{root: root}
}

var (
	_ Walkable = dir{}
	_ Ranged   = dir{}
)

type dir struct {
	root string
}

func (d dir) path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	if d.root == "" {
		return "", errors.New("artifact: no root directory")
	}
	return filepath.Join(d.root, filepath.FromSlash(key)), nil
}

func (d dir) Has(ctx context.Context, key string) (bool, error) {
	p, err := d.path(key)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	switch _, err := os.Stat(p); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		// Absence is the expected answer on the way to a write, not a failure.
		return false, nil
	default:
		return false, fmt.Errorf("artifact: object %s: %w", key, err)
	}
}

func (d dir) Put(ctx context.Context, key string, r io.Reader) (err error) {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Staged beside the object rather than in the system temporary directory, so that the
	// rename that publishes it stays on one filesystem and is therefore atomic: a reader
	// sees the whole object under the key or nothing at all.
	stage, err := staging(filepath.Dir(p))
	if err != nil {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}
	staged := stage.Name()
	defer func() {
		stage.Close()
		// Harmless once the rename has happened, and the whole of the cleanup when
		// anything before it failed.
		os.Remove(staged)
	}()

	if _, err := io.Copy(stage, ctxReader{ctx: ctx, r: r}); err != nil {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}
	if err := stage.Sync(); err != nil {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}
	if err := stage.Close(); err != nil {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}
	// An object is its digest, so its bytes never change again. A mode that says so
	// turns a later accidental write into an error rather than a silent divergence
	// between an object and the key addressing it.
	if err := os.Chmod(staged, 0o444); err != nil {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}
	if err := os.Rename(staged, p); err != nil {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}
	return nil
}

// staging makes dir and the file a write is staged in there. A Remove of the last object under dir
// removes dir as well, and may do so between the two, which leaves the file nowhere to be made: dir
// is made again then, up to three times, a failure that lasts answering the same each time. Once the
// file is there dir is not empty, and no Remove takes it until the write has renamed it.
func staging(dir string) (*os.File, error) {
	for attempt := 1; ; attempt++ {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		f, err := os.CreateTemp(dir, ".staging-*")
		if err == nil || attempt == 3 {
			return f, err
		}
	}
}

// Remove deletes one object, and then each directory its key made that it leaves empty, up to
// the key's first two segments.
//
// The directories go too because a log is written under a directory of its own, run, task and
// dispatch, and months of runs would otherwise leave millions of empty ones behind. Only one that
// is empty goes, which the operating system decides in one step, so a directory another object is
// being written to is never removed from under it: nothing is written to a log's directories
// once its run's retention has run out, which is the only time its objects are removed, and the
// namespace's sha256 directory, which every artifact and envelope of the namespace is written to,
// is one of the two segments kept.
func (d dir) Remove(ctx context.Context, key string) (bool, error) {
	p, err := d.path(key)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	removed := true
	if err := os.Remove(p); errors.Is(err, fs.ErrNotExist) {
		removed = false
	} else if err != nil {
		return false, fmt.Errorf("artifact: object %s: %w", key, err)
	}
	for parent := path.Dir(key); strings.Count(parent, "/") >= 2; parent = path.Dir(parent) {
		err := os.Remove(filepath.Join(d.root, filepath.FromSlash(parent)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Not empty, most likely, and then nothing above it is empty either. One already
			// gone is what a purge that died on its way up leaves, and it goes on up.
			break
		}
	}
	return removed, nil
}

func (d dir) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		// os.Open's error already satisfies errors.Is(err, fs.ErrNotExist), and wrapping
		// keeps it that way while naming the key.
		return nil, fmt.Errorf("artifact: object %s: %w", key, err)
	}
	return f, nil
}

// OpenRange opens one object to be read at any offset, which an os.File is: ReadAt is pread, which
// several goroutines may call at once.
func (d dir) OpenRange(ctx context.Context, key string) (RangeReader, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("artifact: object %s: %w", key, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("artifact: object %s: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("artifact: object %s is not a file", key)
	}
	return ranged{File: f, size: info.Size()}, nil
}

// ranged is an object held open, with the size it had when it was opened, which is its size for
// good: the bytes under a key never change, a write of them again renames the same bytes over them,
// and a rename that replaces the file leaves this one as it was.
type ranged struct {
	*os.File
	size int64
}

func (r ranged) Size() int64 { return r.size }

// Walk lists the objects of one namespace: the files of its sha256 directory named as a digest is,
// and nothing else. A directory, a link, a write being staged and a name that is not sixty-four
// lowercase hexadecimal characters are passed over, and the logs are in another directory, which a
// walk never opens. A namespace that holds no object yet is a walk that answers none.
//
// The directory is held open for as long as the walk is, so that a walk taken up again a pass later
// goes on from where it stopped rather than reading a million names again to find its place.
func (d dir) Walk(namespace string) (Walk, error) {
	if err := checkNamespace(namespace); err != nil {
		return nil, err
	}
	if d.root == "" {
		return nil, errors.New("artifact: no root directory")
	}
	f, err := os.Open(filepath.Join(d.root, namespace, "sha256"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &dirWalk{}, nil
	case err != nil:
		return nil, fmt.Errorf("artifact: the objects of namespace %s: %w", namespace, err)
	}
	return &dirWalk{f: f, namespace: namespace}, nil
}

type dirWalk struct {
	f         *os.File
	namespace string
}

func (w *dirWalk) Next(ctx context.Context, n int) ([]Stored, bool, error) {
	if w.f == nil {
		return nil, false, nil
	}
	if n <= 0 {
		return nil, false, fmt.Errorf("artifact: a walk asked for %d objects", n)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	entries, err := w.f.ReadDir(n)
	switch {
	case errors.Is(err, io.EOF):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("artifact: the objects of namespace %s: %w", w.namespace, err)
	}
	out := make([]Stored, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() || !digestName(e.Name()) {
			continue
		}
		// Asked of the entry itself and never through a link, and an entry removed since the
		// directory was read is one the walk no longer has to answer.
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("artifact: object %s: %w", Key(w.namespace, e.Name()), err)
		}
		out = append(out, Stored{Digest: e.Name(), Size: info.Size(), Written: info.ModTime()})
	}
	return out, true, nil
}

func (w *dirWalk) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// digestName is sixty-four lowercase hexadecimal characters, the one name Key gives an object.
func digestName(name string) bool {
	return len(name) == 64 && strings.Trim(name, "0123456789abcdef") == ""
}
