package api

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/agentiik/agentiik/internal/fixtures"
)

// The grammars a pool is checked against are the ones the wire writes, pattern for pattern, so
// that the API refuses exactly what a reader of the wire refuses: a looser copy would answer a
// pool the wire rejects, and a narrower one would refuse a pool the documentation prints.
func TestThePoolGrammarsAreTheWiresOwn(t *testing.T) {
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	type pattern struct {
		Pattern    string             `json:"pattern"`
		Properties map[string]pattern `json:"properties"`
	}
	var schema struct {
		Defs map[string]pattern `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	pool := schema.Defs["runnerPool"].Properties["pool"].Properties
	token := schema.Defs["runnerPool"].Properties["join_token"].Properties
	resources := schema.Defs["resources"].Properties

	for _, c := range []struct {
		what string
		ours *regexp.Regexp
		wire string
	}{
		{"a pool's name", givenName, pool["name"].Pattern},
		{"the pool a token names", givenName, token["pool"].Pattern},
		{"a namespace", givenName, schema.Defs["namespace"].Pattern},
		{"a label", labelForm, schema.Defs["label"].Pattern},
		{"a cpu ceiling", cpuForm, resources["cpu"].Pattern},
		{"a memory ceiling", memoryForm, resources["memory"].Pattern},
	} {
		if c.wire == "" {
			t.Errorf("the wire writes no pattern for %s where this test looks for one", c.what)
			continue
		}
		if c.ours.String() != c.wire {
			t.Errorf("%s is checked against %s, and the wire writes %s", c.what, c.ours, c.wire)
		}
	}
}
