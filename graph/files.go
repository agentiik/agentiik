package graph

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/brick"
)

// What a step's files select from the repository tree.
//
// "files narrows the repository tree, which it otherwise gets whole under /agk/repo/: a path or
// glob relative to the root, or { from, to, mode } to relocate one." It is read in two places
// that have to agree. The API reads it when a runner redeems a narrowed step's grant, and answers
// with the files the step asked for and no others, so that a runner fetches only those. The
// driver reads it on a laptop, where agk run --local binds the working tree whole and places
// what a long form relocates itself. Both call SelectFiles, so a glob selects the same files on a
// server and on a laptop.
//
// The grammar is the documentation's: "In a glob * matches within one path segment, ? one
// character, [abc] one of a set or a range, and ** written as a segment of its own zero or more
// whole segments." A glob is matched against the path of every file of the tree, segment by
// segment, and selects files and never a directory: a directory is selected by naming it.

// Placement is one file a long form puts somewhere of its own.
//
// Path is where the file is in the tree and To where the container finds it, an absolute path.
// Mode is the mode the selector asked for, as it wrote it, and empty where it asked for none, in
// which case the file keeps its own.
type Placement struct {
	Path string
	To   string
	Mode string
}

// Selection is what a step's files select from a tree: the paths laid out where they are under
// /agk/repo, and the files placed somewhere of their own.
type Selection struct {
	Tree   []string
	Placed []Placement
}

// SelectFiles is what files select from a tree whose files are at paths, each relative to its
// root and written as it cleans to.
//
// With no selector the tree is given whole, since narrowing "is an optimisation for large
// repositories, never a requirement, and never a permission boundary". Otherwise a selector with
// no to leaves what it selects where it is under /agk/repo, and one with a to relocates it
// there instead: "a directory or a glob relocated there keeps each file's path below the
// directory, or below the glob's segments before its first wildcard, and mode applies to every
// file it places". A selector that asks for a mode and relocates nothing leaves its files where
// they are and places each over itself, under /agk/repo, with that mode.
//
// It refuses nothing: a selector that CheckFiles would refuse selects nothing here, since the
// API answers a runner from what it holds rather than judging the workflow, and the driver holds
// the task to CheckFiles before anything is placed.
func SelectFiles(files []FileSelector, paths []string) Selection {
	if len(files) == 0 {
		tree := slices.Clone(paths)
		slices.Sort(tree)
		return Selection{Tree: slices.Compact(tree)}
	}
	inTree := map[string]bool{}
	placed := map[Placement]bool{}
	for _, f := range files {
		if checkFile(f) != nil {
			continue
		}
		from, _ := treeRelative(f.From)
		base, isGlob := baseOf(from)
		for _, p := range paths {
			var under bool
			switch {
			case isGlob:
				under = globMatch(from, p)
			default:
				under = p == from || from == "." || strings.HasPrefix(p, from+"/")
			}
			if !under {
				continue
			}
			switch {
			case f.To != "":
				to := path.Clean(f.To)
				// A path naming a file takes to as its own path; a directory or a
				// glob keeps what is below it.
				if isGlob || p != from {
					to = path.Join(to, below(base, p))
				}
				placed[Placement{Path: p, To: to, Mode: f.Mode}] = true
			case f.Mode != "":
				inTree[p] = true
				placed[Placement{Path: p, To: brick.RepoDir + "/" + p, Mode: f.Mode}] = true
			default:
				inTree[p] = true
			}
		}
	}
	var s Selection
	for p := range inTree {
		s.Tree = append(s.Tree, p)
	}
	slices.Sort(s.Tree)
	for p := range placed {
		s.Placed = append(s.Placed, p)
	}
	slices.SortFunc(s.Placed, func(a, b Placement) int {
		if c := strings.Compare(a.To, b.To); c != 0 {
			return c
		}
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Mode, b.Mode)
	})
	return s
}

// CheckFiles refuses a selector no tree could answer: one that leaves the repository, relocates to
// a path that is not absolute or to the root of the container, or asks for a mode that is not
// three octal digits.
//
// Every one of these is the workflow's, and none depends on what the tree holds, so it is said the
// same way whichever tree the step runs against.
func CheckFiles(files []FileSelector) error {
	for i, f := range files {
		if err := checkFile(f); err != nil {
			return fmt.Errorf("files[%d] %w", i, err)
		}
	}
	return nil
}

