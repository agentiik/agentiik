package repo

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
)

// Mode is what a tree entry is, as git writes it in octal.
type Mode uint32

// The five modes git writes. Git once wrote 100664 as well, which git fsck reports as
// badFilemode and lets by, and git hash-object refuses, as this package does.
const (
	ModeTree       Mode = 0o40000
	ModeFile       Mode = 0o100644
	ModeExecutable Mode = 0o100755
	ModeSymlink    Mode = 0o120000
	// ModeSubmodule is a gitlink: a commit of another repository, which this one does not hold.
	ModeSubmodule Mode = 0o160000
)

// String is the mode as a tree writes it, with no leading zero.
func (m Mode) String() string { return strconv.FormatUint(uint64(m), 8) }

// IsRegular is whether the entry is a file, executable or not.
func (m Mode) IsRegular() bool { return m == ModeFile || m == ModeExecutable }

func (m Mode) valid() bool {
	switch m {
	case ModeTree, ModeFile, ModeExecutable, ModeSymlink, ModeSubmodule:
		return true
	}
	return false
}

// TreeEntry is one name in a tree.
type TreeEntry struct {
	Name string
	Mode Mode
	ID   ID
}

// MaxNameBytes is the longest name one tree entry may carry, in bytes. It is git fsck's own
// bound, fsck.largePathname at its default, past which --strict refuses the tree: no filesystem
// holds such a name, so no clone could check it out.
const MaxNameBytes = 4096

// ParseTree reads a tree object and refuses one git fsck --strict refuses: an entry it cannot
// read, a mode other than the five, a name that is empty, holds a slash, is . or .., is .git on
// some filesystem or is longer than MaxNameBytes, .gitmodules that is not a file, an entry naming
// no object, and entries out of git's order or twice.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	err := eachEntry(data, func(e TreeEntry) { entries = append(entries, e) })
	return entries, err
}

// checkTree holds a tree to what ParseTree does, keeping none of its entries: a pushed tree is
// checked without being held twice.
func checkTree(data []byte) error { return eachEntry(data, nil) }

