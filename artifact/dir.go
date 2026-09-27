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
// built-in store of a server, AGK_OBJECTS_DIR, which is why it can remove an object.
func Dir(root string) Removable {
	return dir{root: root}
}

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
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}

	// Staged beside the object rather than in the system temporary directory, so that the
	// rename that publishes it stays on one filesystem and is therefore atomic: a reader
	// sees the whole object under the key or nothing at all.
	stage, err := os.CreateTemp(filepath.Dir(p), ".staging-*")
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
func (d dir) Remove(ctx context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("artifact: object %s: %w", key, err)
	}
	for parent := path.Dir(key); strings.Count(parent, "/") >= 2; parent = path.Dir(parent) {
		err := os.Remove(filepath.Join(d.root, filepath.FromSlash(parent)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Not empty, most likely, and then nothing above it is empty either. One already
			// gone is what a purge that died on its way up leaves, and it goes on up.
			break
		}
	}
	return nil
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