func checkFile(f FileSelector) error {
	if _, err := treeRelative(f.From); err != nil {
		return err
	}
	switch to := path.Clean(f.To); {
	case f.To == "":
	case !path.IsAbs(f.To):
		return fmt.Errorf("relocates %q to %q, which is not absolute: the long form of a selector names where in the container a tool insists on finding the file", f.From, f.To)
	case to == "/":
		return fmt.Errorf("relocates %q to the root of the container, where nothing can be placed: to names the file, or the directory what it selects goes under", f.From)
	case strings.ContainsRune(f.To, 0):
		return fmt.Errorf("relocates %q to a path carrying a NUL", f.From)
	}
	if f.Mode != "" && !fileMode.MatchString(f.Mode) {
		return fmt.Errorf("gives %q the mode %q: a mode is three octal digits, written as a string", f.From, f.Mode)
	}
	return nil
}

// Base is the directory, relative to the root of the tree, that every file the selector may select
// sits under: the path itself for a path, a file's or a directory's, and for a glob its segments
// before the first one holding a wildcard, "." where the first one does. It is what a walk of a
// tree too large to list whole starts from.
func (f FileSelector) Base() (string, error) {
	from, err := treeRelative(f.From)
	if err != nil {
		return "", err
	}
	base, _ := baseOf(from)
	return base, nil
}

// treeRelative reads a selector's from as a path relative to the root of the tree, written as it
// cleans to, "." for the root itself. A leading ./ or / says the root, as the documentation's own
// ./sql/** does, and a path that climbs out of the tree is refused.
func treeRelative(from string) (string, error) {
	if from == "" {
		return "", fmt.Errorf("names no path: a selector names a path or a glob relative to the root of the workflow repository")
	}
	if strings.ContainsRune(from, 0) {
		return "", fmt.Errorf("names %q, which carries a NUL", from)
	}
	p := path.Clean(from)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("names %q, which leaves the repository tree: a selector names a path relative to the root of the workflow repository", from)
	}
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		p = "."
	}
	return p, nil
}

// isWild says whether one segment holds a wildcard.
//
// A segment that holds one and does not read as a glob, a [ that no ] closes, is read as the name
// it spells instead. Files were narrowed by nothing on a server before v0.4.0, so a version
// recorded then may name such a path, and it selects that path rather than refusing a step that
// ran before.
func isWild(segment string) bool {
	if !strings.ContainsAny(segment, "*?[") {
		return false
	}
	_, err := path.Match(segment, "")
	return err == nil
}

// baseOf is a from's fixed prefix, and whether it is a glob at all.
func baseOf(from string) (string, bool) {
	segments := strings.Split(from, "/")
	for i, s := range segments {
		if isWild(s) {
			if i == 0 {
				return ".", true
			}
			return strings.Join(segments[:i], "/"), true
		}
	}
	return from, false
}

// below is p's path under base, which p is at or below.
func below(base, p string) string {
	switch {
	case base == ".":
		return p
	case p == base:
		return ""
	}
	return strings.TrimPrefix(p, base+"/")
}

// segmentMatch says whether one segment of a glob matches one segment of a path.
func segmentMatch(glob, name string) bool {
	if !isWild(glob) {
		return glob == name
	}
	ok, _ := path.Match(glob, name)
	return ok
}

// globMatch says whether a glob selects the file at p, segment by segment.
//
// ** is held to "a segment of its own", and written inside one it is two stars, each matching
// within that segment. The match is a table of which segments of the glob can meet which of the
// path's rather than a recursion, so that a glob of many ** against a deep tree costs the product
// of their lengths and never more: a runner's redemption reads it once per file of the tree, and a
// workflow should not be able to make that exponential.
func globMatch(glob, p string) bool {
	pat := strings.Split(glob, "/")
	name := strings.Split(p, "/")
	// meets[i][j]: pat[i:] matches name[j:].
	meets := make([][]bool, len(pat)+1)
	for i := range meets {
		meets[i] = make([]bool, len(name)+1)
	}
	meets[len(pat)][len(name)] = true
	for i := len(pat) - 1; i >= 0; i-- {
		for j := len(name); j >= 0; j-- {
			if pat[i] == "**" {
				// Zero segments, or one and then as many as follow.
				meets[i][j] = meets[i+1][j] || j < len(name) && meets[i][j+1]
				continue
			}
			if j == len(name) {
				continue
			}
			if segmentMatch(pat[i], name[j]) {
				meets[i][j] = meets[i+1][j+1]
			}
		}
	}
	return meets[0][0]
}
