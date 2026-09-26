package access

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/fixtures"
)

// wireDefs reads the vendored wire schema's $defs, as much of each as these tests look at.
func wireDefs(t *testing.T) map[string]wireDef {
	t.Helper()
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]wireDef `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	return schema.Defs
}

type wireDef struct {
	Enum    []string  `json:"enum"`
	Pattern string    `json:"pattern"`
	Ref     string    `json:"$ref"`
	OneOf   []wireDef `json:"oneOf"`
	Not     *wireDef  `json:"not"`
	Const   string    `json:"const"`
}

// The vocabulary is the wire's, word for word and in its order: a permission the wire does not
// know is one no client can send, and one the wire knows and this package does not is one the API
// would refuse to grant.
func TestTheVocabularyIsTheWiresOwn(t *testing.T) {
	defs := wireDefs(t)

	var permissions []string
	for _, p := range Permissions {
		permissions = append(permissions, string(p))
	}
	if want := defs["permission"].Enum; len(want) == 0 || !slices.Equal(permissions, want) {
		t.Errorf("the permissions are %v, and the wire's are %v", permissions, want)
	}

	var roles []string
	for _, r := range Roles {
		roles = append(roles, string(r))
	}
	if want := defs["role"].Enum; len(want) == 0 || !slices.Equal(roles, want) {
		t.Errorf("the roles are %v, and the wire's are %v", roles, want)
	}
}

// A scope is read on the wire's grammars, pattern for pattern, so that this package refuses what
// a reader of the wire refuses: a looser copy would resolve a grant no one could write, and a
// narrower one would refuse a grant the documentation prints.
func TestTheScopeGrammarsAreTheWiresOwn(t *testing.T) {
	defs := wireDefs(t)
	scope := defs["grantScope"].OneOf
	if len(scope) != 2 {
		t.Fatalf("the wire's grantScope has %d forms, and this package reads two", len(scope))
	}
	if scope[0].Ref != "#/$defs/namespace" {
		t.Errorf("the wire's first form of a scope is %q, and this package reads a namespace", scope[0].Ref)
	}
	if got, want := namespaceForm.String(), defs["namespace"].Pattern; got != want {
		t.Errorf("a namespace is read as %s, and the wire writes %s", got, want)
	}
	if got, want := `^`+namespaceGrammar+`/`+workflowGrammar+`$`, scope[1].Pattern; got != want {
		t.Errorf("a workflow's scope is read as %s, and the wire writes %s", got, want)
	}
}

// A principal is read on the wire's three forms, pattern for pattern: a login is a namespace's name
// and never operator, a group is group:NAME and a service account NS/NAME, both on the namespace
// grammar.
func TestThePrincipalGrammarsAreTheWiresOwn(t *testing.T) {
	defs := wireDefs(t)
	forms := defs["principalRef"].OneOf
	if len(forms) != 3 || forms[0].Ref != "#/$defs/login" || forms[1].Ref != "#/$defs/groupRef" || forms[2].Ref != "#/$defs/serviceAccountRef" {
		t.Fatalf("the wire's principalRef is %+v, and this package reads a login, a group and a service account", forms)
	}
	if login := defs["login"]; login.Ref != "#/$defs/namespace" || login.Not == nil || login.Not.Const != "operator" {
		t.Errorf("the wire's login is %+v, and this package reads a namespace's name that is not operator", login)
	}
	if got, want := `^group:`+namespaceGrammar+`$`, defs["groupRef"].Pattern; got != want {
		t.Errorf("a group is read as %s, and the wire writes %s", got, want)
	}
	if got, want := `^`+namespaceGrammar+`/`+namespaceGrammar+`$`, defs["serviceAccountRef"].Pattern; got != want {
		t.Errorf("a service account is read as %s, and the wire writes %s", got, want)
	}
}

// Every principal reference the corpus holds valid is one a grant can name, and every one it holds
// invalid is refused.
func TestThePrincipalReferenceCorpus(t *testing.T) {
	cases, err := fixtures.PrincipalRefs()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		b, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		var ref string
		if err := json.Unmarshal(b, &ref); err != nil {
			t.Fatalf("%s: %v", c.File, err)
		}
		err = principalRef(ref)
		if c.Valid && err != nil {
			t.Errorf("%s: %q is refused, and the corpus accepts it as %s: %v", c.File, ref, c.Covers, err)
		}
		if !c.Valid && err == nil {
			t.Errorf("%s: %q is accepted, and the corpus refuses it: %s", c.File, ref, c.Rule)
		}
	}
}

// Every access grant the corpus holds valid reads and validates and is written back as it was
// read, and every one it holds invalid is refused, by reading or by Validate.
func TestTheAccessGrantCorpus(t *testing.T) {
	cases, err := fixtures.AccessGrants()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.File, func(t *testing.T) {
			b, err := fs.ReadFile(fixtures.FS, c.File)
			if err != nil {
				t.Fatal(err)
			}
			var g Grant
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			err = dec.Decode(&g)
			if err == nil {
				err = g.Validate()
			}
			if !c.Valid {
				if err == nil {
					t.Errorf("accepted, and the corpus refuses it: %s", c.Rule)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused, and the corpus accepts it as %s: %v", c.Covers, err)
			}
			out, err := json.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			var was, is any
			if err := json.Unmarshal(b, &was); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(out, &is); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(was, is) {
				t.Errorf("read and written back, it is %s", out)
			}
		})
	}
}

// Every role and every permission the corpus holds valid is one of the four or the nine, and every
// one it holds invalid is not.
func TestTheRoleAndPermissionCorpora(t *testing.T) {
	for _, corpus := range []struct {
		read  func() ([]fixtures.Case, error)
		valid func(string) bool
	}{
		{fixtures.Roles, func(s string) bool { return Role(s).Valid() }},
		{fixtures.Permissions, func(s string) bool { return Permission(s).Valid() }},
	} {
		cases, err := corpus.read()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			b, err := fs.ReadFile(fixtures.FS, c.File)
			if err != nil {
				t.Fatal(err)
			}
			var s string
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatalf("%s: %v", c.File, err)
			}
			if corpus.valid(s) != c.Valid {
				t.Errorf("%s: %q reads as valid %v, and the corpus says %v (%s%s)", c.File, s, !c.Valid, c.Valid, c.Covers, c.Rule)
			}
		}
	}
}

// A scope reads as the wire writes it and writes back the same, and one no grant can name is
// refused both ways.
func TestAScopeReadsAsTheWireWritesIt(t *testing.T) {
	for _, s := range []string{"finance", "team-ops", "finance/monthly-invoicing", "finance/Nightly_Sync", "a/b"} {
		got, err := ParseScope(s)
		if err != nil {
			t.Errorf("%q is refused: %v", s, err)
			continue
		}
		if got.String() != s {
			t.Errorf("%q reads back as %q", s, got)
		}
	}
	for _, s := range []string{
		"", "/", "finance/", "/monthly-invoicing", "Finance", "finance/monthly/invoicing",
		"finance/-invoicing", "runs", "runs/monthly-invoicing", "me", "team--ops", "finance/monthly invoicing",
		strings.Repeat("a", 256), "finance/" + strings.Repeat("a", 256),
	} {
		if got, err := ParseScope(s); err == nil {
			t.Errorf("%.40q is read as %+v", s, got)
		}
	}
	if _, err := (Scope{}).MarshalText(); err == nil {
		t.Error("the installation is written out as a grant's scope")
	}
	if _, err := (Scope{Namespace: "runs", Workflow: "x"}).MarshalText(); err == nil {
		t.Error("a scope on a reserved word is written out")
	}
	var s Scope
	if err := json.Unmarshal([]byte(`"finance/monthly-invoicing"`), &s); err != nil || s != (Scope{Namespace: "finance", Workflow: "monthly-invoicing"}) {
		t.Errorf("read as %+v: %v", s, err)
	}
}
