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

func TestTheEnvelopeSchemaIsVendoredWithIt(t *testing.T) {
	b, err := fs.ReadFile(FS, "envelope.schema.json")
	if err != nil {
		t.Fatalf("reading the schema: %v", err)
	}
	if !strings.Contains(string(b), `"$id": "https://schemas.agentiik.dev/envelope.schema.json"`) {
		t.Fatal("the vendored envelope.schema.json is not the released one")
	}
}
