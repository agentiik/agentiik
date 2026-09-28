package repo

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Signature is who wrote a commit or a tag, and when, as the author, committer and tagger lines
// carry it.
type Signature struct {
	Name  string
	Email string
	// When is in seconds since the epoch.
	When int64
	// Zone is the offset from UTC the time was written in, as git writes it: +0100 or -0000.
	Zone string
}

// String is the signature as a commit's author line carries it, after the word author.
func (s Signature) String() string {
	return s.Name + " <" + s.Email + "> " + strconv.FormatInt(s.When, 10) + " " + s.Zone
}

// Header is a header line of a commit or a tag that is none of the ones this package reads, such
// as encoding, gpgsig and mergetag: its key, and its value, whose lines after the first are
// joined with line feeds, as git joins them.
type Header struct {
	Key   string
	Value string
}

// Commit is a commit object.
type Commit struct {
	Tree      ID
	Parents   []ID
	Author    Signature
	Committer Signature
	// Headers are the lines between the committer and the message, in their order.
	Headers []Header
	Message string
}

// ParseCommit reads a commit object and refuses one git fsck --strict refuses: a null byte
// anywhere, headers that do not end, a tree line or a parent line that does not name an object,
// no author or more than one, no committer, and an author or a committer line that does not
// read as a name, an email, a time and a zone.
func ParseCommit(data []byte) (*Commit, error) {
	if err := verifyHeaders(TypeCommit, data); err != nil {
		return nil, err
	}
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return nil, refuse(TypeCommit, checkNulInCommit, "a null byte at offset %d", i)
	}
	lines, message := splitHeaders(data)
	c := &Commit{Message: message}
	next := func(key string) (string, bool) {
		if len(lines) == 0 {
			return "", false
		}
		value, ok := strings.CutPrefix(lines[0], key+" ")
		if ok {
			lines = lines[1:]
		}
		return value, ok
	}
	value, ok := next("tree")
	if !ok {
		return nil, refuse(TypeCommit, checkMissingTree, "no tree line where the commit begins")
	}
	var err error
	if c.Tree, err = ParseID(value); err != nil {
		return nil, refuse(TypeCommit, checkBadTreeSha1, "the tree line %q, which names no object", value)
	}
	for {
		value, ok := next("parent")
		if !ok {
			break
		}
		parent, err := ParseID(value)
		if err != nil {
			return nil, refuse(TypeCommit, checkBadParentSha1, "the parent line %q, which names no object", value)
		}
		c.Parents = append(c.Parents, parent)
	}
	authors := 0
	for {
		value, ok := next("author")
		if !ok {
			break
		}
		if authors++; authors > 1 {
			return nil, refuse(TypeCommit, checkMultipleAuthors, "more than one author line")
		}
		if c.Author, err = parseSignature(TypeCommit, value); err != nil {
			return nil, err
		}
	}
	if authors == 0 {
		return nil, refuse(TypeCommit, checkMissingAuthor, "no author line after the tree and the parents")
	}
	value, ok = next("committer")
	if !ok {
		return nil, refuse(TypeCommit, checkMissingCommitter, "no committer line after the author")
	}
	if c.Committer, err = parseSignature(TypeCommit, value); err != nil {
		return nil, err
	}
	c.Headers = parseHeaders(lines)
	return c, nil
}

// splitHeaders answers the header lines of an object, without their line feeds, and its message:
// everything after the first blank line, or nothing where there is none.
func splitHeaders(data []byte) ([]string, string) {
	head, message := string(data), ""
	if i := strings.Index(head, "\n\n"); i >= 0 {
		head, message = head[:i+1], head[i+2:]
	}
	return strings.Split(strings.TrimSuffix(head, "\n"), "\n"), message
}

// parseHeaders reads the headers this package keeps as they are. A line beginning with a space
// continues the value above it, and one that continues nothing, which git's own reader drops, is
// kept as the value of a header with no key, so that it is written back where it was.
func parseHeaders(lines []string) []Header {
	var headers []Header
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, " "); ok && len(headers) > 0 {
			headers[len(headers)-1].Value += "\n" + rest
			continue
		} else if ok {
			headers = append(headers, Header{Value: rest})
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		headers = append(headers, Header{Key: key, Value: value})
	}
	return headers
}

