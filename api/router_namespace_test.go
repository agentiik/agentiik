package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// A namespace's record, "to an administrator and to a principal holding a grant in it; anyone else
// is answered the 404 of one that does not exist", which the router answers for a route taking
// OnNamespace from what the authorizer says of both.

// members says carol administers the installation, alice holds a grant in finance unless gone is
// set, and nobody else holds anything; with broke set, the namespaces cannot be read.
type members struct {
	broke bool
	gone  *atomic.Bool
}

func (members) Allow(_ context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	return who == "carol" && what == api.GrantManage && over == api.Target{}, nil
}

func (m members) HeldIn(_ context.Context, who api.Principal) ([]string, error) {
	if m.broke {
		return nil, context.DeadlineExceeded
	}
	if who == "alice" && (m.gone == nil || !m.gone.Load()) {
		return []string{"finance"}, nil
	}
	return nil, nil
}

// seen writes which of three namespaces the route's caller sees, as Sees answers.
func seen(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
	var out []string
	for _, ns := range []string{"finance", "hr", "team-ops"} {
		if api.Sees(r)(ns) {
			out = append(out, ns)
		}
	}
	w.Write([]byte(strings.Join(out, ",")))
}

func namespaceRouter(t *testing.T, auth api.Authorizer) *api.Router {
	t.Helper()
	rt, err := api.NewRouter(auth, scoped)
	if err != nil {
		t.Fatal(err)
	}
	rt.MustHandle("GET", "/api/v1/namespaces", api.OnNamespace{}, seen)
	rt.MustHandle("GET", "/api/v1/namespaces/{namespace}", api.OnNamespace{}, seen)
	return rt
}

// A namespace's record is answered to an administrator whichever it is, and to anybody else where
// they hold a grant in it; one they hold nothing in is the 404 of one that is not there, and a caller
// with no credential is told to present one.
func TestANamespaceIsSeenByItsAdministratorAndWhoeverHoldsAGrantInIt(t *testing.T) {
	rt := namespaceRouter(t, members{})
	for _, c := range []struct {
		path, as string
		want     int
		body     string
	}{
		{"/api/v1/namespaces/hr", "carol", http.StatusOK, "finance,hr,team-ops"},
		{"/api/v1/namespaces/finance", "alice", http.StatusOK, "finance"},
		{"/api/v1/namespaces/hr", "alice", http.StatusNotFound, ""},
		{"/api/v1/namespaces/finance", "bob", http.StatusNotFound, ""},
		{"/api/v1/namespaces/finance", "", http.StatusUnauthorized, ""},
		{"/api/v1/namespaces", "carol", http.StatusOK, "finance,hr,team-ops"},
		{"/api/v1/namespaces", "alice", http.StatusOK, "finance"},
		{"/api/v1/namespaces", "bob", http.StatusOK, ""},
	} {
		code, body := reached(t, rt, "GET", c.path, c.as)
		if code != c.want || c.want == http.StatusOK && body != c.body {
			t.Errorf("GET %s as %q answered %d %q, want %d %q", c.path, c.as, code, body, c.want, c.body)
		}
		if code == http.StatusNotFound && body != `{"error":"no such thing, or not yours"}`+"\n" {
			t.Errorf("GET %s as %q was refused with %s, which is not the refusal of what is absent", c.path, c.as, body)
		}
	}
}

// A token narrows what its holder sees as it narrows everything else: a within takes an
// administrator's powers away, and reaches a namespace through the namespace or one of its
// workflows. The permissions a token keeps narrow nothing here, since reading a record is none of
// them.
func TestATokenNarrowsTheNamespacesItsHolderSees(t *testing.T) {
	rt := namespaceRouter(t, members{})
	for _, c := range []struct {
		as, keeps, within string
		want              string
	}{
		{"carol", "", "", "finance,hr,team-ops"},
		{"carol", "", "finance", ""},
		{"carol", "workflow:run", "", ""},
		{"alice", "", "finance/monthly-invoicing", "finance"},
		{"alice", "workflow:run", "", "finance"},
		{"alice", "", "hr", ""},
	} {
		w := narrowed(t, rt, "GET", "/api/v1/namespaces", c.as, c.keeps, c.within)
		if w.Code != http.StatusOK || w.Body.String() != c.want {
			t.Errorf("%s keeping %q within %q sees %d %q, want %q", c.as, c.keeps, c.within, w.Code, w.Body, c.want)
		}
	}
	if w := narrowed(t, rt, "GET", "/api/v1/namespaces/finance", "alice", "", "hr"); w.Code != http.StatusNotFound {
		t.Errorf("a token reaching hr alone read finance's record: %d", w.Code)
	}
}

