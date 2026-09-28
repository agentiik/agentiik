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
	// The corpus the documentation describes: four documents that must be
	// accepted and twelve that must be refused.
	if valid != 4 || invalid != 12 {
		t.Fatalf("the corpus holds %d valid and %d invalid documents, want 4 and 12", valid, invalid)
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

func TestStopsCarriesTheWholeCorpus(t *testing.T) {
	cases, err := Stops()
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
	// A stop that must be accepted, and one that must be refused: a reason written as the task
	// state the stop ends in.
	if valid != 1 || invalid != 1 {
		t.Fatalf("the corpus holds %d valid and %d invalid stops, want 1 and 1", valid, invalid)
	}
}

func TestTaskProgressesCarriesTheWholeCorpus(t *testing.T) {
	cases, err := TaskProgresses()
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
	// Both states progress says, and the two refusals that keep it apart from a result: an
	// ending, and the keyword a result is told apart by.
	if valid != 2 || invalid != 2 {
		t.Fatalf("the corpus holds %d valid and %d invalid progress messages, want 2 and 2", valid, invalid)
	}
}

func TestRunnerHeartbeatsCarriesTheWholeCorpus(t *testing.T) {
	cases, err := RunnerHeartbeats()
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
	// A heartbeat and its answer that must be accepted, and one that must be refused: a key
	// without its attempt.
	if valid != 1 || invalid != 1 {
		t.Fatalf("the corpus holds %d valid and %d invalid heartbeats, want 1 and 1", valid, invalid)
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
	// The corpus the release carries: eleven documents that must be accepted and
	// fifty-nine that must be refused, of which fourteen are rules no JSON Schema can
	// express and the evaluator owns. The included file carrying mcp is the fragment
	// group's now, since it is not an entry point.
	if valid != 11 || invalid != 59 || byValidator != 14 {
		t.Fatalf("the corpus holds %d valid and %d invalid documents, %d of them the validator's, want 11, 59 and 14", valid, invalid, byValidator)
	}
}

func TestTheCorporaOfSeveralDocumentsCarryTheWholeCorpus(t *testing.T) {
	for _, c := range []struct {
		what           string
		read           func() ([]Case, error)
		valid, invalid int
	}{
		// Hidden blocks at the root, and every key a fragment may carry; refused, each of the
		// nine keys that make a document an entry point, and a key nothing defines.
		{"fragments", Fragments, 2, 10},
		// The resolution order, depth-first extends, merged scripts, a hidden step, a step of an
		// included file over defaults, a workflow include at a tag and a stored commit; refused,
		// every rule a pre-receive hook refuses a tree by.
		{"repository cases", Repositories, 7, 20},
		// The documentation's workflow as its version resolves it; refused, six records the
		// wire does not carry.
		{"resolved graphs", ResolvedGraphs, 1, 6},
	} {
		cases, err := c.read()
		if err != nil {
			t.Fatal(err)
		}
		var valid, invalid int
		for _, f := range cases {
			if f.Valid {
				valid++
				if f.Covers == "" {
					t.Errorf("%s says nothing about what it covers", f.File)
				}
				continue
			}
			invalid++
			if f.Rule == "" {
				t.Errorf("%s names no rule it is refused by", f.File)
			}
		}
		if valid != c.valid || invalid != c.invalid {
			t.Errorf("the corpus holds %d valid and %d invalid %s, want %d and %d", valid, invalid, c.what, c.valid, c.invalid)
		}
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
	if valid != 5 || invalid != 16 {
		t.Fatalf("the corpus holds %d valid and %d invalid manifests, want 5 and 16", valid, invalid)
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

func TestTheAccessCorporaCarryTheWholeCorpus(t *testing.T) {
	for _, c := range []struct {
		what           string
		read           func() ([]Case, error)
		valid, invalid int
	}{
		// A viewer on a namespace, an operator on one workflow with an expiry, a deny of
		// run:read_data on that workflow, which is the documentation's example, and an owner
		// grant the installation wrote; refused, a row carrying a role and a deny, a deny naming
		// a role, a role that is not one of the four, a scope on a reserved word, and a
		// namespace of 256 characters.
		{"access grants", AccessGrants, 4, 5},
		// A login, a group and a service account, and a group and a service account at 255
		// characters; refused, operator, installation, a user written with a prefix, a group
		// written in capitals, and a group and a service account of 256 characters.
		{"principal references", PrincipalRefs, 5, 6},
		// operator and owner; refused, admin.
		{"roles", Roles, 2, 1},
		// run:read_data and grant:manage; refused, a hyphenated one and a wildcard.
		{"permissions", Permissions, 2, 2},
	} {
		cases, err := c.read()
		if err != nil {
			t.Fatal(err)
		}
		var valid, invalid int
		for _, f := range cases {
			if f.Valid {
				valid++
				continue
			}
			invalid++
			if f.Rule == "" {
				t.Errorf("%s names no rule it is refused by", f.File)
			}
		}
		if valid != c.valid || invalid != c.invalid {
			t.Errorf("the corpus holds %d valid and %d invalid %s, want %d and %d", valid, invalid, c.what, c.valid, c.invalid)
		}
	}
}
