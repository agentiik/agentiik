package graph

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/fixtures"
)

// Grants stay out of the workflow file. "Grants live in the platform database and are managed
// through the API and its clients, never in the workflow YAML. In the file, workflow:write would
// equal grant:manage: an editor could make themselves owner in the same commit. It also keeps the
// file portable between installations." So no keyword of the language confers access, and nothing
// the evaluator reads out of a file could: the words access is written in appear nowhere in the
// published workflow schema, a file writing one is refused, and no field of a parsed workflow or of
// its graph is named for access or holds a type of package access.

// accessWords are the words a keyword conferring access would be spelled with: the grant's own
// fields, its principals, the four roles and the nine permissions. None is a keyword of the
// language, and none of the roles or permissions is a value one takes.
var accessWords = []string{
	"grant", "grants", "deny", "denies", "permission", "permissions", "role", "roles",
	"principal", "principals", "owner", "owners", "share", "shares", "sharing", "access", "acl", "acls",
	"admin", "admins", "administrator", "administrators", "members",
	"viewer", "operator", "editor",
	"workflow:read", "workflow:run", "workflow:write", "workflow:delete",
	"run:read", "run:read_data", "secret:use", "secret:write", "grant:manage",
}

// A keyword of the published workflow schema, at any depth, or a value it enumerates, is never a
// word access is written in.
func TestNoKeywordOfTheWorkflowFileConfersAccess(t *testing.T) {
	doc, err := fs.ReadFile(fixtures.FS, "workflow.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	var keywords, values []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, sub := range v {
				switch k {
				case "properties", "patternProperties", "$defs":
					if names, ok := sub.(map[string]any); ok {
						for name := range names {
							keywords = append(keywords, name)
						}
					}
				case "enum":
					if list, ok := sub.([]any); ok {
						for _, e := range list {
							if s, ok := e.(string); ok {
								values = append(values, s)
							}
						}
					}
				case "const":
					if s, ok := sub.(string); ok {
						values = append(values, s)
					}
				case "description", "examples", "$comment", "title":
					// Prose may say what access is: it confers none.
					continue
				}
				walk(sub)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(schema)
	// A schema read to nothing would pass whatever it said.
	if len(keywords) < 50 || !slices.Contains(keywords, "steps") || !slices.Contains(keywords, "secrets") {
		t.Fatalf("the workflow schema was read to %d keywords, which is not the language", len(keywords))
	}
	for _, k := range keywords {
		if slices.Contains(accessWords, strings.ToLower(k)) {
			t.Errorf("the workflow schema has a keyword %q: a grant written in the file would be one workflow:write could write, which is grant:manage", k)
		}
	}
	for _, v := range values {
		if slices.Contains(accessWords, strings.ToLower(v)) {
			t.Errorf("the workflow schema takes the value %q, which names a role or a permission", v)
		}
	}
}

// A file that writes access in, at the root, under metadata, a trigger or a step, is refused, as
// any keyword the language does not know is: it is not read and set aside. Each is refused for that
// line alone, since the file without it reads.
func TestAFileThatWritesAccessInIsRefused(t *testing.T) {
	webhook := strings.Replace(minimal, "steps:", "on:\n  webhook:\n    - path: /invoice\nsteps:", 1)
	for what, c := range map[string]struct{ doc, line, in string }{
		"grants at the root":       {minimal, "steps:", "grants:\n  - principal: alice\n    role: owner\nsteps:"},
		"access at the root":       {minimal, "steps:", "access:\n  alice: owner\nsteps:"},
		"an owner in metadata":     {minimal, "  name: nightly-reconciliation", "  name: nightly-reconciliation\n  owner: alice"},
		"permissions on a step":    {minimal, "    outputs: [out]", "    outputs: [out]\n    permissions: [grant:manage]"},
		"a principal on a webhook": {webhook, "    - path: /invoice", "    - path: /invoice\n      principal: alice"},
	} {
		parsed(t, c.doc)
		if _, err := Parse([]byte(strings.Replace(c.doc, c.line, c.in, 1))); err == nil {
			t.Errorf("a file with %s was read", what)
		}
	}
}

// Nothing the language parses a file into, or builds a graph from, has a field named for access or
// of a type of package access, so no rule the evaluator follows can have been handed one.
func TestNothingParsedFromAFileHoldsAccess(t *testing.T) {
	const module = "github.com/agentiik/agentiik/"
	seen := map[reflect.Type]bool{}
	var fields int
	var walk func(t reflect.Type, path string)
	walk = func(ty reflect.Type, path string) {
		if seen[ty] {
			return
		}
		seen[ty] = true
		if strings.HasPrefix(ty.PkgPath(), module+"access") {
			t.Errorf("%s is of type %s, from package access", path, ty)
			return
		}
		switch ty.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(ty.Elem(), path)
		case reflect.Map:
			walk(ty.Key(), path+"[key]")
			walk(ty.Elem(), path+"[]")
		case reflect.Struct:
			// A type of another module, the YAML parser's syntax tree among them, is the
			// file as text and not what the language makes of it.
			if ty.PkgPath() != "" && !strings.HasPrefix(ty.PkgPath(), module) {
				return
			}
			for i := range ty.NumField() {
				f := ty.Field(i)
				fields++
				if slices.Contains(accessWords, strings.ToLower(f.Name)) {
					t.Errorf("%s.%s is named for access", path, f.Name)
				}
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeFor[Workflow](), "Workflow")
	walk(reflect.TypeFor[Graph](), "Graph")
	walk(reflect.TypeFor[Task](), "Task")
	if fields < 50 {
		t.Fatalf("walked %d fields, which is not what a workflow holds", fields)
	}
}
