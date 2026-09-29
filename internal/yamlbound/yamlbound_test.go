package yamlbound

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// laughs is a document of depth lines, each naming the line before it nine times: it stands for nine
// to the power depth values, and writes a few dozen bytes a line.
func laughs(depth int) string {
	var b strings.Builder
	b.WriteString("l0: &l0 [a, a, a, a, a, a, a, a, a]\n")
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&b, "l%d: &l%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 9), ", "))
	}
	return b.String()
}

// refusedBoth holds a document to being refused by Check, and, where the decoder takes it at all,
// by CheckValue over what it decodes to: the second pass stops what the first could not foresee.
func refusedBoth(t *testing.T, name, doc string) {
	t.Helper()
	if err := Check([]byte(doc)); err == nil {
		t.Errorf("%s (%d bytes) passes Check", name, len(doc))
	}
	var v any
	if yaml.Unmarshal([]byte(doc), &v) == nil {
		if err := CheckValue(v); err == nil {
			t.Errorf("%s (%d bytes) passes CheckValue once decoded", name, len(doc))
		}
	}
}

func TestADocumentBuiltToExhaustItsReaderIsRefused(t *testing.T) {
	for _, depth := range []int{6, 12, 40} {
		refusedBoth(t, fmt.Sprintf("%d lines each naming the one before nine times", depth), laughs(depth))
	}
}

// What the decoder builds is what its names mean as it reads, and it reads a merged anchor's tree
// again at every merge: a name defined twice would mean one thing to the count and another to the
// decoder. The two shapes the review of this package found, each refused whatever its size.
func TestAnAnchorDefinedTwiceIsRefused(t *testing.T) {
	var redefinedInAMerge strings.Builder
	redefinedInAMerge.WriteString("p0: &p0 a\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&redefinedInAMerge, "t%d: &t%d {k: &p%d [%s]}\nx%d: &p%d s\nu%d: {<<: *t%d}\n",
			i, i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*p%d, ", i-1), 9), ", "), i, i, i, i)
	}
	var definedByAMerge strings.Builder
	definedByAMerge.WriteString("p0: &p0 a\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&definedByAMerge, "b%d: &p%d [%s]\nm%d: {<<: &p%d {k: v}}\n",
			i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*p%d, ", i-1), 9), ", "), i, i)
	}
	for name, doc := range map[string]string{
		"an anchor redefined inside a merged one": redefinedInAMerge.String(),
		"an anchor defined by a merge":            definedByAMerge.String(),
		"one anchor written twice":                "a: &x 1\nb: &x 2\nc: *x\n",
	} {
		err := Check([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), "defined twice") {
			t.Errorf("%s is answered %v", name, err)
		}
	}
	if err := Check([]byte("a: *x\nb: &x 1\n")); err == nil {
		t.Error("an alias naming no anchor before it passes")
	}
}

// Bytes are counted wherever they are repeated: one alias to a long scalar, or a long key, repeated,
// stands for as many copies of it as a reader writes out.
func TestWhatARepeatedScalarOrKeyWeighsIsCounted(t *testing.T) {
	long := strings.Repeat("A", 128<<10)
	refusedBoth(t, "a scalar of 128 KiB named 1,600 times",
		"s: &x "+long+"\nd: ["+strings.TrimSuffix(strings.Repeat("*x, ", 1600), ", ")+"]\n")

	var key strings.Builder
	fmt.Fprintf(&key, "k: &k %s\nm0: &m0 {*k : 1}\n", strings.Repeat("A", 200<<10))
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&key, "m%d: &m%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*m%d, ", i-1), 9), ", "))
	}
	refusedBoth(t, "a key of 200 KiB repeated nine to the fifth times", key.String())
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
	for name, doc := range map[string]string{"merge keys": doc, "4 lines of nine": laughs(4)} {
		if err := Check([]byte(doc)); err != nil {
			t.Errorf("%s: %s", name, err)
		}
		var v any
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatal(err)
		}
		if err := CheckValue(v); err != nil {
			t.Errorf("%s once decoded: %s", name, err)
		}
	}
}

