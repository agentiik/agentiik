package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/agentiik/agentiik/graph"
	versions "github.com/agentiik/agentiik/version"
)

// The one place a working tree is read, so that agk validate, agk graph and agk run --local can
// never disagree about what is valid, and all three judge it by what agk push and a hook judge it
// by.
//
// Two readers would be two answers, and the second one would be found by somebody whose
// validate passed and whose run refused. Everything both commands do to a file before a
// container exists is here.

// entryPoint is the file a workflow is read from when the command line names none.
const entryPoint = "agentiik.yaml"

// load reads the entry point and resolves the graph its edges describe, judged by version.Check,
// which is what agk push, the push route and a pre-receive hook judge a version by.
//
// The tree is the directory holding the entry point, as an fs.FS, which is what pins every
// include and every schema $ref inside it: "a path include resolves inside the same commit,
// so it can never be stale and nothing has to pin it", and a reference that leaves the tree
// is refused by the packages that resolve one rather than by a check here. On a laptop the
// commit is the working tree, unmodified, which is the whole of what agk run --local means by
// pinned. It is not a commit's tree, so the rules a commit's tree is held to are not applied:
// a working copy's .git is the repository itself.
//
// Nothing of the installation is reached. A workflow include is reaching another repository at
// a ref, and there is no server here, so a file that declares one is refused naming the
// repository and the ref it wanted; no image is pinned; and the manifests are read by the
// caller that needs them, since reading one is pulling an image.
func load(ctx context.Context, e Env, entry string) (*graph.Workflow, fs.FS, string, error) {
	fsys, dir, base, err := workingCopy(e, entry)
	if err != nil {
		return nil, nil, "", err
	}
	checked, err := versions.Check(ctx, fsys, versions.Checking{Entry: base})
	if err != nil {
		return nil, nil, "", inTree(err, dir)
	}
	if checked.Library {
		return nil, nil, "", fmt.Errorf("%s is a library's root, written as a fragment, which other workflows include and nothing runs: there is no graph to draw or run, and agk validate judges it", filepath.Join(dir, base))
	}
	return checked.Workflow, fsys, dir, nil
}

// workingCopy is the tree a command reads a workflow out of: the directory holding the entry
// point, and the entry point's name in it.
//
// The entry point is reached for by its whole path before any of that, which is the one thing
// this function adds to what the packages below it say. An error names the step, the exit code
// and what was refused, and where a file is the subject the where is the path: below this line
// the tree is an fs.FS rooted at the directory, so every name inside it is relative to that
// root, which is what pins an include to the commit and what leaves a refusal saying "open
// agentiik.yaml: no such file or directory" with no directory in it. That tells somebody
// standing in the wrong directory, or who mistyped the argument of -f, the one thing they
// already knew.
func workingCopy(e Env, entry string) (fs.FS, string, string, error) {
	if entry == "" {
		entry = entryPoint
	}
	path := e.path(entry)
	dir, base := filepath.Dir(path), filepath.Base(path)

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "", "", fmt.Errorf("there is no workflow at %s: one is read from %s in the directory the command is run in, or from the path -f names", path, entryPoint)
	case err != nil:
		return nil, "", "", fmt.Errorf("the workflow at %s could not be read: %w", path, err)
	case info.IsDir():
		// A directory where a file was expected is the commonest way -f is mistyped,
		// since the tree and the entry point differ by one segment. Read as a file it
		// would be refused as a document rather than as a path.
		return nil, "", "", fmt.Errorf("%s is a directory: -f names the entry point itself, which is %s inside it", path, entryPoint)
	}
	return os.DirFS(dir), dir, base, nil
}

// inTree names the tree a file was looked for in, where what was refused is a file that is not
// there.
//
// That is an include or a schema reference, and the packages that resolve one say which name
// they wanted and that it has to be in the tree the run was pinned to. Which tree that is, is
// this side's half of the sentence: the directory is chosen here, out of -f and the directory
// the command was run in, and nothing below has been told it.
func inTree(err error, dir string) error {
	var r *graph.Refusal
	if errors.As(err, &r) && r.Rule == graph.RuleIncludeMissing {
		r.Detail += ". The tree read was " + dir
		return err
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return fmt.Errorf("%w. The tree read was %s", err, dir)
}
