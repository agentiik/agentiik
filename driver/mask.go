package driver

import (
	"bytes"
	"slices"
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

// newMasker takes the values a task was given, as Config.Secrets redeemed them.
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
