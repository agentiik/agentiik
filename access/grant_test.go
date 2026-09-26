package access_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
)

// Validate refuses a grant that does not say what it binds, and says which part it could not read.
func TestAGrantSaysWhatItBinds(t *testing.T) {
	for _, c := range []struct {
		what string
		g    access.Grant
		says string
	}{
		{"no principal", access.Grant{Scope: finance, Role: access.Viewer}, "principal"},
		{"a login in capitals", access.Grant{Principal: "Alice", Scope: finance, Deny: access.RunReadData}, "names no principal"},
		{"a login with a trailing space", access.Grant{Principal: "alice ", Scope: finance, Deny: access.RunReadData}, "names no principal"},
		{"a user written with a prefix", access.Grant{Principal: "user:alice", Scope: finance, Deny: access.RunReadData}, "names no principal"},
		{"the v0.2 operator", access.Grant{Principal: "operator", Scope: finance, Role: access.Owner}, "v0.2 operator"},
		{"a login on a reserved word", access.Grant{Principal: "runs", Scope: finance, Role: access.Viewer}, "routes on"},
		{"a group in capitals", access.Grant{Principal: "group:Team-Finance", Scope: finance, Deny: access.RunReadData}, "names no group"},
		{"a group with no name", access.Grant{Principal: "group:", Scope: finance, Deny: access.RunReadData}, "names no group"},
		{"a group name with a space", access.Grant{Principal: "group:team finance", Scope: finance, Deny: access.RunReadData}, "names no group"},
		{"a service account with three parts", access.Grant{Principal: "finance/nightly/sync", Scope: finance, Role: access.Viewer}, "names no service account"},
		{"a service account with no namespace", access.Grant{Principal: "/agentiik", Scope: finance, Role: access.Viewer}, "names no service account"},
		{"a service account of a reserved word", access.Grant{Principal: "runs/agentiik", Scope: finance, Role: access.Viewer}, "routes on"},
		{"a login longer than a name is", access.Grant{Principal: strings.Repeat("a", 256), Scope: finance, Role: access.Viewer}, "names no principal"},
		{"a group longer than a name is", access.Grant{Principal: "group:" + strings.Repeat("a", 256), Scope: finance, Role: access.Viewer}, "names no group"},
		{"a service account longer than a name is", access.Grant{Principal: "finance/" + strings.Repeat("a", 256), Scope: finance, Role: access.Viewer}, "names no service account"},
		{"no scope", access.Grant{Principal: "alice", Role: access.Viewer}, "installation"},
		{"a scope on a reserved word", access.Grant{Principal: "alice", Scope: access.Scope{Namespace: "runs"}, Role: access.Viewer}, "routes on"},
		{"neither a role nor a deny", access.Grant{Principal: "alice", Scope: finance}, "neither"},
		{"both a role and a deny", access.Grant{Principal: "alice", Scope: finance, Role: access.Editor, Deny: access.SecretWrite}, "both"},
		{"a role that is not one of the four", access.Grant{Principal: "alice", Scope: finance, Role: "admin"}, "not a role"},
		{"a deny naming a role", access.Grant{Principal: "alice", Scope: finance, Deny: "operator"}, "is a role"},
		{"a deny naming no permission", access.Grant{Principal: "alice", Scope: finance, Deny: "run:*"}, "not a permission"},
	} {
		err := c.g.Validate()
		if err == nil {
			t.Errorf("%s is accepted", c.what)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s is refused as %q", c.what, err)
		}
	}
	for _, g := range []access.Grant{
		allow("g1", "group:team-finance", finance, access.Viewer),
		until(allow("g2", "finance/agentiik", invoicing, access.Operator), t0.AddDate(0, 0, 90)),
		deny("d1", "alice", invoicing, access.RunReadData),
		allow("g3", "bob-martin", finance, access.Editor),
		allow("g4", "group:runs", finance, access.Viewer),
		allow("g5", strings.Repeat("a", 255), finance, access.Viewer),
	} {
		if err := g.Validate(); err != nil {
			t.Errorf("%+v is refused: %v", g, err)
		}
	}
}

// A grant is written as the wire's accessGrant: the scope as one string, the role or the deny and
// not the other, and no expiry where it has none.
func TestAGrantIsWrittenAsTheWireWritesIt(t *testing.T) {
	b, err := json.Marshal([]access.Grant{
		allow("g1", "group:team-finance", finance, access.Viewer),
		until(deny("d1", "alice", invoicing, access.RunReadData), t0.AddDate(0, 3, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":"g1","principal":"group:team-finance","scope":"finance","role":"viewer","granted_by":"operator","granted_at":"2026-09-27T09:00:00Z"},` +
		`{"id":"d1","principal":"alice","scope":"finance/monthly-invoicing","deny":"run:read_data","expires_at":"2026-12-27T09:00:00Z","granted_by":"operator","granted_at":"2026-09-27T09:00:00Z"}]`
	if string(b) != want {
		t.Errorf("written as\n%s\nwant\n%s", b, want)
	}

	// An expiry at the zero time is written, since it is one, and read back as one.
	b, err = json.Marshal(until(allow("g2", "alice", finance, access.Owner), time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"expires_at":"0001-01-01T00:00:00Z"`) {
		t.Fatalf("a grant that ended at the zero time is written as %s", b)
	}
	var g access.Grant
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if !g.Expired(t0) {
		t.Errorf("a grant that ended at the zero time reads back as %s", b)
	}
}