// parseSignature is git's fsck_ident, on what follows the word author, committer or tagger.
func parseSignature(t Type, line string) (Signature, error) {
	if strings.HasPrefix(line, "<") {
		return Signature{}, refuse(t, checkMissingNameBeforeEmail, "the line %q has no name before its email", line)
	}
	lt := strings.IndexAny(line, "<>")
	switch {
	case lt < 0:
		return Signature{}, refuse(t, checkMissingEmail, "the line %q has no email", line)
	case line[lt] == '>':
		return Signature{}, refuse(t, checkBadName, "the line %q has a > in its name", line)
	case line[lt-1] != ' ':
		return Signature{}, refuse(t, checkMissingSpaceBeforeEmail, "the line %q has no space before its email", line)
	}
	s := Signature{Name: line[:lt-1]}
	rest := line[lt+1:]
	gt := strings.IndexAny(rest, "<>")
	if gt < 0 || rest[gt] == '<' {
		return Signature{}, refuse(t, checkBadEmail, "the line %q has an email that does not end with >", line)
	}
	s.Email, rest = rest[:gt], rest[gt+1:]
	rest, ok := strings.CutPrefix(rest, " ")
	if !ok {
		return Signature{}, refuse(t, checkMissingSpaceBeforeDate, "the line %q has no space before its date", line)
	}
	// Git's order: a zero not followed by the space after the date is a padded date, whatever
	// follows it.
	if strings.HasPrefix(rest, "0") && !strings.HasPrefix(rest, "0 ") {
		return Signature{}, refuse(t, checkZeroPaddedDate, "the line %q has a date written with a leading zero", line)
	}
	// Git reads the date with strtoumax, which also takes spaces before the digits, and git fsck
	// lets them by. Git never writes them, and they are refused, so that a date has one spelling.
	digits := len(rest) - len(strings.TrimLeft(rest, "0123456789"))
	if digits == 0 {
		return Signature{}, refuse(t, checkBadDate, "the line %q has no date", line)
	}
	when, err := strconv.ParseUint(rest[:digits], 10, 64)
	if err != nil || when > math.MaxInt64 {
		return Signature{}, refuse(t, checkBadDateOverflow, "the line %q has a date past what 64 bits hold", line)
	}
	s.When = int64(when)
	zone, ok := strings.CutPrefix(rest[digits:], " ")
	if !ok || !validZone(zone) {
		return Signature{}, refuse(t, checkBadTimezone, "the line %q has no zone written as +hhmm or -hhmm after its date", line)
	}
	s.Zone = zone
	return s, nil
}

func validZone(z string) bool {
	return len(z) == 5 && (z[0] == '+' || z[0] == '-') && strings.Trim(z[1:], "0123456789") == ""
}

// Encode writes the commit as git writes it, and refuses one ParseCommit would refuse or would
// read back as something else.
func (c *Commit) Encode() ([]byte, error) {
	var b strings.Builder
	b.WriteString("tree " + c.Tree.String() + "\n")
	for _, p := range c.Parents {
		b.WriteString("parent " + p.String() + "\n")
	}
	if err := encodeSignature(&b, TypeCommit, "author", c.Author); err != nil {
		return nil, err
	}
	if err := encodeSignature(&b, TypeCommit, "committer", c.Committer); err != nil {
		return nil, err
	}
	if err := encodeHeaders(&b, TypeCommit, c.Headers); err != nil {
		return nil, err
	}
	b.WriteString("\n" + c.Message)
	data := []byte(b.String())
	back, err := ParseCommit(data)
	if err != nil {
		return nil, err
	}
	if !back.equal(c) {
		return nil, fmt.Errorf("repo: the commit cannot be written so that it reads back as itself: its headers would be read as others")
	}
	return data, nil
}

func (c *Commit) equal(d *Commit) bool {
	return c.Tree == d.Tree && slices.Equal(c.Parents, d.Parents) && c.Author == d.Author && c.Committer == d.Committer &&
		slices.Equal(c.Headers, d.Headers) && c.Message == d.Message
}

func encodeSignature(b *strings.Builder, t Type, key string, s Signature) error {
	switch {
	case strings.ContainsAny(s.Name, "<>\n\x00"):
		return refuse(t, checkBadName, "the %s's name %q holds <, >, a line feed or a null byte", key, s.Name)
	case strings.ContainsAny(s.Email, "<>\n\x00"):
		return refuse(t, checkBadEmail, "the %s's email %q holds <, >, a line feed or a null byte", key, s.Email)
	case s.When < 0:
		return refuse(t, checkBadDate, "the %s's time %d is before the epoch, which git cannot write", key, s.When)
	case !validZone(s.Zone):
		return refuse(t, checkBadTimezone, "the %s's zone %q is not +hhmm or -hhmm", key, s.Zone)
	}
	b.WriteString(key + " " + s.String() + "\n")
	return nil
}

// encodeHeaders writes the headers after the ones this package reads. A key is refused where it
// would not read back as itself: empty but for the continuation ParseCommit keeps under no key,
// or holding a space, a line feed or a null byte.
func encodeHeaders(b *strings.Builder, t Type, headers []Header) error {
	for i, h := range headers {
		if strings.ContainsAny(h.Key, " \n\x00") || (h.Key == "" && i > 0) || strings.IndexByte(h.Value, 0) >= 0 {
			return fmt.Errorf("repo: the %s header %q cannot be written so that it reads back as itself: a key holds no space, line feed or null byte, only the first header may have none, and a value holds no null byte", t, h.Key)
		}
		// The first line of the value follows the key after a space, and each line after it
		// begins with one, which is what makes it a continuation. An empty first line is written
		// with no space, so that a key such as tagger, which a tag's own line begins with, reads
		// back as the header it was and not as that line; a header with no key is a continuation
		// of nothing, and begins with its space whatever follows.
		first, rest, more := strings.Cut(h.Value, "\n")
		b.WriteString(h.Key)
		if h.Key == "" || first != "" {
			b.WriteString(" " + first)
		}
		if more {
			b.WriteString("\n " + strings.ReplaceAll(rest, "\n", "\n "))
		}
		b.WriteByte('\n')
	}
	return nil
}
