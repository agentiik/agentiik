package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTheTwoFormsAreTheTwoTheDocumentationWrites reads the parser directly, because the
// whole of the decision is which expressions exist: the page writes .valid and .valid |
// not, and this accepts those two and refuses the rest.
func TestTheTwoFormsAreTheTwoTheDocumentationWrites(t *testing.T) {
	for _, c := range []struct {
		expr   string
		path   string
		negate bool
	}{
		{".valid", "valid", false},
		{".valid | not", "valid", true},
		{".valid|not", "valid", true},
		{"  .valid  |  not  ", "valid", true},
		{".response.valid", "response.valid", false},
		{".vat_number", "vat_number", false},
		{".x-ray", "x-ray", false},
		{".0", "0", false},
	} {
		f, err := parseFilter(c.expr)
		if err != nil {
			t.Errorf("%q is refused: %s", c.expr, err)
			continue
		}
		if got := strings.Join(f.path, "."); got != c.path {
			t.Errorf("%q reads the path %q, want %q", c.expr, got, c.path)
		}
		if f.negate != c.negate {
			t.Errorf("%q reads negate %v", c.expr, f.negate)
		}
	}
}

// TestAnythingElseIsRefusedNamingWhatIsSupported is where the subset stops pretending. A
// filter that accepted .items[0].name and then ignored the index would be worse than one
// that refused it.
func TestAnythingElseIsRefusedNamingWhatIsSupported(t *testing.T) {
	for _, expr := range []string{
		"valid",
		".",
		"..",
		".a..b",
		".items[0]",
		`.["valid"]`,
		".valid | length",
		".valid | not | not",
		".valid and .checked",
		"select(.valid)",
		".valid == true",
		"$x",
		".a.b.",
		".a b",
	} {
		_, err := parseFilter(expr)
		if err == nil {
			t.Errorf("%q is accepted, and it is not one of the two forms", expr)
			continue
		}
		if !strings.Contains(err.Error(), "--filter takes a field path") {
			t.Errorf("the refusal of %q does not say what is supported: %s", expr, err)
		}
		// It points at jq as the tool that does the rest, and never calls itself
		// one: a subset answering to that name would promise a language and
		// deliver two expressions of it.
		if !strings.Contains(err.Error(), "jq and a redirect do the same job") {
			t.Errorf("the refusal of %q does not point at jq: %s", expr, err)
		}
	}
}

// TestNoFilterKeepsEverything is the default, which is the case the documented example's
// other three emits are.
func TestNoFilterKeepsEverything(t *testing.T) {
	f, err := parseFilter("")
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{}`, `{"valid":false}`, `{"valid":null}`} {
		if !f.keeps(decode(t, payload)) {
			t.Errorf("no filter dropped %s", payload)
		}
	}
}

// TestTruthinessIsStatedRatherThanAssumed is the rule --filter rests on, written where it
// can be argued with: only null, false and an absent field are false.
func TestTruthinessIsStatedRatherThanAssumed(t *testing.T) {
	for _, c := range []struct {
		payload string
		keep    bool
	}{
		{`{"valid":true}`, true},
		{`{"valid":false}`, false},
		{`{"valid":null}`, false},
		{`{}`, false},
		{`{"valid":0}`, true},
		{`{"valid":""}`, true},
		{`{"valid":[]}`, true},
		{`{"valid":{}}`, true},
		{`{"valid":"no"}`, true},
	} {
		f, err := parseFilter(".valid")
		if err != nil {
			t.Fatal(err)
		}
		if got := f.keeps(decode(t, c.payload)); got != c.keep {
			t.Errorf("%s is kept %v, want %v", c.payload, got, c.keep)
		}
		// The negated form is the exact complement, which is what lets the two
		// partition one payload across two ports.
		n, err := parseFilter(".valid | not")
		if err != nil {
			t.Fatal(err)
		}
		if n.keeps(decode(t, c.payload)) == c.keep {
			t.Errorf("%s is kept by both forms or by neither", c.payload)
		}
	}
}

// TestAPathThroughSomethingThatIsNotAnObjectIsFalse covers the field path that stops being
// followable halfway: a step is absent rather than an error, because the item simply does
// not carry what was asked for.
func TestAPathThroughSomethingThatIsNotAnObjectIsFalse(t *testing.T) {
	f, err := parseFilter(".response.valid")
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{"response":true}`, `{"response":[{"valid":true}]}`, `{"response":{}}`, `{}`} {
		if f.keeps(decode(t, payload)) {
			t.Errorf("%s passes .response.valid", payload)
		}
	}
	if !f.keeps(decode(t, `{"response":{"valid":true}}`)) {
		t.Error("a path of two segments does not follow one")
	}
}

// decode reads one payload the way emit reads it, numbers held as they were written.
func decode(t *testing.T, payload string) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(payload))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}
