package graph

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// A key join says what it matches items on as a JSON path, join: { on: "$.data.customer_id" },
// and that is the only place in the language a path is written. What is read here is what a
// key join needs and nothing more: the root, named members and array indices. A path is a
// value to match by, never a query returning several values, so there is no wildcard, no
// slice and no filter to implement and none to explain.
//
// The path is read against one item rather than against the envelope, because a join
// matches items: $ is the item, and $.data.customer_id is the member customer_id of its
// data. An item carries id, data and files and nothing else, so a path reaching for
// anything else selects nothing, and an item whose key selects nothing leaves on the
// unmatched port rather than disappearing quietly.

// selector is a parsed join path, kept with the text it was written as so that a refusal
// quotes the author's own line.
type selector struct {
	source string
	steps  []pathStep
}

// pathStep is one member name or one array index of the path.
type pathStep struct {
	name  string
	index int
	named bool
}

// parsePath reads a join path. It refuses at the moment the workflow is read rather than
// at the moment a join runs, so a path nobody can match is found before a run is started.
func parsePath(path string) (selector, error) {
	s := selector{source: path}
	rest := strings.TrimSpace(path)
	if rest == "" {
		return selector{}, fmt.Errorf("a join matches items on a JSON path and this one is empty, for example %q", "$.data.customer_id")
	}
	if !strings.HasPrefix(rest, "$") {
		return selector{}, fmt.Errorf("a join path starts at the item, so it begins with $: %q", path)
	}
	rest = rest[1:]
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := rest[:end]
			if name == "" {
				return selector{}, fmt.Errorf("a join path names a member after each dot: %q", path)
			}
			s.steps = append(s.steps, pathStep{name: name, named: true})
			rest = rest[end:]
		case strings.HasPrefix(rest, "["):
			end := strings.Index(rest, "]")
			if end < 0 {
				return selector{}, fmt.Errorf("a join path closes every bracket it opens: %q", path)
			}
			inner := strings.TrimSpace(rest[1:end])
			rest = rest[end+1:]
			if len(inner) >= 2 && (inner[0] == '"' && inner[len(inner)-1] == '"' || inner[0] == '\'' && inner[len(inner)-1] == '\'') {
				s.steps = append(s.steps, pathStep{name: inner[1 : len(inner)-1], named: true})
				continue
			}
			n, err := strconv.Atoi(inner)
			if err != nil || n < 0 {
				return selector{}, fmt.Errorf("a join path indexes an array with a whole number and names a member with a quoted string, and %q is neither, in %q", inner, path)
			}
			s.steps = append(s.steps, pathStep{index: n})
		default:
			return selector{}, fmt.Errorf("a join path is made of .member and [index] steps, and %q is neither, in %q", rest, path)
		}
	}
	if len(s.steps) == 0 {
		return selector{}, fmt.Errorf("a join matches on a value inside the item, and $ alone is the whole item: %q", path)
	}
	if head := s.steps[0]; !head.named || (head.name != "id" && head.name != "data" && head.name != "files") {
		return selector{}, fmt.Errorf("an item carries id, data and files, so a join path reads one of those three and not %q", path)
	}
	return s, nil
}

// value resolves the path against one item. The second result says whether the item
// carries the value at all, which is what tells a matched item from an unmatched one.
func (s selector) value(it agk.Item) (any, bool) {
	var current any
	switch s.steps[0].name {
	case "id":
		current = it.ID
	case "data":
		if it.Data == nil {
			return nil, false
		}
		current = map[string]any(it.Data)
	case "files":
		current = filesValue(it.Files)
	}
	for _, step := range s.steps[1:] {
		switch {
		case step.named:
			m, ok := current.(map[string]any)
			if !ok {
				return nil, false
			}
			current, ok = m[step.name]
			if !ok {
				return nil, false
			}
		default:
			list, ok := current.([]any)
			if !ok || step.index >= len(list) {
				return nil, false
			}
			current = list[step.index]
		}
	}
	return current, true
}

// key turns the value an item matches by into something two items can be compared on.
//
// The comparison is made on the JSON encoding of the value rather than on the value
// itself, so that a number, a string and an object each compare as the document says
// they are, and two items matching on an object match when the object is the same one.
// A key that is null is a key an item does not carry: null is the absence of a value,
// and matching every item that is missing the key against every other would join rows
// that have nothing in common.
func (s selector) key(it agk.Item) (string, bool) {
	v, ok := s.value(it)
	if !ok || v == nil {
		return "", false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// filesValue presents the files of an item the way a path reads them, and is built only
// when a path actually asks for one, because a join on $.data.<key> is the ordinary case
// and it should not pay for this.
func filesValue(files []agk.File) []any {
	out := make([]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{
			"name":       f.Name,
			"uri":        f.URI.String(),
			"media_type": f.MediaType,
			"size":       f.Size,
			"sha256":     f.SHA256,
		})
	}
	return out
}
