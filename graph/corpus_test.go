package graph

import (
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/fixtures"
)

// TestTheWorkflowCorpus is where the claim that this reader agrees with the released
// workflow.schema.json is made. It is made here and not in the code: every document the
// corpus declares valid is read and checked, and every document it declares invalid is
// refused, by the reader where the corpus says the schema refuses it and by the validator
// where the corpus says only a validator can.
//
// The fifteen the corpus marks refused_by "validator" are held to the rule they pin,
// which is one lookup because a Rule is spelled the way the corpus names the fixture.
func TestTheWorkflowCorpus(t *testing.T) {
	cases, err := fixtures.Workflows()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(path.Base(c.File), func(t *testing.T) {
			doc := read(t, c.File)

			// One fixture is the included file itself, which is where the rule it
			// pins can be seen at all: a fragment is not an entry point and is never
			// read as one.
			if c.Role != "" {
				_, err := ParseFragment(doc)
				held(t, err, ruleOf(c.File))
				return
			}

			wf, err := Parse(doc)
			switch {
			case c.Valid:
				if err != nil {
					t.Fatalf("the corpus says this document covers %s, and it was refused when read: %v", c.Covers, err)
				}
				if err := Check(wf); err != nil {
					t.Fatalf("the corpus says this document covers %s, and it was refused when checked: %v", c.Covers, err)
				}
			case c.RefusedBy == "schema":
				if err == nil {
					t.Fatalf("this document was read without complaint, and the corpus refuses it by its shape: %s", c.Rule)
				}
			default:
				// A validator fixture is schema valid on purpose: it has to be read
				// before the rule it pins can be reached at all.
				if err != nil {
					t.Fatalf("the corpus says this document is schema valid and it was refused when read: %v", err)
				}
				held(t, refusal(t, wf, c.Manifest), ruleOf(c.File))
			}
		})
	}
}

// refusal runs the layer that can reach the rule: Check where the workflow file is the
// whole of it, and Build where the corpus names a brick manifest, because "the rule it
// pins is about the two documents together, so neither file alone shows it".
func refusal(t *testing.T, wf *Workflow, manifest string) error {
	t.Helper()
	if manifest == "" {
		return Check(wf)
	}
	m, err := brick.ParseManifest(read(t, manifest))
	if err != nil {
		t.Fatalf("reading the manifest the fixture is checked against: %v", err)
	}
	manifests := map[string]brick.Manifest{}
	for _, image := range Images(wf) {
		manifests[image] = m
	}
	_, err = Build(wf, manifests)
	return err
}

// held says that an error is the refusal the corpus names, and nothing else.
func held(t *testing.T, err error, rule Rule) {
	t.Helper()
	if err == nil {
		t.Fatalf("this document was accepted, and the corpus refuses it by %s", rule)
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("this document was refused by something that is not a refusal of the language: %v", err)
	}
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("this document was refused without a rule: %v", err)
	}
	if r.Rule != rule {
		t.Fatalf("this document was refused by %s, and the corpus pins it to %s: %v", r.Rule, rule, err)
	}
}

// ruleOf is the rule a fixture pins, which is the name the corpus files it under. Rule
// values are spelled that way so that holding a fixture to its rule is one lookup.
func ruleOf(file string) Rule {
	return Rule(strings.TrimSuffix(path.Base(file), ".yaml"))
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	doc, err := fs.ReadFile(fixtures.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
