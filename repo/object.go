package repo

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
)

// ID names an object: the SHA-1 of its type, its size and its content, as git computes it.
type ID [sha1.Size]byte

// String is the ID as git writes it, 40 hexadecimal digits in lower case.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// IsZero is whether the ID is forty zeros, which names no object: git writes it for the old
// value of a ref being created and the new value of one being deleted.
func (id ID) IsZero() bool { return id == ID{} }

// ParseID reads an ID as git writes one.
//
// Upper case is refused, although git's own reader takes it: git never writes it, a ref or a
// line carrying it came from something other than git, and one spelling per ID is what lets an ID
// be compared as the string it arrived as.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 2*len(id) || !lowerHex(s) {
		return ID{}, fmt.Errorf("repo: %q is not an object ID, which is 40 hexadecimal digits in lower case", s)
	}
	hex.Decode(id[:], []byte(s))
	return id, nil
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Type is what an object is. The values are the ones a pack writes in an entry's header.
type Type byte

// The four types of object, with the numbers a pack gives them.
const (
	TypeCommit Type = 1
	TypeTree   Type = 2
	TypeBlob   Type = 3
	TypeTag    Type = 4
)

// String is the type as git names it in an object's header and in a tag.
func (t Type) String() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	}
	return "type " + strconv.Itoa(int(t))
}

func (t Type) valid() bool { return t >= TypeCommit && t <= TypeTag }

// parseType reads a type as a tag names the object it tags.
func parseType(s string) (Type, bool) {
	for _, t := range []Type{TypeCommit, TypeTree, TypeBlob, TypeTag} {
		if s == t.String() {
			return t, true
		}
	}
	return 0, false
}

// HashObject is the ID of an object of type t holding data.
func HashObject(t Type, data []byte) ID {
	h := newObjectHash(t, int64(len(data)))
	h.Write(data)
	return sumID(h)
}

// newObjectHash is a SHA-1 that has been given the header git hashes before an object's content,
// "blob 12" and a null byte, so that the content can be written to it as it is read.
func newObjectHash(t Type, size int64) hash.Hash {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", t, size)
	return h
}

func sumID(h hash.Hash) ID {
	var id ID
	h.Sum(id[:0])
	return id
}

// ErrMissing is what a Lookup answers for an object it does not hold.
//
// It is not fs.ErrNotExist, and TreeFS does not turn it into one: a tree naming an object the
// repository does not hold is a broken repository, and a reader told that a file is absent would
// go on as if the tree had been written without it.
var ErrMissing = errors.New("repo: the object is not in the repository")

// Lookup finds objects by their ID: a stored pack, every live pack of a repository, or a pack
// being received beside them. A thin pack's deltas are resolved against one, and TreeFS reads
// through one.
type Lookup interface {
	// OpenObject answers the object named id, to be read from its first byte and closed. An
	// object the lookup does not hold is an error wrapping ErrMissing.
	OpenObject(ctx context.Context, id ID) (ObjectReader, error)
}

// ObjectReader is one object's content, as it is read.
type ObjectReader interface {
	io.ReadCloser
	Type() Type
	// Size is the length of the content, known before it is read.
	Size() int64
}

// ReadObject reads the object named id whole, and refuses one larger than max bytes before it
// reads a byte of it.
func ReadObject(ctx context.Context, l Lookup, id ID, max int64) (Type, []byte, error) {
	r, err := l.OpenObject(ctx, id)
	if err != nil {
		return 0, nil, err
	}
	defer r.Close()
	if r.Size() > max {
		return 0, nil, fmt.Errorf("repo: the %s %s is %d bytes, more than the %d it may be to be read whole", r.Type(), id, r.Size(), max)
	}
	data := make([]byte, r.Size())
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, fmt.Errorf("repo: the %s %s: %w", r.Type(), id, err)
	}
	// Read to the end, so that a reader that checks what it served when it finishes has done so.
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		if err == nil || err == io.EOF {
			err = errors.New("the object is longer than its size")
		}
		return 0, nil, fmt.Errorf("repo: the %s %s: %w", r.Type(), id, err)
	}
	return r.Type(), data, nil
}
