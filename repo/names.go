package repo

import "strings"

// DotGit is whether a name is .git on some filesystem a tree may be checked out on, which is the
// rule git applies itself, with core.protectNTFS and core.protectHFS, before it writes a name, and
// which git fsck applies to every name of every tree as hasDotgit.
//
// A filesystem that folds case makes .GIT one. NTFS also drops the dots and spaces a name ends
// with, reads what follows a colon as a stream of the file before it, reads a backslash as a
// separator, and gives .git the short name GIT~1, so .git., .git::$INDEX_ALLOCATION, a\.git and
// GIT~1 are each .git there. HFS+ ignores a handful of invisible code points, so .g\u200cit is
// .git on it. On a Linux disk every one of these is an ordinary name, and a tree is laid out on
// whatever disk its runner has, and cloned onto whatever disk its author has.
//
// The API's tree push applies it to every segment of a path, so both hold the one rule.
func DotGit(name string) bool {
	for _, part := range strings.Split(name, `\`) {
		if dotGitPart(part) {
			return true
		}
	}
	return false
}

func dotGitPart(name string) bool {
	s := ntfsName(hfsName(name))
	if strings.EqualFold(s, ".git") {
		return true
	}
	// GIT~1, and any other number, since which one NTFS gives depends on what the directory held
	// before. Git refuses GIT~1 alone; the others are refused here because a runner's disk is not
	// git's business and is this one's.
	number, short := strings.CutPrefix(strings.ToLower(s), "git~")
	return short && number != "" && strings.Trim(number, "0123456789") == ""
}

// hfsName is a name without the code points HFS+ leaves out when it compares two names: the ones
// git's own is_hfs_dotgit skips.
func hfsName(name string) string {
	return strings.Map(func(r rune) rune {
		if hfsIgnores(r) {
			return -1
		}
		return r
	}, name)
}

func hfsIgnores(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
		return true
	}
	return false
}

// ntfsName is the name NTFS opens for a name: what comes before a colon, which is a stream of that
// file, without the dots and spaces it ends with.
func ntfsName(name string) string {
	if colon := strings.IndexByte(name, ':'); colon >= 0 {
		name = name[:colon]
	}
	return strings.TrimRight(name, ". ")
}

// dotGitmodules is whether a name is .gitmodules on some filesystem, as git's is_hfs_dotgitmodules
// and is_ntfs_dotgitmodules read it. Git holds that name to be a file, since a clone reads it to
// decide where submodules go, and one that is a symbolic link would have it read something the
// tree does not hold.
func dotGitmodules(name string) bool {
	for _, part := range strings.Split(name, `\`) {
		if strings.EqualFold(hfsName(part), ".gitmodules") || ntfsDotGitmodules(part) {
			return true
		}
	}
	return false
}

// ntfsDotGitmodules is git's is_ntfs_dot_generic for .gitmodules: the long name, the short name
// NTFS derives from its first six letters, GITMOD~1 to ~4, and the one it falls back to past
// those, a hash of the name that is gi7eba followed by a tilde and a number, each with what NTFS
// drops from the end of a name.
func ntfsDotGitmodules(name string) bool {
	s := ntfsName(name)
	lower := strings.ToLower(s)
	if lower == ".gitmodules" {
		return true
	}
	if len(lower) == 8 && lower[:7] == "gitmod~" && lower[7] >= '1' && lower[7] <= '4' {
		return true
	}
	// The fall-back short name: a prefix of gi7eba, a tilde, a digit from 1 and digits, eight
	// characters in all.
	if len(lower) != 8 {
		return false
	}
	tilde := strings.IndexByte(lower, '~')
	if tilde < 0 || tilde > 6 || lower[:tilde] != "gi7eba"[:tilde] {
		return false
	}
	digits := lower[tilde+1:]
	return digits != "" && digits[0] >= '1' && digits[0] <= '9' && strings.Trim(digits, "0123456789") == ""
}
