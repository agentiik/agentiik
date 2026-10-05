package version

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"testing/fstest"
	"unicode/utf8"

	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/repo"
)

// The rules a version is refused by beyond the language's own, each spelled as the repository
// fixtures name it, so that a hook writing one on git's error stream and a fixture pinning it
// read the same word. They are about the tree a commit holds, about the installation a push is
// made to and about the name it is made under, none of which one document can show.
const (
	// RuleEntryPointMissing is a tree holding no agentiik.yaml at all. "The entry point is
	// always at the root", and agentiik.yml is another file.
	RuleEntryPointMissing graph.Rule = "entry-point-missing"
	// RuleEntryPointBelowRoot is a tree whose only agentiik.yaml is in a directory. A workflow
	// kept in a directory of a larger repository is pushed as a repository of its own, which
	// git subtree split makes.
	RuleEntryPointBelowRoot graph.Rule = "entry-point-below-root"
	// RuleSymlinkInTree is a symbolic link: "its target is resolved on whichever host lays
	// the tree out, and can point outside the repository".
	RuleSymlinkInTree graph.Rule = "symlink-in-tree"
	// RuleSubmoduleInTree is a submodule: "it is another repository, and the runner holds no
	// credential to fetch it".
	RuleSubmoduleInTree graph.Rule = "submodule-in-tree"
	// RuleNameNotUTF8 is a name that is not UTF-8: "a name travels as JSON text, where it
	// would arrive as another name".
	RuleNameNotUTF8 graph.Rule = "name-not-utf-8"
	// RuleDotGitInTree is a name that is .git on some filesystem a tree is laid out on, which
	// under /agk/repo would be a repository configuration, hooks and all, that any git a step
	// runs there obeys. Named here first: no fixture pins it yet.
	RuleDotGitInTree graph.Rule = "dot-git-in-tree"
	// RuleBackslashInTree is a name holding a backslash, which Windows reads as a separator,
	// so that the name is one file on one host and another on the next. Named here first.
	RuleBackslashInTree graph.Rule = "backslash-in-tree"
	// RuleTreePathTooLong is a path past TreePathMaxBytes, and RuleTreeNameTooLong one segment
	// of it past TreeNameMaxBytes: a file no runner could create. Named here first.
	RuleTreePathTooLong graph.Rule = "tree-path-too-long"
	RuleTreeNameTooLong graph.Rule = "tree-name-too-long"
	// RuleMetadataNameNotRepository is a metadata.name other than the repository's: "the name
	// is the identity runs, grants and the git remote are addressed by, and the repository
	// already holds one".
	RuleMetadataNameNotRepository graph.Rule = "metadata-name-not-repository"
	// RuleMetadataNamespaceNotRepository is a metadata.namespace, where the file writes one,
	// other than the namespace the repository belongs to: "a workflow reaches the secrets, the
	// quotas and the runner pools of the namespace that owns it", and the file cannot move it.
	RuleMetadataNamespaceNotRepository graph.Rule = "metadata-namespace-not-repository"
	// RuleImageNotPinned is a step naming an image by a tag the namespace's store holds no
	// digest for. A server runs every image by the digest its tag was pinned to, and reaches no
	// registry to resolve one.
	RuleImageNotPinned graph.Rule = "image-not-pinned"
	// RuleSecretNotDeclaredByNamespace is a secret the workflow names that its namespace does
	// not declare, refused although no step mounts it: "a workflow can name only the secrets its
	// own namespace declares, so that moving it elsewhere breaks the reference rather than
	// carrying access along". graph.RuleSecretNotDeclared is another rule, a step mounting a
	// secret the secrets block does not name.
	RuleSecretNotDeclaredByNamespace graph.Rule = "secret-not-declared-by-namespace"
)

// EntryPoint is the file a workflow repository is read from: "agentiik.yaml at the root is the
// entry point", spelled so.
const EntryPoint = "agentiik.yaml"

// TreeNameMaxBytes is the longest one segment of a tree path may be, which is NAME_MAX: 255 bytes is
// what the filesystems a runner lays a tree out on hold a name to. A longer name is a file no
// runner can create, and a version holding one is a version every run of which fails, so it is
// refused at the push rather than at each of them.
const TreeNameMaxBytes = 255

// TreePathMaxBytes is the longest a tree path may be.
//
// Linux holds a path to PATH_MAX, 4096 bytes with the null that ends it, and a tree is laid out
// below a directory twice over: the runner's own on the host, and /agk/repo in the container,
// where a step opens it by name. Half of PATH_MAX leaves the other half to whichever directory
// that is, and a workflow repository whose paths need more is not one anybody writes by hand.
const TreePathMaxBytes = 2048

