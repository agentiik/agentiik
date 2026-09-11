package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// tree is the repository tree a workflow travelled with: schemas beside the entry point,
// referenced as ./schemas/<name>.json.
func tree() fstest.MapFS {
	return fstest.MapFS{
		"schemas/order.json": &fstest.MapFile{Data: []byte(`{
			"type": "object",
			"required": ["id", "amount"],
			"properties": {
				"id": { "type": "string" },
				"amount": { "type": "number", "minimum": 0 }
			}
		}`)},
		"schemas/customer.json": &fstest.MapFile{Data: []byte(`{
			"type": "array",
			"items": { "$ref": "./order.json" }
		}`)},
		"schemas/broken.json": &fstest.MapFile{Data: []byte(`{ "type": `)},
	}
}

// decode reads a value the way a trigger body arrives, so that numbers are json.Number
// exactly as they are on the wire.
func decode(t *testing.T, s string) any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

func TestCompileAndValidate(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		value string
		// want is the substring the refusal has to name, or empty when the value passes.
		want string
	}{
		{
			name:  "inline object schema accepts a conforming value",
			doc:   `{ "type": "object", "properties": { "id": { "type": "string" } } }`,
			value: `{ "id": "a" }`,
		},
		{
			name:  "inline object schema names the keyword it refused by",
			doc:   `{ "type": "object", "properties": { "id": { "type": "string" } } }`,
			value: `{ "id": 1 }`,
			want:  "/id: got number, want string",
		},
		{
			name:  "true accepts every value, which is what a passthrough says",
			doc:   `true`,
			value: `{ "anything": [1, 2] }`,
		},
		{
			name:  "false accepts no value at all",
			doc:   `false`,
			value: `{}`,
			want:  "false schema",
		},
		{
			name:  "a reference resolves against the repository tree",
			doc:   `{ "$ref": "./schemas/order.json" }`,
			value: `{ "id": "o-1", "amount": 12 }`,
		},
		{
			name:  "a referenced schema refuses, naming the instance location",
			doc:   `{ "$ref": "./schemas/order.json" }`,
			value: `{ "id": "o-1", "amount": -1 }`,
			want:  "/amount: minimum: got -1, want 0",
		},
		{
			name:  "a reference inside a referenced schema resolves too",
			doc:   `{ "$ref": "./schemas/customer.json" }`,
			value: `[{ "id": "o-1", "amount": 0 }]`,
		},
		{
			name:  "a reference from the root of the tree resolves",
			doc:   `{ "$ref": "/schemas/order.json" }`,
			value: `{ "id": "o-1", "amount": 1 }`,
		},
		{
			name:  "every departure is named, not only the first",
			doc:   `{ "$ref": "./schemas/order.json" }`,
			value: `{ "amount": -1 }`,
			want:  "missing property 'id'; /amount: minimum: got -1, want 0",
		},
		{
			name:  "a large integer bound keeps the number the author wrote",
			doc:   `{ "type": "integer", "maximum": 9007199254740993 }`,
			value: `9007199254740993`,
		},
	}

	c := NewCompiler(tree())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := c.Compile([]byte(tc.doc))
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			err = s.Validate(decode(t, tc.value))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate: %v, want no refusal", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("Validate: no refusal, want one naming %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Validate: %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// A refusal is read in a log beside the input it refused, so it is one line.
func TestValidateRefusalIsOneLine(t *testing.T) {
	c := NewCompiler(tree())
	s, err := c.Compile([]byte(`{ "$ref": "./schemas/order.json" }`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	err = s.Validate(decode(t, `{ "amount": -1 }`))
	if err == nil {
		t.Fatal("Validate: no refusal, want one")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("Validate: %q spans more than one line", err)
	}
	if strings.Contains(err.Error(), treeBase) {
		t.Fatalf("Validate: %q names the synthetic base URI, which means nothing to a reader", err)
	}
}

func TestCompileRefuses(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "a reference to the network",
			doc:  `{ "$ref": "https://json.schemastore.org/order.json" }`,
			want: "leaves the repository tree",
		},
		{
			name: "a reference to the runner's own filesystem",
			doc:  `{ "$ref": "file:///etc/passwd" }`,
			want: "leaves the repository tree",
		},
		{
			name: "a reference to another tree",
			doc:  `{ "$ref": "agk://elsewhere/schemas/order.json" }`,
			want: "leaves the repository tree",
		},
		{
			name: "a reference climbing out of the tree",
			doc:  `{ "$ref": "../../etc/passwd" }`,
			want: "not in the repository tree",
		},
		{
			name: "a reference to a file the commit does not carry",
			doc:  `{ "$ref": "./schemas/absent.json" }`,
			want: "not in the repository tree",
		},
		{
			name: "a referenced file that is not JSON",
			doc:  `{ "$ref": "./schemas/broken.json" }`,
			want: "schemas/broken.json",
		},
		{
			name: "a schema document that is not JSON",
			doc:  `{ "type": `,
			want: "not a JSON document",
		},
		{
			name: "a schema document that is not a schema",
			doc:  `["type", "object"]`,
			want: "does not compile",
		},
		{
			name: "a keyword whose value the draft does not allow",
			doc:  `{ "properties": { "id": 3 } }`,
			want: "does not compile",
		},
	}

	c := NewCompiler(tree())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Compile([]byte(tc.doc))
			if err == nil {
				t.Fatalf("Compile: accepted, want a refusal naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile: %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// A run given no tree cannot be holding the file a reference names.
func TestCompileWithoutATreeRefusesEveryReference(t *testing.T) {
	c := NewCompiler(nil)

	if _, err := c.Compile([]byte(`{ "type": "object" }`)); err != nil {
		t.Fatalf("Compile an inline schema: %v", err)
	}

	_, err := c.Compile([]byte(`{ "$ref": "./schemas/order.json" }`))
	if err == nil {
		t.Fatal("Compile: accepted a reference, want a refusal")
	}
	if !strings.Contains(err.Error(), "no repository tree") {
		t.Fatalf("Compile: %q, want it to say there is no repository tree", err)
	}
}

// An inline schema is compiled under a base URI that names no file, so a workflow
// cannot shadow a committed schema by writing one inline.
func TestCompileDoesNotShadowTheTree(t *testing.T) {
	fsys := tree()
	fsys["repo"] = &fstest.MapFile{Data: []byte(`{ "type": "null" }`)}
	fsys[""] = &fstest.MapFile{Data: []byte(`{ "type": "null" }`)}

	c := NewCompiler(fsys)
	s, err := c.Compile([]byte(`{ "type": "object" }`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if err := s.Validate(decode(t, `{}`)); err != nil {
		t.Fatalf("Validate: %v, want the inline schema to be the one that applied", err)
	}
}

// Two compilations share no state, so one workflow's schema cannot collide with
// another's and a Compiler can be handed to more than one run at a time.
func TestCompileIsRepeatable(t *testing.T) {
	c := NewCompiler(tree())
	for i := 0; i < 3; i++ {
		if _, err := c.Compile([]byte(`{ "$ref": "./schemas/order.json" }`)); err != nil {
			t.Fatalf("Compile %d: %v", i, err)
		}
	}
}

func TestCompileIsSafeForConcurrentUse(t *testing.T) {
	c := NewCompiler(tree())
	done := make(chan error, 8)
	for i := 0; i < cap(done); i++ {
		go func() {
			_, err := c.Compile([]byte(`{ "$ref": "./schemas/customer.json" }`))
			done <- err
		}()
	}
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Fatalf("Compile: %v", err)
		}
	}
}

// TestCompileAtResolvesAgainstTheWholeDocument holds what a brick manifest needs: a
// port schema written as { "$ref": "#/spec/definitions/order" } resolves against the
// manifest, and not against the fragment the port carries.
func TestCompileAtResolvesAgainstTheWholeDocument(t *testing.T) {
	const manifest = `{
	  "apiVersion": "agentiik.dev/v1",
	  "kind": "Brick",
	  "spec": {
	    "outputs": { "ok": { "schema": { "$ref": "#/spec/definitions/order" } } },
	    "definitions": {
	      "order": { "type": "object", "required": ["customer_id"] }
	    }
	  }
	}`

	c := NewCompiler(nil)
	s, err := c.CompileAt([]byte(manifest), "#/spec/outputs/ok/schema")
	if err != nil {
		t.Fatalf("compiling the port schema: %v", err)
	}
	if err := s.Validate(map[string]any{"customer_id": "c-1"}); err != nil {
		t.Fatalf("an order the definition accepts was refused: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Fatal("an order carrying no customer_id was accepted, so the reference never resolved")
	}
}

// TestCompileAtRefusesAPointerThatIsNotOne keeps the pointer in the form the manifest
// writes it: it names a place in the document it was written in.
func TestCompileAtRefusesAPointerThatIsNotOne(t *testing.T) {
	c := NewCompiler(nil)
	if _, err := c.CompileAt([]byte(`{}`), "/spec/definitions/order"); err == nil {
		t.Fatal("a pointer written without its leading # was accepted")
	}
}
