package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
)

// Laying out the workflow repository a task runs against.
//
// "A runner still never speaks git and never holds a credential": the API names every file of the
// commit's tree with a URL for its object, and the runner writes those files into a directory of
// its own, which the driver binds read-only at /agk/repo (driver.Sources.Repo). Each file is held
// to the digest the tree names before the directory is handed over, so a tree that is not the
// commit's is refused before any container exists.
//
// The modes are what a remapped container can read. The container runs as an account of the
// remapped range, and the bind is what it reaches the tree through, so the directory and what is
// under it are the whole of the access decision: directories are 0755 and files 0444, or 0555
// where the tree gives the file an executable bit, since an entry point that arrives 0644 is a
// step that will not run. Nothing is writable by anybody but the agent, which keeps the write bit
// on directories so that it can take the tree away again.

// TreesDir is where the trees sit under the work root.
//
// The dot keeps it apart from the task directories beside it, whose first segment is a run
// identifier, for the reason driver.KeysDir has one. It is outside every task's directory,
// because the driver creates that directory fresh, removing whatever it held, when the task's
// container is prepared, which is after the tree is laid out.
const TreesDir = ".trees"

const (
	treeDirMode  os.FileMode = 0o755
	treeFileMode os.FileMode = 0o444
	treeExecMode os.FileMode = 0o555
)

// treeFetchers is how many files are fetched at once. A tree is up to 4096 files, one request
// each, and one after the other a task would wait on thousands of round trips before its
// container is created; a handful at once is most of the gain without a runner laying out several
// trees opening hundreds of connections to the store.
const treeFetchers = 8

// treeDir is the directory one task's tree is laid out in, named after the task's identity as the
// driver names its working directory, so two tasks can no more share a tree than an identity.
func treeDir(workRoot string, id agk.TaskID) (string, error) {
	if workRoot == "" {
		return "", errors.New("runner: no work root: a task's tree is laid out under one")
	}
	if err := id.Validate(); err != nil {
		return "", fmt.Errorf("runner: %w", err)
	}
	return filepath.Join(workRoot, TreesDir, filepath.FromSlash(string(id))), nil
}

// layOutTree writes every file of a tree under dir, fresh, each fetched through objects and held
// to its digest.
//
// A directory left by an earlier delivery of the task is removed first rather than reused, for
// the reason the driver gives its working directory: a file from the delivery before this one is
// a file nobody checked this time. On any refusal the directory is taken away again, so what is
// left behind is either a whole, checked tree or nothing.
//
// A file above artifact_max_bytes is refused as it arrives, since a tree file is an object of the
// store like any other and the store holds none larger. Files of one digest are fetched once and
// copied, since they are one object.
func layOutTree(ctx context.Context, objects artifact.Objects, namespace, dir string, entries []TreeEntry, l agk.Limits) (err error) {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("runner: the tree %s left by an earlier delivery could not be removed: %w", dir, err)
	}
	defer func() {
		if err != nil {
			if left := os.RemoveAll(dir); left != nil {
				err = fmt.Errorf("%w, and what was laid out was not all taken away: %v", err, left)
			}
		}
	}()
	if err := mkdirTree(dir); err != nil {
		return err
	}

	// The directories first, one after the other, so that the fetchers only ever create
	// files, and a file that some other entry needs as a directory is refused here.
	byDigest := map[string][]TreeEntry{}
	for _, e := range entries {
		if err := treePath(e.Path); err != nil {
			return fmt.Errorf("runner: %w", err)
		}
		if err := mkdirTree(filepath.Join(dir, filepath.FromSlash(path.Dir(e.Path)))); err != nil {
			return err
		}
		byDigest[e.SHA256] = append(byDigest[e.SHA256], e)
	}
	digests := make([]string, 0, len(byDigest))
	for d := range byDigest {
		digests = append(digests, d)
	}
	sort.Strings(digests)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan string)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			first = err
			cancel()
		}
	}
	for range min(treeFetchers, len(digests)) {
		wg.Go(func() {
			for digest := range work {
				if err := writeObject(ctx, objects, namespace, dir, digest, byDigest[digest], l); err != nil {
					fail(err)
				}
			}
		})
	}
feed:
	for _, d := range digests {
		select {
		case work <- d:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// mkdirTree creates a directory of the tree and its parents with the tree's mode, which is set
// rather than requested, since the process umask would otherwise decide it.
func mkdirTree(dir string) error {
	if err := os.MkdirAll(dir, treeDirMode); err != nil {
		return fmt.Errorf("runner: the tree's directory %s could not be created: %w", dir, err)
	}
	if err := os.Chmod(dir, treeDirMode); err != nil {
		return fmt.Errorf("runner: the tree's directory %s: %w", dir, err)
	}
	return nil
}

// writeObject fetches one object and writes it at every path of the tree that names it.
func writeObject(ctx context.Context, objects artifact.Objects, namespace, dir, digest string, at []TreeEntry, l agk.Limits) error {
	r, err := objects.Open(ctx, artifact.Key(namespace, digest))
	if err != nil {
		return fmt.Errorf("runner: the tree file %s could not be fetched: %w", at[0].Path, err)
	}
	defer r.Close()
	first := filepath.Join(dir, filepath.FromSlash(at[0].Path))
	if err := writeChecked(first, r, digest, at[0], l); err != nil {
		return err
	}
	for _, e := range at[1:] {
		src, err := os.Open(first)
		if err != nil {
			return fmt.Errorf("runner: the tree file %s: %w", e.Path, err)
		}
		err = writeChecked(filepath.Join(dir, filepath.FromSlash(e.Path)), src, digest, e, l)
		src.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeChecked writes one file of the tree, hashing it on the way, and gives it its mode only once
// the bytes are the ones the tree names. A file that already exists is refused rather than
// written through, since the directory was created empty and one there now is a second entry of
// one path, or something that is not this runner's.
func writeChecked(name string, r io.Reader, digest string, e TreeEntry, l agk.Limits) error {
	mode, err := treeMode(e.Mode)
	if err != nil {
		return fmt.Errorf("runner: the tree gives %s %w", e.Path, err)
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("runner: the tree file %s could not be created: %w", e.Path, err)
	}
	h := sha256.New()
	limit := l.ArtifactMaxBytes
	var from io.Reader = r
	if limit > 0 {
		from = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), from)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return fmt.Errorf("runner: the tree file %s could not be fetched: %w", e.Path, err)
	case limit > 0 && n > limit:
		return fmt.Errorf("%w: the tree file %s is longer than the %d bytes an object of the store may be", ErrNotAsNamed, e.Path, limit)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("%w: the tree file %s is named sha256 %s and its bytes are sha256 %s", ErrNotAsNamed, e.Path, digest, got)
	}
	perm := treeFileMode
	if mode&0o111 != 0 {
		perm = treeExecMode
	}
	if err := os.Chmod(name, perm); err != nil {
		return fmt.Errorf("runner: the tree file %s: %w", e.Path, err)
	}
	return nil
}
