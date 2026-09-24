package driver

import (
	"bytes"
	"slices"

	"github.com/agentiik/agentiik/agk"
)

// maskToken is what a secret value is replaced by.
//
// It names what happened rather than merely hiding the text, so that a person reading a
// log knows a value stood there, and it is one constant width, so that the length of the
// value it stands for cannot be read off the line either.
const maskToken = "[masked]"

// masker replaces the secret values a task was given wherever they appear literally.
//
// Literal match is the whole of the rule, and the documentation is explicit about what
// that buys and what it does not: it catches a secret printed as it arrived, and does
// not catch one that was base64-encoded, URL-encoded, split across lines or hashed
// first. It is a guard against accident, never against intent.
//
// A nil masker is a task with no secrets, and every method works on one. That is what
// lets the log and the capture hold a *masker without asking whether there is anything
// to mask.
type masker struct {
	// The values, longest first, so that a value carrying another inside it is
	// replaced whole rather than having its own text broken up by the shorter one
	// and the remainder left in the clear.
	values [][]byte
}

// newMasker takes the values a task was given, as its secret source redeemed them:
// Sources.Secrets, or Config.Secrets where a runner gave none, and for a container a
// delivery adopted, the values the first delivery wrote for it as well.
//
// Each value is held twice where the two differ: as it arrived, and with the whitespace
// around it removed. A secret is mounted as a file, and a script reading it with
// $(cat /agk/secrets/name) prints it without the newline the file ends on, which is the
// same secret reaching the log by the commonest route there is.
func newMasker(values ...[]byte) *masker {
	m := &masker{}
	seen := make(map[string]bool, 2*len(values))
	for _, v := range values {
		for _, candidate := range [][]byte{v, bytes.TrimSpace(v)} {
			// The empty value is not a value. Replacing it would put the token
			// between every byte of the log and mask nothing at all.
			if len(candidate) == 0 || seen[string(candidate)] {
				continue
			}
			seen[string(candidate)] = true
			m.values = append(m.values, bytes.Clone(candidate))
		}
	}
	slices.SortFunc(m.values, func(a, b []byte) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		// Equal lengths are ordered by their bytes, so that two tasks holding the
		// same values mask in the same order and a log reads the same way twice.
		return bytes.Compare(a, b)
	})
	return m
}

// mask replaces every value it holds.
//
// It returns b itself when nothing matched, and a new slice when something did, so a
// caller that keeps the result must not assume either. Nothing here writes into b.
func (m *masker) mask(b []byte) []byte {
	if m == nil || len(m.values) == 0 || len(b) == 0 {
		return b
	}
	token := []byte(maskToken)
	for _, v := range m.values {
		// Asked before replacing, because ReplaceAll allocates whether or not it
		// finds anything, and most lines of most logs carry no secret at all.
		if bytes.Contains(b, v) {
			b = bytes.ReplaceAll(b, v, token)
		}
	}
	return b
}

// maskString is mask for the lines the driver writes itself, which are built as strings.
func (m *masker) maskString(s string) string {
	if m == nil || len(m.values) == 0 || s == "" {
		return s
	}
	return string(m.mask([]byte(s)))
}

// hold is how many bytes have to stay behind when text is written out in pieces rather
// than a line at a time.
//
// A value can only be matched once it is whole, so everything that could still turn out
// to be the beginning of one is kept back for the next piece. A match that needs the
// bytes after the cut starts inside the last len(longest)-1 bytes, and those are exactly
// what this holds.
func (m *masker) hold() int {
	if m == nil || len(m.values) == 0 {
		return 0
	}
	// The values are sorted longest first, so the first one is the longest.
	return len(m.values[0]) - 1
}

// maskItems is the payload half of the rule, which doc.go states and which for a while only
// the standard output shorthand kept: "masking covers the payload and not only the log".
//
// The subject is what a brick wrote at /agk/out/ports/<port>.json. That document is collected
// output exactly as the log is, and a brick that reads /agk/secrets/<name> and puts the value
// in a field of an item it publishes would otherwise have it written to the object store, to
// the state of the run, to the envelope on disk and to standard output in the clear. It is the
// same literal match, with the same guarantee and the same limits: a value as it arrived is
// caught, one that was encoded or hashed first is not.
//
// What it walks is items[].data and nothing else. An item's identity is what a shard, a merge
// and a replay speak about, and a files[] entry is a reference to bytes whose digest the
// envelope asserts and a consumer verifies: rewriting either would refuse a downstream step
// for a reason nobody could find, and a secret deliberately written into an artifact's bytes is
// the case the documentation already answers, masking being "a guard against accident, never
// against intent".
//
// Nothing handed in is written into. The envelopes come back from brick.Collect and the caller
// still holds them, so a mask that mutated a map in place would make the order of two
// collections matter. A task with no secrets is returned as it came, which is most tasks.
//
// One consequence is wider here than it is in a log, and it is the documentation's own rule
// rather than a defect: a literal match finds the value wherever it appears, so a value short
// enough to occur by accident rewrites payload a person meant to keep. A one-character secret
// turns the reference a1 into a[masked]. The answer is not a minimum length on this side, which
// would quietly stop masking a short value that really is a secret; it is that a value with so
// little entropy was never a secret, and the documentation says what masking is for: "a guard
// against accident, never against intent".
func maskItems(m *masker, items []agk.Item) []agk.Item {
	if m == nil || len(m.values) == 0 || len(items) == 0 {
		return items
	}
	out := make([]agk.Item, len(items))
	for i, item := range items {
		out[i] = item
		out[i].Data = m.maskValue(item.Data).(map[string]any)
	}
	return out
}

// maskValue walks one decoded JSON value and replaces every string inside it.
//
// A key is walked as well as a value, because a brick that wrote a secret as a field name put
// it in the document just the same, and a decoded document is the four shapes below and
// nothing else: what came out of encoding/json is an object, an array, a string, or a scalar
// that cannot hold text.
func (m *masker) maskValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[m.maskString(key)] = m.maskValue(value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = m.maskValue(value)
		}
		return out
	case string:
		return m.maskString(v)
	default:
		// A boolean and null carry no text. A number carries digits and is deliberately
		// not walked: agk.Decode reads a payload with UseNumber, so a number arrives as
		// json.Number, which is a string type this case catches rather than the string
		// case above. Replacing inside one would leave a document whose number is the
		// word [masked], which is not a number, and the envelope would then be refused
		// by its own size and shape rules with nothing to say why. A secret that is a
		// bare number is the encoded case the documentation already excludes: "it
		// catches a secret printed as it arrived", and a value that has to be read as a
		// number to be used was not printed, it was parsed.
		return v
	}
}
