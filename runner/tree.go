package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/driver"
)

// Laying out the workflow repository a task runs against.
//
// "A runner still never speaks git and never holds a credential": the API names every file of the
// commit's tree the step selected with a URL for its object, and the runner lays those files out
// in a directory of its own, which the driver binds read-only at /agk/repo (driver.Sources.Repo).
// Each file is held to the digest the tree names when it enters the namespace's cache, which the
// directory is linked from, so a tree that is not the commit's is refused before any container
// exists.
//
// The modes are what a remapped container can read. The container runs as an account of the
// remapped range, and the bind is what it reaches the tree through, so the directory and what is
// under it are the whole of the access decision: directories are 0755 and files 0444, or 0555
// where the tree gives the file an executable bit, since an entry point that arrives 0644 is a
// step that will not run. Nothing is writable by anybody but the agent, which keeps the write bit
// on directories so that it can take the tree away again.

// TreesDir is where the trees sit under the work root: each task's, named after the task, and
// each namespace's cache, named after the namespace (cache.go).
//
// The dot keeps it apart from the task directories beside it, whose first segment is a run
// identifier, for the reason driver.KeysDir has one. It is outside every task's directory,
// because the driver creates that directory fresh, removing whatever it held, when the task's
// container is prepared, which is after the tree is laid out.
//
// It is private to the agent, as the driver keeps the parents of a task's directory, so that the
// permissive modes of a tree sit behind a directory nothing else on the host can enter. The
// container is not held back by it: the daemon resolves a bind's source as root, and the container
// reaches the tree through the bind, whose own modes are the whole of its access.
const TreesDir = ".trees"

const (
	treesMode    os.FileMode = 0o700
	treeDirMode  os.FileMode = 0o755
	treeFileMode os.FileMode = 0o444
	treeExecMode os.FileMode = 0o555
)

// treeFetchers is how many files are fetched at once. A tree is up to 4096 files, one request
// each, and one after the other a task would wait on thousands of round trips before its
// container is created; a handful at once is most of the gain without a runner laying out several
// trees opening hundreds of connections to the store.
const treeFetchers = 8

// newTreeDir creates the directory one assembly of a task lays its tree out in: new, empty, and
// named after the task, so that a person listing the trees can tell whose each is.
//
// It is one directory per assembly and never one per task. A task is assembled again when its
// message comes round to the host that holds it, and when the agent restarts under a container
// that is still running and redeems again for the values its log is masked with. That container
// has the earlier tree bound at /agk/repo, and a tree removed or rewritten in place under it
// would be a step losing its repository halfway through. So each assembly has its own, and takes
// away its own.
func newTreeDir(workRoot string, id agk.TaskID) (string, error) {
	if workRoot == "" {
		return "", errors.New("runner: no work root: a task's tree is laid out under one")
	}
	run, step, attempt, shard, err := agk.ParseTaskID(string(id))
	if err == nil {
		err = id.Validate()
	}
	if err != nil {
		return "", fmt.Errorf("runner: %w", err)
	}
	// Dots between the parts, since neither a run identifier nor a step name can hold one,
	// and the shard as the driver writes it in a directory name.
	name := string(run) + "." + string(step) + "." + strconv.Itoa(attempt)
	if !shard.IsZero() {
		name += "." + strconv.Itoa(shard.Index) + "-" + strconv.Itoa(shard.Of)
	}
	trees := filepath.Join(workRoot, TreesDir)
	if err := os.MkdirAll(trees, treesMode); err != nil {
		return "", fmt.Errorf("runner: the trees directory %s could not be created: %w", trees, err)
	}
	if err := os.Chmod(trees, treesMode); err != nil {
		return "", fmt.Errorf("runner: the trees directory %s: %w", trees, err)
	}
	dir, err := os.MkdirTemp(trees, name+".")
	if err != nil {
		return "", fmt.Errorf("runner: task %s: a directory for its tree could not be created: %w", id, err)
	}
	return dir, nil
}

// The two directories of one assembly's layout. repoDir is the tree under /agk/repo, which the
// driver binds read-only, and placedDir holds one link for each object a long form of the step's
// files places somewhere else, which the driver copies from into the task's working directory,
// with the mode the selector asked for.
const (
	repoDir   = "repo"
	placedDir = "placed"
)

