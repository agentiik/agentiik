package diff

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// Difference is one member of one item of one port that does not match.
//
// The order of the fields is the order somebody looks in: which port, which item, which
// member. Item is empty where the difference is about the port itself, which is its
// absence or the length of its batch, and Member is empty where it is about the port's
// presence.
type Difference struct {
	Port   agk.Port
	Item   string
	Member string
	Want   any
	Got    any
}

// String reads as one line naming the port, the item and the member, with what was wanted
// and what was got.
//
// Never a whole-document dump. A reader holding two envelopes of two hundred items needs
// the one member that moved, and a diff of two pretty-printed documents makes them find
// it themselves.
func (d Difference) String() string {
	var b strings.Builder
	b.WriteString("port " + string(d.Port))
	if d.Item != "" {
		b.WriteString(": item " + d.Item)
	}
	if d.Member != "" {
		b.WriteString(": " + d.Member)
	}
	b.WriteString(": want " + show(d.Want) + ", got " + show(d.Got))
	return b.String()
}

// show writes one value the way the document writes it, so that what a reader sees is
// what they can search the file for.
//
// A value that is not a number, a string, a boolean or nothing is written as the compact
// JSON it is, because a member whose whole shape changed, an object where a list was
// written, is a difference about the shape and not about a leaf inside it.
func show(v any) string {
	switch value := v.(type) {
	case nil:
		return "nothing"
	case string:
		return fmt.Sprintf("%q", value)
	case json.Number:
		return value.String()
	case bool, int, int64, float64:
		return fmt.Sprintf("%v", value)
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano)
	case agk.URI:
		return value.String()
	case phrase:
		// A difference stated in words, printed as it reads.
		return string(value)
	case agk.RunID:
		return string(value)
	case agk.Step:
		return string(value)
	case agk.Port:
		return string(value)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
