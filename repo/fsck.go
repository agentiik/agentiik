package repo

import (
	"fmt"
	"strings"
)

// ObjectError is an object refused for what it holds: a commit, a tree or a tag that git fsck
// --strict refuses, or one this package refuses beside it.
type ObjectError struct {
	// Type is what the object is.
	Type Type
	// ID is the object's name, or zero where the content was parsed without one.
	ID ID
	// Check is git's own name for the check that failed, as git fsck prints it and as the
	// fsck.<check> setting names it, such as treeNotSorted or hasDotgit. A refusal named the way
	// git names it is one a person can look up in git's documentation, and one that reads the same
	// here as in git fsck on their own clone.
	Check string
	// Reason says what is wrong, as a sentence.
	Reason string
}

func (e *ObjectError) Error() string {
	var b strings.Builder
	b.WriteString("repo: the ")
	b.WriteString(e.Type.String())
	if !e.ID.IsZero() {
		b.WriteByte(' ')
		b.WriteString(e.ID.String())
	}
	b.WriteString(": ")
	b.WriteString(e.Reason)
	b.WriteString(" (")
	b.WriteString(e.Check)
	b.WriteByte(')')
	return b.String()
}

func refuse(t Type, check, format string, a ...any) *ObjectError {
	return &ObjectError{Type: t, Check: check, Reason: fmt.Sprintf(format, a...)}
}

// The checks, under the names git gives them (git help fsck, FSCK MESSAGES). Every one of them but
// badFilemode is an error under --strict, which is what a server that stores other people's history
// must hold it to: the objects are served to every clone, and a clone that sets
// transfer.fsckObjects refuses the lot for one of them. badFilemode, which git fsck reports and
// lets by, is refused as git hash-object refuses it: a mode other than the five is read by a
// checkout as one of them, and so is a second spelling of a tree.
const (
	checkBadTree                 = "badTree"
	checkBadFilemode             = "badFilemode"
	checkZeroPaddedFilemode      = "zeroPaddedFilemode"
	checkFullPathname            = "fullPathname"
	checkHasDot                  = "hasDot"
	checkHasDotdot               = "hasDotdot"
	checkHasDotgit               = "hasDotgit"
	checkLargePathname           = "largePathname"
	checkNullSha1                = "nullSha1"
	checkDuplicateEntries        = "duplicateEntries"
	checkTreeNotSorted           = "treeNotSorted"
	checkGitmodulesSymlink       = "gitmodulesSymlink"
	checkGitmodulesBlob          = "gitmodulesBlob"
	checkNulInHeader             = "nulInHeader"
	checkUnterminatedHeader      = "unterminatedHeader"
	checkMissingTree             = "missingTree"
	checkBadTreeSha1             = "badTreeSha1"
	checkBadParentSha1           = "badParentSha1"
	checkMissingAuthor           = "missingAuthor"
	checkMultipleAuthors         = "multipleAuthors"
	checkMissingCommitter        = "missingCommitter"
	checkNulInCommit             = "nulInCommit"
	checkMissingNameBeforeEmail  = "missingNameBeforeEmail"
	checkMissingEmail            = "missingEmail"
	checkBadName                 = "badName"
	checkMissingSpaceBeforeEmail = "missingSpaceBeforeEmail"
	checkBadEmail                = "badEmail"
	checkMissingSpaceBeforeDate  = "missingSpaceBeforeDate"
	checkZeroPaddedDate          = "zeroPaddedDate"
	checkBadDate                 = "badDate"
	checkBadDateOverflow         = "badDateOverflow"
	checkBadTimezone             = "badTimezone"
	checkMissingObject           = "missingObject"
	checkBadObjectSha1           = "badObjectSha1"
	checkMissingTypeEntry        = "missingTypeEntry"
	checkBadType                 = "badType"
	checkMissingTagEntry         = "missingTagEntry"
)

// verifyHeaders is git's verify_headers: no null byte before the blank line that ends the
// headers, and a last header line that ends, where there is no blank line and so no message.
func verifyHeaders(t Type, data []byte) *ObjectError {
	for i, c := range data {
		switch c {
		case 0:
			return refuse(t, checkNulInHeader, "a null byte at offset %d, among the headers", i)
		case '\n':
			if i+1 < len(data) && data[i+1] == '\n' {
				return nil
			}
		}
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		return nil
	}
	return refuse(t, checkUnterminatedHeader, "the headers do not end with a line feed")
}
