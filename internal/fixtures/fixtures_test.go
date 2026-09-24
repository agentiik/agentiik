package fixtures

import (
	"io/fs"
	"strings"
	"testing"
)

func TestVersionMatchesTheVendoredTree(t *testing.T) {
	b, err := fs.ReadFile(FS, "SCHEMAS_VERSION")
	if err != nil {
		t.Fatalf("reading the pin: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != Version {
		t.Fatalf("the tree is pinned at %q and the package says %q", got, Version)
	}
}

func TestEnvelopesCarriesTheWholeCorpus(t *testing.T) {
	cases, err := Envelopes()
	if err != nil {
		t.Fatal(err)
	}

	var valid, invalid int
	for _, c := range cases {
		switch {
		case c.Valid:
			valid++
			if c.Covers == "" {
				t.Errorf("%s says nothing about what it covers", c.File)
			}
		default:
			invalid++
			if c.Rule == "" {
				t.Errorf("%s names no rule it is refused by", c.File)
			}
		}
	}
	// The corpus the documentation describes: three documents that must be
	// accepted and eleven that must be refused.
	if valid != 3 || invalid != 11 {
		t.Fatalf("the corpus holds %d valid and %d invalid documents, want 3 and 11", valid, invalid)
	}
}

func TestGrantRedemptionsCarriesTheWholeCorpus(t *testing.T) {
	cases, err := GrantRedemptions()
	if err != nil {
		t.Fatal(err)
	}
	var valid, invalid int
	for _, c := range cases {
		if c.Valid {
			valid++
			continue
		}
		invalid++
		if c.Rule == "" {
			t.Errorf("%s names no rule it is refused by", c.File)
		}
	}
	// Two exchanges that must be accepted, one of them a secret mounted at a file name
	// carrying a dot, and two that must be refused: a tree carrying a mode that is not octal,
	// and a secret mounted at the parent of /agk/secrets/.
	if valid != 2 || invalid != 2 {
		t.Fatalf("the corpus holds %d valid and %d invalid redemptions, want 2 and 2", valid, invalid)
	}
}

func TestRunnerPoolsCarriesTheWholeCorpus(t *testing.T) {
	cases, err := RunnerPools()
	if err != nil {
		t.Fatal(err)
	}
	var valid, invalid int
	for _, c := range cases {
		if c.Valid {
			valid++
			continue
		}
		invalid++
		if c.Rule == "" {
			t.Errorf("%s names no rule it is refused by", c.File)
		}
	}
	// A pool and the token issued from it that must be accepted, and one that must be
	// refused: a token permitting a label with no value.
	if valid != 1 || invalid != 1 {
		t.Fatalf("the corpus holds %d valid and %d invalid pools, want 1 and 1", valid, invalid)
	}
}

func TestRunnerRegistrationsCarriesTheWholeCorpus(t *testing.T) {
	cases, err := RunnerRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	var valid, invalid int
	for _, c := range cases {
		if c.Valid {
			valid++
			continue
		}
		invalid++
		if c.Rule == "" {
			t.Errorf("%s names no rule it is refused by", c.File)
		}
	}
	// A join and its answer that must be accepted, and one that must be refused: a join
	// carrying the private half of the host's keypair.
	if valid != 1 || invalid != 1 {
		t.Fatalf("the corpus holds %d valid and %d invalid registrations, want 1 and 1", valid, invalid)
	}
}

func TestTheEnvelopeSchemaIsVendoredWithIt(t *testing.T) {
	b, err := fs.ReadFile(FS, "envelope.schema.json")
	if err != nil {
		t.Fatalf("reading the schema: %v", err)
	}
	if !strings.Contains(string(b), `"$id": "https://schemas.agentiik.dev/envelope.schema.json"`) {
		t.Fatal("the vendored envelope.schema.json is not the released one")
	}
}

func TestWorkflowsCarriesTheWholeCorpus(t *testing.T) {
	cases, err := Workflows()
	if err != nil {
		t.Fatal(err)
	}

	var valid, invalid, byValidator int
	for _, c := range cases {
		switch {
		case c.Valid:
			valid++
			if c.Covers == "" {
				t.Errorf("%s says nothing about what it covers", c.File)
			}
		default:
			invalid++
			if c.Rule == "" {
				t.Errorf("%s names no rule it is refused by", c.File)
			}
			switch c.RefusedBy {
			case "schema":
			case "validator":
				byValidator++
			default:
				t.Errorf("%s says it is refused by %q, which is neither the schema nor the validator", c.File, c.RefusedBy)
			}
		}
	}
	// The corpus the release carries: nine documents that must be accepted and
	// fifty-three that must be refused, of which fifteen are rules no JSON Schema can
	// express and the evaluator owns.
	if valid != 9 || invalid != 53 || byValidator != 15 {
		t.Fatalf("the corpus holds %d valid and %d invalid documents, %d of them the validator's, want 9, 53 and 15", valid, invalid, byValidator)
	}
}

func TestBricksCarriesTheWholeCorpus(t *testing.T) {
	cases, err := Bricks()
	if err != nil {
		t.Fatal(err)
	}

	var valid, invalid int
	for _, c := range cases {
		if c.Valid {
			valid++
			continue
		}
		invalid++
		if c.Rule == "" {
			t.Errorf("%s names no rule it is refused by", c.File)
		}
	}
	if valid != 4 || invalid != 15 {
		t.Fatalf("the corpus holds %d valid and %d invalid manifests, want 4 and 15", valid, invalid)
	}
}

func TestTheWorkflowAndBrickSchemasAreVendoredWithTheirFixtures(t *testing.T) {
	for _, name := range []string{"workflow.schema.json", "brick.schema.json"} {
		b, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatalf("reading the schema: %v", err)
		}
		if !strings.Contains(string(b), `"$id": "https://schemas.agentiik.dev/`+name+`"`) {
			t.Fatalf("the vendored %s is not the released one", name)
		}
	}
}
