package yamlbound

import (
	"fmt"
	"strings"
	"testing"
)

// bomb is a document of depth lines, each naming the line before it nine times: it stands for nine
// to the power depth values, and writes a few dozen bytes a line.
func bomb(depth int) string {
	var b strings.Builder
	b.WriteString("l0: &l0 [a, a, a, a, a, a, a, a, a]\n")
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&b, "l%d: &l%d [", i, i)
		for j := range 9 {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*l%d", i-1)
		}
		b.WriteString("]\n")
	}
	return b.String()
}

func TestADocumentBuiltToExhaustItsReaderIsRefused(t *testing.T) {
	for _, depth := range []int{6, 12, 40} {
		if err := Check([]byte(bomb(depth)), MaxValues); err == nil {
			t.Errorf("a document of %d lines each naming the one before nine times was accepted", depth)
		}
	}
}

// Anchors reusing a block a few times are what a person writes, and stand for a few times what
// they write.
func TestAnchorsAPersonWritesAreAccepted(t *testing.T) {
	doc := `
defaults: &defaults
  image: ghcr.io/acme/agk-invoice:1.4.0
  timeout: 10m
  retry: { max: 3, on: [lost] }
steps:
  normalize:
    <<: *defaults
    outputs: [ok, rejected]
  archive:
    <<: *defaults
    needs: [normalize]
`
	if err := Check([]byte(doc), MaxValues); err != nil {
		t.Error(err)
	}
	for _, depth := range []int{1, 2, 3, 4} {
		if err := Check([]byte(bomb(depth)), MaxValues); err != nil {
			t.Errorf("%d lines standing for %d values were refused: %s", depth, nine(depth), err)
		}
	}
}

// What a document stands for is counted as the decoder would build it: a line naming the one before
// nine times stands for nine of it and itself.
func TestWhatADocumentStandsForIsCountedAsDecoded(t *testing.T) {
	// l0 is a sequence and its nine scalars, 10; l1 a sequence of nine of those, 91; each key one
	// more, and the mapping holding them one.
	if err := Check([]byte(bomb(1)), 1+1+10+1+91); err != nil {
		t.Errorf("counted past what it stands for: %s", err)
	}
	if err := Check([]byte(bomb(1)), 1+1+10+1+91-1); err == nil {
		t.Error("counted short of what it stands for")
	}
}

// A document that does not parse is the decoder's to refuse, in its own words.
func TestADocumentThatDoesNotParseIsLeftToTheDecoder(t *testing.T) {
	if err := Check([]byte("a: [b, c\n"), MaxValues); err != nil {
		t.Error(err)
	}
}

func nine(depth int) int {
	n := 9
	for range depth {
		n *= 9
	}
	return n
}
