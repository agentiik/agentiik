package driver

import (
	"bytes"
	"strings"
	"testing"
)

// The rule is one sentence of the documentation: "Secret values are masked in collected
// logs by literal match against the values known to the task, before anything is written
// to the store." These hold it to exactly that, including the two halves of what it
// buys, since the same page says what literal matching does not catch.

func TestASecretPrintedAsItArrivedIsMasked(t *testing.T) {
	m := newMasker([]byte("s3cr3t-value"))

	got := string(m.mask([]byte("Authorization: Bearer s3cr3t-value\n")))
	if strings.Contains(got, "s3cr3t-value") {
		t.Fatalf("the value was written in the clear: %q", got)
	}
	if want := "Authorization: Bearer " + maskToken + "\n"; got != want {
		t.Errorf("masked to %q, want %q", got, want)
	}
}

func TestEveryOccurrenceOfAValueIsMasked(t *testing.T) {
	m := newMasker([]byte("abc"))

	if got := string(m.mask([]byte("abc abc abc"))); strings.Contains(got, "abc") {
		t.Fatalf("an occurrence survived: %q", got)
	}
}

// A value carrying another inside it is replaced whole. Replacing the shorter one first
// would leave the rest of the longer one in the clear, which is the one thing the
// ordering exists for.
func TestAValueCarryingAnotherIsMaskedWhole(t *testing.T) {
	m := newMasker([]byte("abc"), []byte("abcdef"))

	got := string(m.mask([]byte("x abcdef y")))
	if strings.Contains(got, "def") {
		t.Fatalf("the rest of the longer value was left in the clear: %q", got)
	}
	if want := "x " + maskToken + " y"; got != want {
		t.Errorf("masked to %q, want %q", got, want)
	}
}

// A secret is mounted as a file, and a script reading it with $(cat /agk/secrets/name)
// prints it without the newline the file ends on. That is the same secret arriving by
// the commonest route there is.
func TestAValueIsCaughtWithoutTheWhitespaceTheFileEndsOn(t *testing.T) {
	m := newMasker([]byte("s3cr3t\n"))

	if got := string(m.mask([]byte("token=s3cr3t"))); strings.Contains(got, "s3cr3t") {
		t.Fatalf("the value was written in the clear once the newline was gone: %q", got)
	}
	if got := string(m.mask([]byte("token=s3cr3t\n"))); strings.Contains(got, "s3cr3t") {
		t.Fatalf("the value was written in the clear as it arrived: %q", got)
	}
}

// The empty value is not a value. Replacing it would put the token between every byte of
// the log and mask nothing at all.
func TestAnEmptyValueMasksNothing(t *testing.T) {
	m := newMasker([]byte(""), []byte("   \n"))

	if got := string(m.mask([]byte("nothing to hide"))); got != "nothing to hide" {
		t.Fatalf("an empty value rewrote the line: %q", got)
	}
}

// The other half of the same sentence, held to on purpose: masking is a guard against
// accident and never against intent, and a value the container encoded first is a value
// this cannot see.
func TestAValueEncodedFirstIsNotCaught(t *testing.T) {
	m := newMasker([]byte("s3cr3t"))

	if got := string(m.mask([]byte("czNjcjN0"))); got != "czNjcjN0" {
		t.Fatalf("a base64 encoding was rewritten, which literal matching cannot do: %q", got)
	}
}

func TestANilMaskerIsATaskWithNoSecrets(t *testing.T) {
	var m *masker

	if got := string(m.mask([]byte("anything"))); got != "anything" {
		t.Errorf("a nil masker rewrote the line: %q", got)
	}
	if m.hold() != 0 {
		t.Errorf("a nil masker holds %d bytes back, want none", m.hold())
	}
	if got := m.maskString("anything"); got != "anything" {
		t.Errorf("a nil masker rewrote the string: %q", got)
	}
}

// Nothing is written into the caller's bytes. The log hands the same buffer to the
// masker on every line, and a masker writing into it would rewrite what it was given.
func TestMaskingDoesNotWriteIntoWhatItWasGiven(t *testing.T) {
	m := newMasker([]byte("abc"))
	line := []byte("xxabcxx")
	held := bytes.Clone(line)

	m.mask(line)
	if !bytes.Equal(line, held) {
		t.Fatalf("the line was rewritten under the caller: %q", line)
	}
}

// hold is what makes a value survive a cut: a match needing the bytes after it begins
// inside the last len(longest)-1 bytes, and those are what is kept back.
func TestHoldIsTheLongestValueLessOne(t *testing.T) {
	m := newMasker([]byte("ab"), []byte("abcdef"), []byte("abcd"))

	if got := m.hold(); got != len("abcdef")-1 {
		t.Errorf("hold is %d, want %d", got, len("abcdef")-1)
	}
	if got := newMasker().hold(); got != 0 {
		t.Errorf("a task with no secrets holds %d bytes back, want none", got)
	}
}

// Two tasks holding the same values mask in the same order, so that one log read twice
// reads the same way. The order is length first and then the bytes themselves.
func TestTheValuesAreOrderedTheSameWayTwice(t *testing.T) {
	first := newMasker([]byte("bbb"), []byte("aaa"), []byte("cccc"))
	second := newMasker([]byte("aaa"), []byte("cccc"), []byte("bbb"))

	if len(first.values) != len(second.values) {
		t.Fatalf("held %d values and %d", len(first.values), len(second.values))
	}
	for i := range first.values {
		if !bytes.Equal(first.values[i], second.values[i]) {
			t.Fatalf("value %d is %q one way and %q the other", i, first.values[i], second.values[i])
		}
	}
}