// Who holds a grant where is a question that could not be answered, which is a 500 and never a
// refusal; and a route that does not take OnNamespace sees nothing, so a handler asking where it
// was given nothing to ask withholds.
func TestANamespaceNobodyCouldSayIsSeenIsNotRefused(t *testing.T) {
	rt := namespaceRouter(t, members{broke: true})
	if code, _ := reached(t, rt, "GET", "/api/v1/namespaces/finance", "alice"); code != http.StatusInternalServerError {
		t.Errorf("a namespace whose holders could not be read answered %d", code)
	}

	rt = router(t, holder{who: "carol", what: api.RunRead, over: api.Target{Namespace: "finance"}})
	rt.MustHandle("GET", "/api/v1/{namespace}/runs", api.Needs{Permission: api.RunRead, Scope: api.Namespace}, seen)
	if code, body := reached(t, rt, "GET", "/api/v1/finance/runs", "carol"); code != http.StatusOK || body != "" {
		t.Errorf("a route not about namespaces saw %q (%d)", body, code)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if api.Sees(r)("finance") {
		t.Error("a request the router did not serve sees a namespace")
	}
}

// A route about namespaces' records is refused registration on a router whose authorizer cannot say
// who holds a grant where, since it could answer nobody but an administrator, and so is one naming
// a workflow, a run or an artifact, which the record is not authorised against.
func TestARouteAboutANamespaceNeedsSomebodyToSayWhoHoldsWhat(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	if err := router(t, api.DenyAll{}).Handle("GET", "/api/v1/namespaces", api.OnNamespace{}, ok); err == nil {
		t.Error("a route about namespaces was registered on a router that cannot say who holds a grant where")
	}
	rt := router(t, members{})
	for _, pattern := range []string{
		"/api/v1/namespaces/{namespace}/workflows/{workflow}",
		"/api/v1/namespaces/{namespace}/runs/{run}",
		"/api/v1/namespaces/{namespace}/artifacts/{uri}",
	} {
		if err := rt.Handle("GET", pattern, api.OnNamespace{}, ok); err == nil {
			t.Errorf("%s was registered about a namespace's record", pattern)
		}
	}
	if err := rt.Handle("GET", "/api/v1/namespaces/{namespace}", api.OnNamespace{}, ok); err != nil {
		t.Fatal(err)
	}
	if got := rt.Routes()[0]; !got.Members || got.Permission != "" || got.Scope != api.Namespace {
		t.Errorf("the route reads back as %+v", got)
	}
}

// A route about a namespace's record asks again, as it goes, what it was let through on: a caller
// still holding a grant there still sees it, and one whose grant went since does not.
func TestARouteAboutANamespaceAsksAgainWhetherItsCallerStillSeesIt(t *testing.T) {
	gone := &atomic.Bool{}
	rt, err := api.NewRouter(members{gone: gone}, scoped)
	if err != nil {
		t.Fatal(err)
	}
	var still []bool
	rt.MustHandle("GET", "/api/v1/namespaces/{namespace}", api.OnNamespace{}, func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
		for _, lost := range []bool{false, true} {
			gone.Store(lost)
			ok, err := api.Still(r)(r.Context())
			if err != nil {
				t.Fatal(err)
			}
			still = append(still, ok)
		}
	})
	if code, _ := reached(t, rt, "GET", "/api/v1/namespaces/finance", "alice"); code != http.StatusOK {
		t.Fatalf("the route answered %d", code)
	}
	if len(still) != 2 || !still[0] || still[1] {
		t.Errorf("the route asked again and was answered %v, want true while the grant held and false once it went", still)
	}
}
