package accesstest

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// Resolves holds GET /api/v1/me, as every asker asks it, to what Holds says the asker holds: whether
// it administers the installation, its groups, and its effective permissions at every scope, before
// the lapse or from it where lapsed is set. It is the route the page names for "effective
// permissions per namespace", which a console reads to hide what the caller does not hold, and
// where each resolution rule shows as a set of permissions: the union of a principal's own grants
// and its groups', a workflow's grant adding to its namespace's, a deny winning at either scope,
// secret:use and secret:write coming from a namespace's grant alone, each role its fixed set,
// workflow:delete an owner's alone, nothing for an administrator where no grant gives it, and what
// a token narrows away.
func (f *Fixture) Resolves(t testing.TB, lapsed bool) {
	t.Helper()
	for _, as := range append(f.Askers(), f.Nobody, f.Bootstrap) {
		want := f.Holds(as, lapsed)
		a := f.ask(t, t.Context(), "GET", "/api/v1/me", as, nil)
		if !want.Opens {
			if a.Status != http.StatusUnauthorized {
				t.Errorf("GET /api/v1/me as %s, whose credential opens nothing, answered %d: %s", as.Name, a.Status, a.Body)
			}
			continue
		}
		if a.Status != http.StatusOK {
			t.Errorf("GET /api/v1/me as %s answered %d: %s", as.Name, a.Status, a.Body)
			continue
		}
		var me api.Me
		decode(t, a, &me)
		got := Holding{Admin: me.Admin, Groups: me.Groups, Permissions: map[string][]string{}}
		for at, held := range me.Permissions {
			for _, p := range held {
				got.Permissions[at] = append(got.Permissions[at], string(p))
			}
		}
		if diff := differs(want, got); diff != "" {
			t.Errorf("GET /api/v1/me as %s answers %s", as.Name, diff)
		}
		if me.Principal != as.Principal {
			t.Errorf("GET /api/v1/me as %s answers the principal %q", as.Name, me.Principal)
		}
	}
}

// differs says how got differs from want in what GET /api/v1/me answers, and is empty where it
// does not: each set compared as a set, whatever order the route lists it in.
func differs(want, got Holding) string {
	var said []string
	if got.Admin != want.Admin {
		said = append(said, fmt.Sprintf("admin %v, want %v", got.Admin, want.Admin))
	}
	if !sameSet(got.Groups, want.Groups) {
		said = append(said, fmt.Sprintf("the groups %v, want %v", got.Groups, want.Groups))
	}
	for _, at := range slices.Sorted(maps.Keys(want.Permissions)) {
		if !sameSet(got.Permissions[at], want.Permissions[at]) {
			said = append(said, fmt.Sprintf("at %s %v, want %v", at, got.Permissions[at], want.Permissions[at]))
		}
	}
	for _, at := range slices.Sorted(maps.Keys(got.Permissions)) {
		if _, ok := want.Permissions[at]; !ok {
			said = append(said, fmt.Sprintf("at %s %v, where it holds nothing of its own", at, got.Permissions[at]))
		}
	}
	return strings.Join(said, "; ")
}

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Compact(slices.Sorted(slices.Values(a))), slices.Compact(slices.Sorted(slices.Values(b))))
}