// TreePath refuses a path a container could not be given, and one that leaves the tree.
//
// The mount is /agk/repo, so a path escaping it is a path writing somewhere else on the host that
// prepares the directory. It is refused where a version is made because that is where somebody is
// watching. Exported because agk push applies it to every name of a commit before it reads a byte
// of the tree, and the push route to every name a push carries, so that a name a hook would refuse
// is refused by all three and by one rule rather than by three copies of it.
//
// What a tree holds is refused by a rule, placed at the path: a name that is not UTF-8, a
// backslash, a name that is .git somewhere, a path or a name past its bound. What cannot be a path
// of any tree, one that is empty, absolute, not in its cleaned form, leaving the root or carrying a
// null byte, is refused by an error of its own, since only a transport that writes paths as text
// can carry one.
func TreePath(p string) error {
	switch {
	case p == "":
		return errors.New("a tree file with no path")
	case len(p) > TreePathMaxBytes:
		// First, so that nothing below prints a path of mebibytes, and placed at no file: the
		// path is what is refused, and the sentence names the start of it, which is as much
		// as anybody reads.
		return inTree(RuleTreePathTooLong, "", fmt.Sprintf("%.64s... is a path of %d bytes, and a tree path is at most %d: laid out below a runner's directory and below /agk/repo it would be a path the host cannot name", p, len(p), TreePathMaxBytes))
	case p == ".":
		return errors.New("a tree file named ., which is the root of the repository and a directory rather than a file")
	case path.IsAbs(p):
		return fmt.Errorf("%s is absolute, and a tree path is relative to the root of the repository", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%q carries a null byte", p)
	case !utf8.ValidString(p):
		// Git names a file with any bytes, and JSON carries UTF-8 only: a name that is not
		// arrives with U+FFFD where its bytes were, so the file would be laid out under a
		// name the commit does not give it, and two such names would arrive as one.
		return inTree(RuleNameNotUTF8, p, fmt.Sprintf("%q is not a UTF-8 name, and a name travels as JSON text, where it would arrive as another name", p))
	case path.Clean(p) != p:
		return fmt.Errorf("%s is not in its cleaned form", p)
	case p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("%s leaves the repository", p)
	case strings.ContainsRune(p, '\\'):
		// A separator on Windows, where a\..\..\x is a path out of the tree and C:\x
		// and \\host\share are somewhere else entirely. A path that names one file on
		// one host and another on the next is not a path a version can promise.
		return inTree(RuleBackslashInTree, p, fmt.Sprintf("%q holds a backslash, which Windows reads as a separator: a tree path separates its directories with / alone, so that it names the same file on every host that lays it out", p))
	}
	for _, segment := range strings.Split(p, "/") {
		if len(segment) > TreeNameMaxBytes {
			return inTree(RuleTreeNameTooLong, p, fmt.Sprintf("%.64s... holds a name of %d bytes, and a filesystem holds a name to %d: no runner could create that file", p, len(segment), TreeNameMaxBytes))
		}
		// A segment that is .git, which a commit's tree never holds since git refuses it,
		// and which laid out under /agk/repo would be a repository configuration, hooks
		// and all, that any git a step runs there obeys. The rule is git's own, held in
		// package repo, which reads the trees a push sends.
		if repo.DotGit(segment) {
			return inTree(RuleDotGitInTree, p, fmt.Sprintf("%q has a segment that is .git on some filesystem a tree is laid out on, and .git is git's own and never part of a commit's tree", p))
		}
	}
	return nil
}

// TreeEntry refuses one entry of a tree by its path and its kind, as fs reports it: a
// symbolic link, and a submodule, which a tree read out of git reports as irregular since it is
// neither a file nor a directory of this repository. Every other rule is TreePath's.
//
// agk push applies it to every entry git lists before any file is read, and Check to every
// entry of the tree it is given, so that the command line and a hook refuse one tree by one rule.
func TreeEntry(name string, mode fs.FileMode) error {
	if err := TreePath(name); err != nil {
		return err
	}
	return treeKind(name, mode)
}

// treeKind is TreeEntry's refusal of an entry by its kind, of a path TreePath accepts.
func treeKind(name string, mode fs.FileMode) error {
	switch {
	case mode&fs.ModeSymlink != 0:
		return inTree(RuleSymlinkInTree, name, fmt.Sprintf("%s is a symbolic link, and a tree carries none: its target would be resolved on whatever host lays the tree out, where it could point outside the repository. Commit the file it points to in its place", name))
	case mode&fs.ModeIrregular != 0:
		return inTree(RuleSubmoduleInTree, name, fmt.Sprintf("%s is a submodule, and a tree carries none: it is another repository, which a runner would need a credential to fetch, and a runner holds none. Commit its files into this repository, or put them in an image", name))
	case mode.Type() != 0 && !mode.IsDir():
		return fmt.Errorf("%s is neither a file nor a directory, and a tree carries files", name)
	}
	return nil
}

// inTree is a refusal about one entry of the tree, placed at its path.
func inTree(rule graph.Rule, name, detail string) *graph.Refusal {
	return &graph.Refusal{Rule: rule, Detail: detail, At: graph.Position{File: name}}
}

// Files is a tree held as its files, by path, as a push carries one and as agk push reads one out
// of git.
//
// Check judges it from its listing, path by path, rather than by walking it. An fstest.MapFS finds
// a directory's entries by reading every path it holds, so a walk costs the files times the
// directories, and a push of 512 empty files 500 directories deep, half a mebibyte of paths, took
// five seconds of processor from anybody allowed to push. A path is held to TreePath segment by
// segment, which is every directory it names, so the listing refuses what the walk did.
type Files map[string]*fstest.MapFile

// Open opens a file of the tree, or a directory, as an fstest.MapFS does.
func (f Files) Open(name string) (fs.File, error) { return fstest.MapFS(f).Open(name) }

// ReadFile reads one file without the directory scan an Open of a path it does not hold starts.
func (f Files) ReadFile(name string) ([]byte, error) { return fstest.MapFS(f).ReadFile(name) }

// checkTree holds every entry of a tree to TreeEntry: in byte order for Files, and in the order a
// walk lists them otherwise.
func checkTree(tree fs.FS) error {
	if files, ok := tree.(Files); ok {
		for _, name := range slices.Sorted(maps.Keys(files)) {
			if err := TreeEntry(name, files[name].Mode.Type()); err != nil {
				return err
			}
		}
		return nil
	}
	return walkTree(tree, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		// A walk holds a directory to TreeEntry before it lists it, and stops at the first
		// refused, so a path below one adds its name and its length and nothing else to
		// check. A path failing either is held to the whole rule, which words the refusal as
		// ever; holding every path to it whole cost a walk of a deep tree the square of its
		// depth.
		if dir, base := path.Split(name); dir == "" || len(name) > TreePathMaxBytes || !plainName(base) {
			if err := TreePath(name); err != nil {
				return err
			}
		}
		return treeKind(name, d.Type())
	})
}