// What a document stands for is counted as the decoder would build it: a line naming the one before
// nine times stands for nine of it and itself.
func TestWhatADocumentStandsForIsCountedAsDecoded(t *testing.T) {
	// l0 is a sequence and its nine scalars, 10; l1 a sequence of nine of those, 91; each key one
	// more, and the mapping holding them one.
	const values = 1 + 1 + 10 + 1 + 91
	if err := check([]byte(laughs(1)), budget{values, MaxBytes}); err != nil {
		t.Errorf("counted past what it stands for: %s", err)
	}
	if err := check([]byte(laughs(1)), budget{values - 1, MaxBytes}); !errors.Is(err, ErrTooMuch) {
		t.Errorf("counted short of what it stands for: %v", err)
	}
	var v any
	if err := yaml.Unmarshal([]byte(laughs(1)), &v); err != nil {
		t.Fatal(err)
	}
	if err := checkValue(v, budget{values - 2, MaxBytes}); err != nil {
		t.Errorf("the decoded value, which holds no key as a value, counted past what it stands for: %s", err)
	}
}

// A document that does not parse is the decoder's to refuse, in its own words.
func TestADocumentThatDoesNotParseIsLeftToTheDecoder(t *testing.T) {
	if err := Check([]byte("a: [b, c\n")); err != nil {
		t.Error(err)
	}
}

// The shape the parser pays the square of is refused before it parses: nesting past MaxDepth, a path
// past MaxPath, and an explicit key.
func TestADocumentThatWouldCostTheParserTheSquareOfItsSizeIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"flow sequences nested 129 deep":     "x: " + strings.Repeat("[", 129) + strings.Repeat("]", 129) + "\n",
		"flow sequences nested 500,000 deep": "x: " + strings.Repeat("[", 500_000) + strings.Repeat("]", 500_000) + "\n",
		"a key of 2 KiB":                     strings.Repeat("k", 2048) + ": 1\n",
		"keys of 300 bytes four deep":        "a" + strings.Repeat("a", 300) + ":\n  b" + strings.Repeat("b", 300) + ":\n    c" + strings.Repeat("c", 300) + ":\n      d" + strings.Repeat("d", 300) + ": 1\n",
		"an explicit key":                    "? a\n: 1\n",
	} {
		if err := Check([]byte(doc)); err == nil {
			t.Errorf("%s passes", name)
		}
	}
}

// What a person writes has its paths followed as the parser builds them, and passes: siblings do not
// add up, a sequence under a key at the key's own column is under the key, and a flow collection's
// entries are under the collection alone.
func TestTheShapeAPersonWritesPasses(t *testing.T) {
	var siblings strings.Builder
	for i := range 500 {
		fmt.Fprintf(&siblings, "key-%03d-%s: 1\n", i, strings.Repeat("x", 100))
	}
	for name, doc := range map[string]string{
		"500 sibling keys of 100 bytes":  siblings.String(),
		"a sequence at its key's column": "steps:\n- " + strings.Repeat("a", 900) + ": 1\n- " + strings.Repeat("b", 900) + ": 2\n",
		"flow entries":                   "x: {" + strings.Repeat("k", 900) + ": 1, " + strings.Repeat("j", 900) + ": 2, l: [" + strings.Repeat("m, ", 1000) + "n]}\n",
		"a workflow":                     "apiVersion: agentiik.dev/v1\nkind: Workflow\nmetadata: { name: nightly }\nsteps:\n  normalize:\n    image: alpine\n    script:\n      - echo one\n      - echo two\n    needs: [{ step: fetch, port: ok }]\n",
		"flow sequences nested 128 deep": "x: " + strings.Repeat("[", 127) + strings.Repeat("]", 127) + "\n",
	} {
		if err := Check([]byte(doc)); err != nil {
			t.Errorf("%s: %s", name, err)
		}
	}
}
