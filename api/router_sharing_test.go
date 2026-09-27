package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// A route an administrator reaches as well, and one handed what its caller sees: the routes writing
// a grant, since "an administrator may create a grant in any namespace, for anybody, as a power of
// the installation rather than through grant:manage there", and a grant naming a service account of
// another namespace answered by whether its writer sees that namespace.

// sharer says carol administers the installation unless demoted is set, alice holds grant:manage on
// finance and a grant in finance and hr, and nobody else holds anything.
type sharer struct {
	demoted *atomic.Bool
}

func (s sharer) Allow(_ context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	switch {
	case who == "carol":
		return (s.demoted == nil || !s.demoted.Load()) && what == api.GrantManage && over == api.Target{}, nil
	case who == "alice":
		return what == api.GrantManage && over == api.Target{Namespace: "finance"}, nil
	}
	return false, nil
}

func (sharer) HeldIn(_ context.Context, who api.Principal) ([]string, error) {
	if who == "alice" {
		return []string{"finance", "hr"}, nil
	}
	return nil, nil
}

func sharingRouter(t *testing.T, auth api.Authorizer) *api.Router {
	t.Helper()
	rt, err := api.NewRouter(auth, scoped)
	if err != nil {
		t.Fatal(err)
	}
	rt.MustHandle("GET", "/api/v1/{namespace}/grants",
		api.Needs{Permission: api.GrantManage, Scope: api.Namespace, OrAdministrator: true, Seeing: true}, seenAdministering)
	rt.MustHandle("GET", "/api/v1/{namespace}/workflows/{workflow}/grants",
		api.Needs{Permission: api.GrantManage, Scope: api.Workflow, OrAdministrator: true}, seenAdministering)
	rt.MustHandle("GET", "/api/v1/{namespace}/secrets",
		api.Needs{Permission: api.GrantManage, Scope: api.Namespace}, seenAdministering)
	return rt
}

// seenAdministering writes what seen writes, and whether the caller came in by an administrator's
// power alone, as Administering answers.
func seenAdministering(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
	seen(w, r, who, over)
	if api.Administering(r) {
		w.Write([]byte("|administering"))
	}
}

// seeing asks as who, narrowed to within where it is not empty, and answers the status and what the
// handler said the caller sees.
func seeing(t *testing.T, rt *api.Router, path, who, within string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+who)
	if within != "" {
		r.Header.Set("X-Within", within)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// An administrator reaches a route declaring it in any namespace, and a workflow of one, holding
// nothing there, and its handler is told they came in by the power; but not a route that does not
// declare it, not through a token whose scope narrows the power away, and not for a namespace no
// grant could name. Whoever holds the permission reaches it as before, and is not administering
// there; the handler is told what its caller sees: every namespace for an administrator, and those
// it holds a grant in and its token reaches for anybody else.
func TestAnAdministratorReachesARouteThatSaysSo(t *testing.T) {
	rt := sharingRouter(t, sharer{})
	for _, c := range []struct {
		path, who, within string
		code              int
		sees              string
	}{
		{"/api/v1/hr/grants", "carol", "", http.StatusOK, "finance,hr,team-ops|administering"},
		{"/api/v1/nowhere-yet/grants", "carol", "", http.StatusOK, "finance,hr,team-ops|administering"},
		{"/api/v1/hr/workflows/onboarding/grants", "carol", "", http.StatusOK, "|administering"},
		{"/api/v1/hr/secrets", "carol", "", http.StatusNotFound, ""},
		{"/api/v1/hr/grants", "carol", "hr", http.StatusNotFound, ""},
		{"/api/v1/Not-A-Name/grants", "carol", "", http.StatusNotFound, ""},
		{"/api/v1/hr/workflows/%ff/grants", "carol", "", http.StatusNotFound, ""},
		{"/api/v1/finance/grants", "alice", "", http.StatusOK, "finance,hr"},
		{"/api/v1/finance/grants", "alice", "finance", http.StatusOK, "finance"},
		{"/api/v1/hr/grants", "alice", "", http.StatusNotFound, ""},
		{"/api/v1/finance/grants", "bob", "", http.StatusNotFound, ""},
	} {
		code, sees := seeing(t, rt, c.path, c.who, c.within)
		if code != c.code || (code == http.StatusOK && sees != c.sees) {
			t.Errorf("%s by %s within %q answered %d seeing %q, want %d seeing %q", c.path, c.who, c.within, code, sees, c.code, c.sees)
		}
	}
}

// A route an administrator reached asks again whether they still administer, as every route asks
// again what it was let through on.
func TestARouteAnAdministratorReachedAsksAgain(t *testing.T) {
	demoted := &atomic.Bool{}
	rt, err := api.NewRouter(sharer{demoted: demoted}, scoped)
	if err != nil {
		t.Fatal(err)
	}
	var still []bool
	rt.MustHandle("GET", "/api/v1/{namespace}/grants", api.Needs{Permission: api.GrantManage, Scope: api.Namespace, OrAdministrator: true},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
			for _, lost := range []bool{false, true} {
				demoted.Store(lost)
				ok, err := api.Still(r)(r.Context())
				if err != nil {
					t.Fatal(err)
				}
				still = append(still, ok)
			}
		})
	if code, _ := seeing(t, rt, "/api/v1/hr/grants", "carol", ""); code != http.StatusOK {
		t.Fatalf("the route answered %d", code)
	}
	if len(still) != 2 || !still[0] || still[1] {
		t.Errorf("the route asked again and was answered %v, want true while carol administered and false once she did not", still)
	}
}

// A route handed what its caller sees needs an authorizer that says who holds a grant where, and an
// administrator's route at the installation says nothing by saying an administrator reaches it;
// what a route declares reads back.
func TestWhatARouteAnAdministratorReachesCannotBe(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	if err := router(t, api.DenyAll{}).Handle("GET", "/api/v1/{namespace}/grants", api.Needs{Permission: api.GrantManage, Scope: api.Namespace, Seeing: true}, ok); err == nil {
		t.Error("a route handed what its caller sees was registered on a router that cannot say who holds a grant where")
	}
	if err := router(t, api.DenyAll{}).Handle("GET", "/api/v1/users", api.Needs{Permission: api.GrantManage, Scope: api.Installation, OrAdministrator: true}, ok); err == nil {
		t.Error("an administrator's route at the installation was registered saying an administrator reaches it as well")
	}
	rt := sharingRouter(t, sharer{})
	for _, r := range rt.Routes() {
		want := r.Pattern != "/api/v1/{namespace}/secrets"
		if r.OrAdministrator != want || r.Seeing != (r.Pattern == "/api/v1/{namespace}/grants") {
			t.Errorf("%s reads back as %+v", r.Pattern, r)
		}
	}
}