// layOutTree writes the tree a redemption names under dir, which is new and empty, and answers
// the files it places elsewhere for the driver to put there.
//
// Every file is a hard link to the namespace's cache (cache.go), where it was held to its digest
// when it arrived: a file the cache does not hold yet is fetched through objects first, once
// however many paths name it, and one it holds is not fetched at all. An entry with no to goes at
// its path under dir/repo. An entry with a to goes under dir/placed, by its digest, and is
// answered as a driver.Placed. None is answered as nil, which tells the driver there is nothing
// the redemption placed: an API that places nothing leaves the step's files to the driver, as
// agk run --local does.
//
// On any refusal the directory is taken away again, so what is left behind is either a whole,
// checked tree or nothing.
//
// A file above artifact_max_bytes is refused as it arrives, since a tree file is an object of the
// store like any other and the store holds none larger.
func layOutTree(ctx context.Context, cache *treeCache, objects artifact.Objects, namespace, dir string, entries []TreeEntry, l agk.Limits) (placed []driver.Placed, err error) {
	defer func() {
		if err != nil {
			if left := os.RemoveAll(dir); left != nil {
				err = fmt.Errorf("%w, and what was laid out was not all taken away: %v", err, left)
			}
		}
	}()
	repo := filepath.Join(dir, repoDir)
	if err := os.Mkdir(repo, treeDirMode); err != nil {
		return nil, fmt.Errorf("runner: the tree's directory %s: %w", repo, err)
	}
	if err := os.Chmod(repo, treeDirMode); err != nil {
		return nil, fmt.Errorf("runner: the tree's directory %s: %w", repo, err)
	}

	// The directories first, one after the other, so that the links only ever create files. A
	// path another entry needs as a directory is refused by Redemption.answers, and would be
	// refused here by the link, which finds a directory in its place.
	var want []cachedObject
	wanted := map[objectKey]bool{}
	for _, e := range entries {
		if err := treePath(e.Path); err != nil {
			return nil, fmt.Errorf("runner: %w", err)
		}
		mode, err := treeMode(e.Mode)
		if err != nil {
			return nil, fmt.Errorf("runner: the tree gives %s %w", e.Path, err)
		}
		// A placed file is copied by the driver with the mode its entry gives, so the
		// cache's plain file is what it needs.
		key := objectKey{digest: e.SHA256, exec: e.To == "" && mode&0o111 != 0}
		if e.To == "" {
			if err := mkdirTree(repo, path.Dir(e.Path)); err != nil {
				return nil, err
			}
		}
		if !wanted[key] {
			wanted[key] = true
			want = append(want, cachedObject{objectKey: key, path: e.Path})
		}
	}

	at, release, err := cache.hold(ctx, objects, namespace, want, l)
	defer cache.evict()
	defer release()
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if e.To != "" {
			continue
		}
		mode, _ := treeMode(e.Mode)
		from := at[objectKey{digest: e.SHA256, exec: mode&0o111 != 0}]
		if err := os.Link(from, filepath.Join(repo, filepath.FromSlash(e.Path))); err != nil {
			return nil, fmt.Errorf("runner: the tree file %s could not be laid out: %w", e.Path, err)
		}
	}
	for _, e := range entries {
		if e.To == "" {
			continue
		}
		dest := filepath.Join(dir, placedDir, e.SHA256)
		if placed == nil {
			if err := os.Mkdir(filepath.Join(dir, placedDir), treesMode); err != nil {
				return nil, fmt.Errorf("runner: the tree's directory %s: %w", filepath.Join(dir, placedDir), err)
			}
		}
		if err := os.Link(at[objectKey{digest: e.SHA256}], dest); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("runner: the tree file %s could not be laid out: %w", e.Path, err)
		}
		placed = append(placed, driver.Placed{Source: dest, To: e.To, Mode: e.Mode})
	}
	return placed, nil
}

// mkdirTree creates a directory of the tree, rel below dir, and every directory between the two,
// each with the tree's mode. The mode is set on each rather than requested, since the process
// umask would otherwise decide it, and a directory an agent's umask left closed to others is one a
// remapped container cannot enter to reach what is below it.
func mkdirTree(dir, rel string) error {
	if rel == "." {
		return nil
	}
	at := dir
	for _, segment := range strings.Split(rel, "/") {
		at = filepath.Join(at, segment)
		err := os.Mkdir(at, treeDirMode)
		if errors.Is(err, fs.ErrExist) {
			// Made for an earlier entry, with its mode set then; anything else under
			// this name is refused when a file is created beneath it.
			continue
		}
		if err == nil {
			err = os.Chmod(at, treeDirMode)
		}
		if err != nil {
			return fmt.Errorf("runner: the tree's directory %s could not be created: %w", at, err)
		}
	}
	return nil
}
