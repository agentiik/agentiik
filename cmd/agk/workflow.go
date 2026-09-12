package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/agentiik/agentiik/graph"
)

// The one place a workflow is read, so that agk validate and agk run --local can never
// disagree about what is valid.
//
// Two readers would be two answers, and the second one would be found by somebody whose
// validate passed and whose run refused. Everything both commands do to a file before a
// container exists is here.

// entryPoint is the file a workflow is read from when the command line names none.
const entryPoint = "agentiik.yaml"

// load reads the entry point and resolves the graph its edges describe.
//
// The tree is the directory holding the entry point, as an fs.FS, which is what pins every
// include and every schema $ref inside it: "a path include resolves inside the same commit,
// so it can never be stale and nothing has to pin it", and a reference that leaves the tree
// is refused by the packages that resolve one rather than by a check here. On a laptop the
// commit is the working tree, unmodified, which is the whole of what agk run --local means by
// pinned.
//
// The remote map is nil. Resolving a workflow include is reaching another repository at a
// ref, and there is no server here, so a file that declares one is refused naming the
// repository and the ref it wanted. That refusal comes out of graph.Load, which already
// writes it.
//
// Check runs here rather than in each caller, because a workflow that parses is not yet a
// workflow that holds together and the order of two calls is not a rule anybody should have
// to remember.
//
// The entry point is reached for by its whole path before any of that, which is the one thing
// this function adds to what the packages below it say. An error names the step, the exit code
// and what was refused, and where a file is the subject the where is the path: below this line
// the tree is an fs.FS rooted at the directory, so every name inside it is relative to that
// root, which is what pins an include to the commit and what leaves a refusal saying "open
// agentiik.yaml: no such file or directory" with no directory in it. That tells somebody
// standing in the wrong directory, or who mistyped the argument of -f, the one thing they
// already knew.
func load(e Env, entry string) (*graph.Workflow, fs.FS, string, error) {
	if entry == "" {
		entry = entryPoint
	}
	path := e.path(entry)
	dir, base := filepath.Dir(path), filepath.Base(path)

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil, "", fmt.Errorf("there is no workflow at %s: one is read from %s in the directory the command is run in, or from the path -f names", path, entryPoint)
	case err != nil:
		return nil, nil, "", fmt.Errorf("the workflow at %s could not be read: %w", path, err)
	case info.IsDir():
		// A directory where a file was expected is the commonest way -f is mistyped,
		// since the tree and the entry point differ by one segment. Read as a file it
		// would be refused as a document rather than as a path.
		return nil, nil, "", fmt.Errorf("%s is a directory: -f names the entry point itself, which is %s inside it", path, entryPoint)
	}

	fsys := os.DirFS(dir)
	wf, err := graph.Load(fsys, base, nil)
	if err != nil {
		return nil, nil, "", inTree(err, dir)
	}
	if err := graph.Check(wf); err != nil {
		return nil, nil, "", err
	}
	return wf, fsys, dir, nil
}

// inTree names the tree a file was looked for in, where what was refused is a file that is not
// there.
//
// That is an include or a schema reference, and the packages that resolve one say which name
// they wanted and that it has to be in the tree the run was pinned to. Which tree that is, is
// this side's half of the sentence: the directory is chosen here, out of -f and the directory
// the command was run in, and nothing below has been told it.
func inTree(err error, dir string) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return fmt.Errorf("%w. The tree read was %s", err, dir)
}