// plainName is whether TreePath accepts a path ending with name below a directory it accepts:
// nothing in the name is refused, and nothing in it makes the path unclean.
func plainName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= TreeNameMaxBytes && utf8.ValidString(name) && !strings.ContainsAny(name, "/\\\x00") && !repo.DotGit(name)
}

// TreeMaxEntries is the most entries, files and directories together, a walk of a tree visits
// before the tree is refused: 65,536. A version holds at most 4,096 files, the bound a pushed tree
// is held to, and sixteen levels of directory above each of them is more than anybody nests.
//
// Counted by path rather than by tree object, since a walk visits paths: git lets one tree name
// another any number of times, so eleven trees of a thousand entries each, a few kilobytes of a
// push, list 10^33 paths, and a walk that did not count them would hold whoever judges the push
// for ever.
const TreeMaxEntries = 1 << 16

// ErrTooManyEntries is a tree listing more than TreeMaxEntries paths.
var ErrTooManyEntries = fmt.Errorf("the tree lists more than %d files and directories, counted by path, where a version holds at most 4,096 files", TreeMaxEntries)

// walkTree walks a tree as fs.WalkDir does, and refuses one listing more than TreeMaxEntries paths.
func walkTree(tree fs.FS, fn fs.WalkDirFunc) error {
	visited := 0
	return fs.WalkDir(tree, ".", func(name string, d fs.DirEntry, err error) error {
		if name != "." {
			if visited++; visited > TreeMaxEntries {
				return ErrTooManyEntries
			}
		}
		return fn(name, d, err)
	})
}

// entryPoint is where a repository's entry point is, refusing a tree that holds none at the
// root: one whose only agentiik.yaml is in a directory is refused naming the file found there,
// the first in byte order, and the command that makes it a repository of its own.
func entryPoint(tree fs.FS) error {
	if info, err := fs.Stat(tree, EntryPoint); err == nil && !info.IsDir() {
		return nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", EntryPoint, err)
	}
	var below []string
	if files, ok := tree.(Files); ok {
		for name, f := range files {
			if !f.Mode.IsDir() && name != EntryPoint && path.Base(name) == EntryPoint {
				below = append(below, name)
			}
		}
	} else if err := walkTree(tree, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && name != EntryPoint && path.Base(name) == EntryPoint {
			below = append(below, name)
		}
		return nil
	}); err != nil {
		return err
	}
	if len(below) == 0 {
		return inTree(RuleEntryPointMissing, EntryPoint, fmt.Sprintf("the tree holds no %s: the entry point is always at the root, spelled so, and agentiik.yml is another file", EntryPoint))
	}
	slices.Sort(below)
	return EntryPointBelowRoot(below[0])
}

// EntryPointBelowRoot is the refusal of an entry point at name, in a directory rather than at the
// root, naming the command that makes the directory a repository of its own. Exported because agk
// push refuses a -f naming one before it reads a byte of the tree, in the same words.
func EntryPointBelowRoot(name string) error {
	dir := path.Dir(name)
	return inTree(RuleEntryPointBelowRoot, name, fmt.Sprintf("the entry point is in %s/ rather than at the root of the repository, where it always is: a workflow kept in a directory of a larger repository is pushed as a repository of its own, which git subtree split --prefix %s makes, and a version pushed from a directory before v0.4.0 stays runnable", dir, dir))
}
