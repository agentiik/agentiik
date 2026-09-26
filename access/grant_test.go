package access_test

import (
	"encoding/json"
	"strings"
	"testing"

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
}