// Refuses holds the refusals no single route is about, each asked once: a request carrying two
// credentials, a browser's request changing something from another origin, a token revoked, the
// tokens nobody but a namespace's owner mints for its service account, and a version naming a
// secret, pushed by whoever holds workflow:write on its workflow and not secret:use in its
// namespace. It changes nothing the fixture holds but the versions it pushes, a second commit of
// each workflow.
func (f *Fixture) Refuses(t testing.TB) {
	t.Helper()

	// "A request with two credentials is 400": a token and a session together are answered as
	// neither.
	both := f.Alice
	both.Session, both.Name = f.AlicesBrowser.Session, "alice's token and her browser's session"
	if a := f.ask(t, t.Context(), "GET", "/api/v1/me", both, nil); a.Status != http.StatusBadRequest {
		t.Errorf("GET /api/v1/me carrying %s answered %d: %s", both.Name, a.Status, a.Body)
	}

	// A browser's session changes something only from the public URL's origin.
	elsewhere := f.CarolsBrowser
	elsewhere.from, elsewhere.Name = "https://elsewhere.example.com", "carol's browser on another host"
	if a := f.ask(t, t.Context(), "POST", "/api/v1/"+Finance+"/grants", elsewhere, refusedBody); a.Status != http.StatusForbidden {
		t.Errorf("POST /api/v1/finance/grants from %s answered %d: %s", elsewhere.Name, a.Status, a.Body)
	}

	// A token revoked opens nothing, from its next request.
	revoked := f.mint(t, f.BobsBrowser, "bob", api.TokenRequest{DeviceLabel: "a lost phone"})
	revoked.Name = "bob's token revoked"
	f.must(t, "GET", "/api/v1/me", revoked, nil, http.StatusOK)
	f.must(t, "DELETE", "/api/v1/auth/tokens/"+revoked.tokenID, f.Bob, nil, http.StatusNoContent)
	if a := f.ask(t, t.Context(), "GET", "/api/v1/me", revoked, nil); a.Status != http.StatusUnauthorized || !bytes.HasPrefix(a.Body, []byte(opensNothing)) {
		t.Errorf("GET /api/v1/me as %s answered %d: %s", revoked.Name, a.Status, a.Body)
	}

	// A token of finance/nightly-sync is minted by an owner of finance alone, through a
	// credential narrowing nothing; never by the service account for itself, whose renewal is
	// somebody's who still means it; and none of finance/agentiik, the installation's identity.
	for _, c := range []struct {
		as        Asker
		principal string
		want      int
	}{
		{f.CarolForFinance, NightlySync, http.StatusForbidden},
		{f.NightlySyncToken, NightlySync, http.StatusForbidden},
		{f.Alice, NightlySync, http.StatusUnprocessableEntity},
		{f.Bob, NightlySync, http.StatusUnprocessableEntity},
		{f.Carol, Finance + "/agentiik", http.StatusUnprocessableEntity},
		{f.Carol, NightlySync, http.StatusCreated},
	} {
		a := f.ask(t, t.Context(), "POST", "/api/v1/auth/tokens", c.as, api.TokenRequest{Principal: c.principal})
		if a.Status != c.want {
			t.Errorf("POST /api/v1/auth/tokens for %s as %s answered %d, want %d: %s", c.principal, c.as.Name, a.Status, c.want, a.Body)
		}
		if a.Status == http.StatusCreated {
			var issued api.IssuedToken
			decode(t, a, &issued)
			f.must(t, "DELETE", "/api/v1/auth/tokens/"+issued.APIToken.ID, f.Carol, nil, http.StatusNoContent)
		}
	}

	// secret:use is checked at the push, against the pusher, and comes from a namespace's grant
	// alone: a version naming a secret is taken from whoever holds workflow:write on the workflow
	// and secret:use in the namespace, refused with 403 from whoever holds the one and not the
	// other, and with the absence of the workflow from whoever holds neither.
	const named = "d7e3b1a5c9f2e4d6b8a0c2e4f6a8b0d2c4e6f8a1"
	for _, as := range f.Askers() {
		h := f.Holds(as, false)
		for _, at := range []Target{{Finance, Invoicing, ""}, {Finance, Payroll, ""}, {HR, Onboarding, ""}, {HR, Offboarding, ""}} {
			want := http.StatusNotFound
			switch {
			case h.Hold("workflow:write", at.Namespace, at.Workflow) && h.Hold("secret:use", at.Namespace, at.Workflow):
				want = http.StatusOK
			case h.Hold("workflow:write", at.Namespace, at.Workflow):
				want = http.StatusForbidden
			}
			path := "/api/v1/" + at.Namespace + "/workflows/" + at.Workflow + "/versions/" + named
			a := f.ask(t, t.Context(), "PUT", path, as, PushedNaming(t, at.Namespace, at.Workflow, f.Secrets[at.Namespace]))
			if a.Status != want {
				t.Errorf("PUT %s naming %s, as %s, answered %d, want %d: %s", path, f.Secrets[at.Namespace], as.Name, a.Status, want, a.Body)
			}
		}
	}
}
