package repo

import (
	"fmt"
	"slices"
	"strings"
)

// Tag is an annotated tag object.
type Tag struct {
	// Object is what the tag names, and Type what that object is.
	Object ID
	Type   Type
	// Name is the tag's name, as the tag holds it, which need not be the ref it is under.
	Name string
	// Tagger is who made the tag, or nil where it holds no tagger line, as the earliest tags git
	// made do not.
	Tagger *Signature
	// Headers are the lines after the tagger and before the message, in their order.
	Headers []Header
	Message string
}

// ParseTag reads a tag object and refuses one git fsck --strict refuses: a null byte among its
// headers, headers that do not end, an object line that does not name an object, a type line that
// names no type, no tag line, and a tagger line that does not read as a name, an email, a time and
// a zone.
//
// A tag with no tagger, extra headers after the tagger, and a name that would be no ref's are let
// by, as git fsck lets them by: the first two are what old and signed tags hold, and the name is
// the tag's own text, which moves no ref.
func ParseTag(data []byte) (*Tag, error) {
	if err := verifyHeaders(TypeTag, data); err != nil {
		return nil, err
	}
	lines, message := splitHeaders(data)
	t := &Tag{Message: message}
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
	value, ok := next("object")
	if !ok {
		return nil, refuse(TypeTag, checkMissingObject, "no object line where the tag begins")
	}
	var err error
	if t.Object, err = ParseID(value); err != nil {
		return nil, refuse(TypeTag, checkBadObjectSha1, "the object line %q, which names no object", value)
	}
	if value, ok = next("type"); !ok {
		return nil, refuse(TypeTag, checkMissingTypeEntry, "no type line after the object")
	}
	if t.Type, ok = parseType(value); !ok {
		return nil, refuse(TypeTag, checkBadType, "the type %q, which is none of commit, tree, blob and tag", value)
	}
	if t.Name, ok = next("tag"); !ok {
		return nil, refuse(TypeTag, checkMissingTagEntry, "no tag line after the type")
	}
	if value, ok := next("tagger"); ok {
		tagger, err := parseSignature(TypeTag, value)
		if err != nil {
			return nil, err
		}
		t.Tagger = &tagger
	}
	t.Headers = parseHeaders(lines)
	return t, nil
}

// Encode writes the tag as git writes it, and refuses one ParseTag would refuse or would read back
// as something else.
func (t *Tag) Encode() ([]byte, error) {
	if !t.Type.valid() {
		return nil, refuse(TypeTag, checkBadType, "the type %s, which is none of commit, tree, blob and tag", t.Type)
	}
	if strings.ContainsAny(t.Name, "\n\x00") {
		return nil, fmt.Errorf("repo: the tag name %q holds a line feed or a null byte, which end a header", t.Name)
	}
	var b strings.Builder
	b.WriteString("object " + t.Object.String() + "\n")
	b.WriteString("type " + t.Type.String() + "\n")
	b.WriteString("tag " + t.Name + "\n")
	if t.Tagger != nil {
		if err := encodeSignature(&b, TypeTag, "tagger", *t.Tagger); err != nil {
			return nil, err
		}
	}
	if err := encodeHeaders(&b, TypeTag, t.Headers); err != nil {
		return nil, err
	}
	b.WriteString("\n" + t.Message)
	data := []byte(b.String())
	back, err := ParseTag(data)
	if err != nil {
		return nil, err
	}
	if !back.equal(t) {
		return nil, fmt.Errorf("repo: the tag cannot be written so that it reads back as itself: a header named tagger would be read as its tagger")
	}
	return data, nil
}

func (t *Tag) equal(u *Tag) bool {
	sameTagger := (t.Tagger == nil) == (u.Tagger == nil) && (t.Tagger == nil || *t.Tagger == *u.Tagger)
	return t.Object == u.Object && t.Type == u.Type && t.Name == u.Name && sameTagger &&
		slices.Equal(t.Headers, u.Headers) && t.Message == u.Message
}
