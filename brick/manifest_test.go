package brick_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/agentiik/agentiik/schema"
)

// TestTheBrickCorpus holds this reader to the released corpus: every manifest the
// corpus declares valid is read, and every manifest it declares invalid is refused. The
// corpus names the rule each one pins, so a failure here says which rule went missing
// rather than which line of this package changed.
func TestTheBrickCorpus(t *testing.T) {
	cases, err := fixtures.Bricks()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.File, func(t *testing.T) {
			doc, err := fs.ReadFile(fixtures.FS, c.File)
			if err != nil {
				t.Fatal(err)
			}
			m, err := brick.ParseManifest(doc)
			switch {
			case c.Valid && err != nil:
				t.Fatalf("the corpus says this manifest covers %s, and it was refused: %v", c.Covers, err)
			case c.Valid:
				if m.APIVersion != "agentiik.dev/v1" || m.Kind != "Brick" {
					t.Fatalf("read %q and %q where the manifest says agentiik.dev/v1 and Brick", m.APIVersion, m.Kind)
				}
			case err == nil:
				t.Fatalf("this manifest was accepted, and the corpus refuses it: %s", c.Rule)
			}
		})
	}
}

// TestAPortSchemaResolvesAgainstTheManifest holds the rule about pointers: a definition
// written under spec.definitions is named #/spec/definitions/<name> and resolved against
// the manifest document itself. It is the one rule that only shows when the manifest and
// a JSON Schema implementation are put together, which is why the test stands here and
// the compiler stands in package schema.
func TestAPortSchemaResolvesAgainstTheManifest(t *testing.T) {
	m := parse(t, "fixtures/brick/valid/normalize.yaml")

	if got := m.InputPorts(); len(got) != 2 || got[0] != "customers" || got[1] != "orders" {
		t.Fatalf("the manifest declares the input ports %v", got)
	}
	if got := m.OutputPorts(); len(got) != 2 || got[0] != "ok" || got[1] != "rejected" {
		t.Fatalf("the manifest declares the output ports %v", got)
	}

	s, err := schema.NewCompiler(nil).CompileAt(m.Document(), "#/spec/outputs/ok/schema")
	if err != nil {
		t.Fatalf("compiling the schema of the ok port: %v", err)
	}
	if err := s.Validate(map[string]any{"customer_id": "c-1"}); err != nil {
		t.Fatalf("an order the definition accepts was refused: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Fatal("an order carrying no customer_id was accepted, so #/spec/definitions/order never resolved")
	}
}

// TestAPortWithoutASchemaAcceptsWhatArrives holds the sentence that says so: the schema
// of a port is optional, and a port declared without one is what a passthrough brick
// says.
func TestAPortWithoutASchemaAcceptsWhatArrives(t *testing.T) {
	m := parse(t, "fixtures/brick/valid/normalize.yaml")

	rejected, ok := m.Port("rejected")
	if !ok {
		t.Fatal("the manifest declares a rejected port and it was not read")
	}
	if rejected.Schema != nil {
		t.Fatalf("the rejected port carries the schema %s, and the manifest declares none", rejected.Schema)
	}
}

// TestTheLanguagesRequiredIsNotJSONSchemas holds the decision recorded beside the
// parameter table: required: true written on the parameter is the language's own
// keyword, and what is left has to be a document a JSON Schema implementation compiles.
func TestTheLanguagesRequiredIsNotJSONSchemas(t *testing.T) {
	m := parse(t, "fixtures/brick/valid/normalize.yaml")

	currency, ok := m.Spec.Params["currency"]
	if !ok {
		t.Fatal("the manifest declares a currency parameter and it was not read")
	}
	if !currency.Required {
		t.Fatal("the parameter is declared required: true and was read as optional")
	}
	if strings.Contains(string(currency.Schema), "required") {
		t.Fatalf("the parameter schema kept the language's own keyword: %s", currency.Schema)
	}

	s, err := schema.NewCompiler(nil).CompileAt(m.Document(), "#/spec/params/currency")
	if err != nil {
		t.Fatalf("compiling the parameter schema out of the manifest: %v", err)
	}
	if err := s.Validate("EUR"); err != nil {
		t.Fatalf("a currency the schema accepts was refused: %v", err)
	}
	if err := s.Validate("euro"); err == nil {
		t.Fatal("a currency the pattern refuses was accepted")
	}
}

// TestTheArrayFormOfRequiredKeepsItsMeaning is the other half of the same decision: on
// a parameter that is itself an object, required names the members the value must carry
// and is JSON Schema's own.
func TestTheArrayFormOfRequiredKeepsItsMeaning(t *testing.T) {
	const doc = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: http-request
  version: 1.0.0
spec:
  params:
    target:
      type: object
      required: [url]
      properties: { url: { type: string } }
  runtime:
    user: "65532:65532"
`
	m, err := brick.ParseManifest([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	target := m.Spec.Params["target"]
	if target.Required {
		t.Fatal("the array form was read as the language's own keyword")
	}

	s, err := schema.NewCompiler(nil).CompileAt(m.Document(), "#/spec/params/target")
	if err != nil {
		t.Fatalf("compiling the parameter schema: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Fatal("a value carrying no url was accepted, so the array form was dropped")
	}
}

// TestTheManifestIsClosedWhereItIsTheEngines and the test below it are the two halves of
// the sentence about what a manifest may carry: closed wherever the document is its own,
// open wherever the author is writing JSON Schema.
func TestTheManifestIsClosedWhereItIsTheEngines(t *testing.T) {
	const doc = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: normalize
  version: 1.0.0
spec:
  outputs:
    ok:
      schema: { type: object }
      description: what came through
  runtime:
    user: "65532:65532"
`
	if _, err := brick.ParseManifest([]byte(doc)); err == nil {
		t.Fatal("a description written beside a port was accepted, and a port carries schema and nothing else")
	}
}

func TestTheManifestIsOpenWhereTheAuthorWritesJSONSchema(t *testing.T) {
	const doc = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata:
  name: normalize
  version: 1.0.0
spec:
  outputs:
    ok:
      schema:
        type: object
        title: An order
        unevaluatedProperties: false
  runtime:
    user: "65532:65532"
`
	if _, err := brick.ParseManifest([]byte(doc)); err != nil {
		t.Fatalf("a keyword inside the author's own schema was refused: %v", err)
	}
}

// TestTheGroupHalfOfTheAccountIsNotRead holds the exception the documentation writes
// out: root, 0 and 0:0 are refused, and nonroot:0 is accepted.
func TestTheGroupHalfOfTheAccountIsNotRead(t *testing.T) {
	for user, accepted := range map[string]bool{
		"nonroot:0":    true,
		"65532:65532":  true,
		"nonroot":      true,
		"root":         false,
		"0":            false,
		"0:0":          false,
		"00":           false,
		"root:nonroot": false,
	} {
		doc := `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: normalize, version: 1.0.0 }
spec:
  runtime:
    user: "` + user + `"
`
		_, err := brick.ParseManifest([]byte(doc))
		if accepted && err != nil {
			t.Errorf("the account %q was refused: %v", user, err)
		}
		if !accepted && err == nil {
			t.Errorf("the account %q was accepted", user)
		}
	}
}

// A name the manifest writes is at most 255 characters, the longest a file name is: a
// port becomes a file under /agk/out/ports/ and a secret, unless it says otherwise, a
// file under /agk/secrets/.
func TestANameIsNoLongerThanAFileName(t *testing.T) {
	longest := strings.Repeat("n", 255)
	manifest := func(port, secret string) []byte {
		return []byte(`
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: normalize, version: 1.0.0 }
spec:
  outputs:
    ` + port + `: {}
  secrets:
    - name: ` + secret + `
  runtime:
    user: "65532:65532"
`)
	}
	if _, err := brick.ParseManifest(manifest(longest, longest)); err != nil {
		t.Errorf("a port and a secret of 255 characters were refused: %v", err)
	}
	for what, doc := range map[string][]byte{
		"a port":   manifest(longest+"n", "billing"),
		"a secret": manifest("ok", longest+"n"),
	} {
		_, err := brick.ParseManifest(doc)
		if err == nil {
			t.Errorf("%s of 256 characters was accepted", what)
			continue
		}
		if said := err.Error(); !strings.Contains(said, "at most 255") {
			t.Errorf("%s of 256 characters was refused with %q", what, said)
		}
	}
}

func parse(t *testing.T, name string) brick.Manifest {
	t.Helper()
	doc, err := fs.ReadFile(fixtures.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := brick.ParseManifest(doc)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
