package graph

import (
	"fmt"
	"path"
	"strings"
)

// What a step's files select of the repository tree, and where each file it selects goes.
//
// "A path or glob relative to the root, or { from, to, mode } to relocate one. A path names a
// file, or a directory with everything under it. In a glob * matches within one path segment, ?
// one character, [abc] one of a set or a range, and ** written as a segment of its own zero or
// more whole segments." The selection is made where the tree is known, by the API for a task it
// answers the files of and by agk run --local over the working tree, and the runner evaluates no
// path rule: what it is handed is each file with the place it goes.

// Placed is one file a step's files select: its path in the tree, where it goes when a selector
// relocates it, absolute, and the mode a selector gives it, empty where none does.
type Placed struct {
	Path string
	To   string
	Mode string
}

// Place answers the files of a tree that selectors select, each once for every place it goes, in
// the order of the tree and then of the selectors. No selector is the whole tree, where every
// file stays where it is, since narrowing is "an optimisation for large repositories, never a
// requirement". A file two selectors put in one place is placed there once, with the mode of the
// last that gives one, and two files one place is refused.
func Place(selectors []FileSelector, tree []string) ([]Placed, error) {
	if len(selectors) == 0 {
		placed := make([]Placed, 0, len(tree))
		for _, p := range tree {
			placed = append(placed, Placed{Path: p})
		}
		return placed, nil
	}
	compiled := make([]fileMatch, 0, len(selectors))
	for _, s := range selectors {
		c, err := compileSelector(s)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, c)
	}
	var placed []Placed
	bound := map[string]int{}
	for _, p := range tree {
		inPlace := -1
		for _, s := range compiled {
			below, ok := s.match(p)
			if !ok {
				continue
			}
			if s.to == "" {
				if inPlace < 0 {
					inPlace = len(placed)
					placed = append(placed, Placed{Path: p})
				}
				if s.mode != "" {
					placed[inPlace].Mode = s.mode
				}
				continue
			}
			to := s.to
			if below != "" {
				to = path.Join(s.to, below)
			}
			switch took, held := bound[to]; {
			case !held:
				bound[to] = len(placed)
				placed = append(placed, Placed{Path: p, To: to, Mode: s.mode})
			case placed[took].Path != p:
				// Two files at one place would be one of them silently missing, whichever
				// was bound last.
				return nil, fmt.Errorf("files: %s and %s are both relocated to %s, and one place holds one file", placed[took].Path, p, to)
			case s.mode != "":
				placed[took].Mode = s.mode
			}
		}
	}
	return placed, nil
}

// fileMatch is a FileSelector made ready to match: its path in segments, and where the part a
// relocation keeps starts.
type fileMatch struct {
	segments []string
	glob     bool
	// fixed is how many segments come before the first holding a wildcard, what a relocated
	// glob's files keep their path below.
	fixed    int
	to, mode string
}

// compileSelector reads one selector's path the way the tree writes its own: relative to the
// root, ./ and a leading / both read as the root itself, and cleaned.
func compileSelector(s FileSelector) (fileMatch, error) {
	rel, err := selectorPath(s.From)
	if err != nil {
		return fileMatch{}, err
	}
	c := fileMatch{to: s.To, mode: s.Mode}
	if s.To != "" {
		c.to = path.Clean(s.To)
	}
	if rel == "." {
		c.segments = nil
	} else {
		c.segments = strings.Split(rel, "/")
	}
	c.fixed = len(c.segments)
	for i, seg := range c.segments {
		if seg == "**" || strings.ContainsAny(seg, "*?[") {
			c.glob = true
			c.fixed = i
			break
		}
	}
	// Each segment is checked once here, so that a malformed set is refused rather than read
	// as matching nothing.
	for _, seg := range c.segments {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fileMatch{}, fmt.Errorf("files: %q is not a glob a tree can be matched against: %w", s.From, err)
		}
	}
	return c, nil
}

// selectorPath is a selector's path relative to the root, refused where it leaves the tree.
func selectorPath(from string) (string, error) {
	rel := strings.TrimPrefix(from, "/")
	rel = path.Clean(strings.TrimPrefix(rel, "./"))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("files: %q leaves the repository tree: a selector names a path relative to the root of the workflow repository", from)
	}
	return rel, nil
}

// match says whether the file at p is selected, and the path it keeps below what the selector
// names when it is relocated: below the directory a path names, below a glob's fixed segments,
// and nothing for the file a path names itself.
func (s fileMatch) match(p string) (string, bool) {
	file := strings.Split(p, "/")
	if !s.glob {
		switch {
		case len(s.segments) == 0:
			return p, true
		case len(file) < len(s.segments):
			return "", false
		}
		for i, seg := range s.segments {
			if file[i] != seg {
				return "", false
			}
		}
		return strings.Join(file[len(s.segments):], "/"), true
	}
	if !matchSegments(s.segments, file) {
		return "", false
	}
	return strings.Join(file[s.fixed:], "/"), true
}

// matchSegments matches a glob's segments against a path's, ** matching zero or more whole
// segments and every other segment one, as path.Match reads it.
func matchSegments(glob, file []string) bool {
	for len(glob) > 0 {
		if glob[0] == "**" {
			rest := glob[1:]
			for skip := 0; skip <= len(file); skip++ {
				if matchSegments(rest, file[skip:]) {
					return true
				}
			}
			return false
		}
		if len(file) == 0 {
			return false
		}
		if ok, _ := path.Match(glob[0], file[0]); !ok {
			return false
		}
		glob, file = glob[1:], file[1:]
	}
	return len(file) == 0
}
