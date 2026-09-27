package access_test

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/internal/fixtures"
)

// Who a token may be minted for, whether a scope narrows anything, and who owns a namespace: what
// POST /api/v1/auth/tokens asks before it mints.

// A scope narrows where either half is present, an empty half included, and the zero scope, which a
// token with none and every other credential carries, narrows nothing.
func TestAScopeNarrowsWhereEitherHalfIsPresent(t *testing.T) {
	if (access.TokenScope{}).Narrows() {
		t.Error("the zero scope narrows something")
	}
	for _, c := range []struct {
		permissions, within []string
	}{
		{[]string{"workflow:run"}, nil},
		{nil, []string{"finance"}},
		{[]string{"workflow:run"}, []string{"finance/monthly-invoicing"}},
		{[]string{}, nil},
	} {
		s, err := access.ParseTokenScope(c.permissions, c.within)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Narrows() {
			t.Errorf("a scope of %q within %q narrows nothing", c.permissions, c.within)
		}
	}
}

// A token is minted for a login or a service account and never for a group, and every principal
// reference the wire's corpus holds valid is one a token may be minted for unless it is a group.
func TestATokenBelongsToAUserOrAServiceAccount(t *testing.T) {
	for _, ref := range []string{"alice", "bob-martin", "finance/nightly-sync", "finance/agentiik", "finance/runs"} {
		if err := access.TokenHolder(ref); err != nil {
			t.Errorf("%q is refused: %v", ref, err)
		}
	}
	for ref, why := range map[string]string{
		"group:team-finance":     "is a group",
		"operator":               "operator",
		"installation":           "the installation itself",
		"runs":                   "a word the API routes on",
		"Alice":                  "names no principal",
		"":                       "names the principal",
		"finance/":               "names no service account",
		"finance/Nightly":        "names no service account",
		"finance/a/b":            "names no service account",
		"/nightly":               "names no service account",
		strings.Repeat("a", 256): "names no principal",
	} {
		err := access.TokenHolder(ref)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%.40q is answered %v, and it is refused as one that %s", ref, err, why)
		}
	}

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
		err = access.TokenHolder(ref)
		switch {
		case c.Valid && !strings.HasPrefix(ref, "group:") && err != nil:
			t.Errorf("%s: %q is refused, and the corpus accepts it as %s: %v", c.File, ref, c.Covers, err)
		case (!c.Valid || strings.HasPrefix(ref, "group:")) && err == nil:
			t.Errorf("%s: %q is accepted as a token's holder", c.File, ref)
		}
	}
}

// Owning a namespace is holding every permission of the owner role there, from a grant on the
// namespace, one's own or a group's: a grant on one workflow owns nothing, another role owns
// nothing, a deny taking one permission away owns nothing, and a grant past its expiry owns
// nothing.
func TestOwningANamespaceIsHoldingTheOwnerRoleThere(t *testing.T) {
	later := t0.Add(time.Hour)
	for _, c := range []struct {
		what   string
		who    access.Principal
		grants []access.Grant
		owns   bool
	}{
		{"an owner grant of one's own", bob, []access.Grant{allow("1", "bob", finance, access.Owner)}, true},
		{"an owner grant of one's group", alice, []access.Grant{allow("1", "group:team-finance", finance, access.Owner)}, true},
		{"a service account's owner grant", access.Principal{Ref: "finance/nightly"}, []access.Grant{allow("1", "finance/nightly", finance, access.Owner)}, true},
		{"an owner grant on one workflow", bob, []access.Grant{allow("1", "bob", invoicing, access.Owner)}, false},
		{"an editor grant", bob, []access.Grant{allow("1", "bob", finance, access.Editor)}, false},
		{"an owner grant in another namespace", bob, []access.Grant{allow("1", "bob", access.Scope{Namespace: "hr"}, access.Owner)}, false},
		{"an owner grant with a deny beside it", bob, []access.Grant{
			allow("1", "bob", finance, access.Owner), deny("2", "bob", finance, access.GrantManage),
		}, false},
		{"an owner grant past its expiry", bob, []access.Grant{until(allow("1", "bob", finance, access.Owner), t0)}, false},
		{"an owner grant of somebody else", bob, []access.Grant{allow("1", "carol", finance, access.Owner)}, false},
	} {
		owns, err := access.Owns(c.who, c.grants, "finance", later)
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if owns != c.owns {
			t.Errorf("%s owns finance: %v, want %v", c.what, owns, c.owns)
		}
	}
	if owns, _ := access.Owns(bob, []access.Grant{allow("1", "bob", finance, access.Owner)}, "", later); owns {
		t.Error("an owner of finance owns the installation")
	}
}