// eachEntry reads a tree's entries in order, holding each to git fsck --strict, and hands each to
// fn where there is one. What it keeps as it goes is the entry before and the names a directory
// coming later could repeat, never the entries themselves.
func eachEntry(data []byte, fn func(TreeEntry)) error {
	var prev TreeEntry
	// files are the names of files a directory of the same name may yet follow, since a directory
	// sorts as if its name ended with a slash: a file a, then a-b, then a directory a. Each is a
	// prefix of the next, and one is let go once a name shows no directory of it can follow.
	var files []string
	for first := true; len(data) > 0; first = false {
		space := bytes.IndexByte(data, ' ')
		if space < 0 {
			return refuse(TypeTree, checkBadTree, "an entry with no space after its mode")
		}
		mode, err := parseMode(data[:space])
		if err != nil {
			return err
		}
		data = data[space+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 {
			return refuse(TypeTree, checkBadTree, "an entry whose name does not end")
		}
		name := string(data[:nul])
		data = data[nul+1:]
		if len(data) < len(ID{}) {
			return refuse(TypeTree, checkBadTree, "the entry %q ends before its object ID", name)
		}
		e := TreeEntry{Name: name, Mode: mode, ID: ID(data[:len(ID{})])}
		data = data[len(ID{}):]
		if err := checkEntry(e); err != nil {
			return err
		}
		if !first {
			switch c := compareEntries(prev, e); {
			case c == 0:
				return refuse(TypeTree, checkDuplicateEntries, "the name %q twice", name)
			case c > 0:
				return refuse(TypeTree, checkTreeNotSorted, "%q after %q, which is not git's order", name, prev.Name)
			}
		}
		for len(files) > 0 && !directoryMayFollow(files[len(files)-1], name) {
			files = files[:len(files)-1]
		}
		switch {
		case e.Mode == ModeTree && len(files) > 0 && files[len(files)-1] == name:
			return refuse(TypeTree, checkDuplicateEntries, "the name %q twice", name)
		case e.Mode != ModeTree:
			files = append(files, name)
		}
		if fn != nil {
			fn(e)
		}
		prev = e
	}
	return nil
}

// directoryMayFollow is whether a directory named file may still come after the entry name in a
// tree in git's order: name is file itself, or file followed by a byte that sorts before a slash.
func directoryMayFollow(file, name string) bool {
	return strings.HasPrefix(name, file) && (len(name) == len(file) || name[len(file)] < '/')
}

// parseMode reads a mode as a tree writes it: octal, one of the five, and with no leading zero,
// since a zero-padded mode is a second spelling of an entry, and so a second ID for one tree.
func parseMode(b []byte) (Mode, error) {
	if len(b) == 0 {
		return 0, refuse(TypeTree, checkBadTree, "an entry with no mode")
	}
	var m uint64
	for _, c := range b {
		if c < '0' || c > '7' {
			return 0, refuse(TypeTree, checkBadTree, "the mode %q, which is not octal", b)
		}
		if m = m<<3 | uint64(c-'0'); m > 0o177777 {
			return 0, refuse(TypeTree, checkBadFilemode, "the mode %q, which is none of git's", b)
		}
	}
	if b[0] == '0' {
		return 0, refuse(TypeTree, checkZeroPaddedFilemode, "the mode %q, written with a leading zero", b)
	}
	if !Mode(m).valid() {
		return 0, refuse(TypeTree, checkBadFilemode, "the mode %s, which is none of 40000, 100644, 100755, 120000 and 160000", b)
	}
	return Mode(m), nil
}

// checkEntry holds one entry to what git fsck --strict holds it to on its own.
func checkEntry(e TreeEntry) *ObjectError {
	switch {
	case e.Name == "":
		// git fsck has emptyName, and never reaches it: git's own reading of a tree fails first.
		return refuse(TypeTree, checkBadTree, "an entry with an empty name")
	case len(e.Name) > MaxNameBytes:
		return refuse(TypeTree, checkLargePathname, "a name of %d bytes, and a name is at most %d", len(e.Name), MaxNameBytes)
	case strings.Contains(e.Name, "/"):
		return refuse(TypeTree, checkFullPathname, "the name %q, which holds a slash", e.Name)
	case e.Name == ".":
		return refuse(TypeTree, checkHasDot, "an entry named .")
	case e.Name == "..":
		return refuse(TypeTree, checkHasDotdot, "an entry named ..")
	case DotGit(e.Name):
		return refuse(TypeTree, checkHasDotgit, "the name %q, which is .git on some filesystem, and .git is git's own and never part of a tree", e.Name)
	case e.ID.IsZero():
		return refuse(TypeTree, checkNullSha1, "the entry %q, naming the object 0000000000000000000000000000000000000000", e.Name)
	}
	if dotGitmodules(e.Name) {
		switch {
		case e.Mode == ModeSymlink:
			return refuse(TypeTree, checkGitmodulesSymlink, "the name %q, which is .gitmodules on some filesystem, as a symbolic link: a clone reads .gitmodules to decide where submodules go, and would read it wherever the link points", e.Name)
		case !e.Mode.IsRegular():
			return refuse(TypeTree, checkGitmodulesBlob, "the name %q, which is .gitmodules on some filesystem, as a %s rather than a file", e.Name, e.Mode.kind())
		}
	}
	return nil
}

func (m Mode) kind() string {
	switch m {
	case ModeTree:
		return "directory"
	case ModeSymlink:
		return "symbolic link"
	case ModeSubmodule:
		return "submodule"
	}
	return "file"
}

// compareEntries is git's order: by name, bytewise, where a directory's name is read as if it
// ended with a slash.
func compareEntries(a, b TreeEntry) int {
	n := min(len(a.Name), len(b.Name))
	if c := strings.Compare(a.Name[:n], b.Name[:n]); c != 0 {
		return c
	}
	next := func(e TreeEntry) int {
		switch {
		case len(e.Name) > n:
			return int(e.Name[n])
		case e.Mode == ModeTree:
			return '/'
		}
		return 0
	}
	return next(a) - next(b)
}

// EncodeTree writes a tree holding entries, in git's order whatever order they are given in, and
// refuses what ParseTree would refuse.
func EncodeTree(entries []TreeEntry) ([]byte, error) {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, compareEntries)
	var b []byte
	for _, e := range sorted {
		if !e.Mode.valid() {
			return nil, refuse(TypeTree, checkBadFilemode, "the mode %o, which is none of 40000, 100644, 100755, 120000 and 160000", uint32(e.Mode))
		}
		if strings.IndexByte(e.Name, 0) >= 0 {
			return nil, refuse(TypeTree, checkBadTree, "the name %q, holding the null byte that ends a name in a tree", e.Name)
		}
		b = append(b, e.Mode.String()...)
		b = append(b, ' ')
		b = append(b, e.Name...)
		b = append(b, 0)
		b = append(b, e.ID[:]...)
	}
	if _, err := ParseTree(b); err != nil {
		return nil, err
	}
	return b, nil
}
