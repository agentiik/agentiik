package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
)

// What a credential narrows its principal to, which the router intersects with every answer it asks
// the authorizer for: "on every request its rights are the principal's intersected with both, so a
// scope can only narrow".

// scoped identifies the bearer as bearer does, narrowed by the scope an X-Keeps and an X-Within
// header name, each a comma separated list, and refuses X-Refused with the sentence it holds.
func scoped(r *http.Request) (api.Identity, error) {
	if why := r.Header.Get("X-Refused"); why != "" {
		return api.Identity{Refused: why}, nil
	}
	as, err := bearer(r)
	if err != nil || as.Principal == "" {
		return as, err
	}
	var keeps, within []string
	if v := r.Header.Get("X-Keeps"); v != "" {
		keeps = strings.Split(v, ",")
	}
	if v := r.Header.Get("X-Within"); v != "" {
		within = strings.Split(v, ",")
	}
	as.Scope, err = access.ParseTokenScope(keeps, within)
	return as, err
}

// recording records every question the router asks, and allows alice everything.
// to refuse.
type recording struct {
	mu    sync.Mutex
	asked []api.Target
}

func (a *recording) Allow(_ context.Context, who api.Principal, _ api.Permission, over api.Target) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, over)
	return who == "alice", nil
}

func narrowed(t *testing.T, rt *api.Router, method, path, as, keeps, within string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+as)
	if keeps != "" {
		r.Header.Set("X-Keeps", keeps)
	}
	if within != "" {
		r.Header.Set("X-Within", within)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w
}

// A route refuses what the token does not keep, at the scope the route answers at, whatever the
// principal holds: a permission the token leaves out, a workflow outside what it reaches, and the
// installation for a token reaching some namespaces. Absent and forbidden stay the same 404 in a
// namespace, and the installation's refusal a 403.
func TestTheRouterRefusesWhatATokenDoesNotKeep(t *testing.T) {
	auth := &recording{}
	rt, err := api.NewRouter(auth, scoped)
	if err != nil {
		t.Fatal(err)
	}
	ok := func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
		w.WriteHeader(http.StatusOK)
	}
	rt.MustHandle("POST", "/api/v1/{namespace}/workflows/{workflow}/runs", api.Needs{Permission: api.WorkflowRun, Scope: api.Workflow}, ok)
	rt.MustHandle("GET", "/api/v1/{namespace}/secrets", api.Needs{Permission: api.WorkflowRead, Scope: api.Namespace}, ok)
	rt.MustHandle("GET", "/api/v1/runners", api.Needs{Permission: api.GrantManage, Scope: api.Installation}, ok)

	start := "/api/v1/finance/workflows/monthly-invoicing/runs"
	for _, c := range []struct {
		method, path, keeps, within string
		want                        int
	}{
		{"POST", start, "", "", http.StatusOK},
		{"POST", start, "workflow:run", "finance/monthly-invoicing", http.StatusOK},
		{"POST", start, "workflow:run", "finance", http.StatusOK},
		{"POST", start, "run:read", "", http.StatusNotFound},
		{"POST", start, "", "finance/payroll", http.StatusNotFound},
		{"POST", start, "", "team-ops", http.StatusNotFound},
		{"GET", "/api/v1/finance/secrets", "", "finance", http.StatusOK},
		{"GET", "/api/v1/finance/secrets", "", "finance/monthly-invoicing", http.StatusNotFound},
		{"GET", "/api/v1/runners", "", "", http.StatusOK},
		{"GET", "/api/v1/runners", "grant:manage", "", http.StatusOK},
		{"GET", "/api/v1/runners", "workflow:run", "", http.StatusForbidden},
		{"GET", "/api/v1/runners", "", "finance", http.StatusForbidden},
	} {
		auth.asked = nil
		w := narrowed(t, rt, c.method, c.path, "alice", c.keeps, c.within)
		if w.Code != c.want {
			t.Errorf("%s %s keeping %q within %q answered %d, want %d", c.method, c.path, c.keeps, c.within, w.Code, c.want)
		}
		if c.want != http.StatusOK && len(auth.asked) > 0 {
			t.Errorf("%s %s keeping %q within %q asked the authorizer about %v, which the token had answered already", c.method, c.path, c.keeps, c.within, auth.asked)
		}
	}
}

// What a handler asks as it goes is narrowed as the route was: what it may reveal, what a request
// may carry, what a listing across the installation may hold, and whether a stream may go on.
func TestWhatAHandlerAsksIsNarrowedAsTheRouteWas(t *testing.T) {
	rt, err := api.NewRouter(&recording{}, scoped)
	if err != nil {
		t.Fatal(err)
	}
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	var revealed, also, still bool
	rt.MustHandle("GET", "/api/v1/{namespace}/workflows/{workflow}/runs",
		api.Needs{Permission: api.RunRead, Scope: api.Workflow, Reveals: api.RunReadData, Also: api.SecretUse},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, over api.Target) {
			var err error
			if revealed, err = api.Revealing(r)(r.Context(), over); err != nil {
				t.Error(err)
			}
			if also, err = api.HoldsAlso(r)(r.Context()); err != nil {
				t.Error(err)
			}
			if still, err = api.Still(r)(r.Context()); err != nil {
				t.Error(err)
			}
		})
	var listed []bool
	rt.MustHandleAcross("GET", "/api/v1/runs", api.Across{Permission: api.RunRead},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target, holds api.Holds) {
			listed = nil
			for _, over := range []api.Target{invoicing, payroll} {
				held, err := holds(r.Context(), over)
				if err != nil {
					t.Error(err)
				}
				listed = append(listed, held)
			}
		})

	path := "/api/v1/finance/workflows/monthly-invoicing/runs"
	narrowed(t, rt, "GET", path, "alice", "", "")
	if !revealed || !also || !still {
		t.Fatalf("an unnarrowed caller was answered revealed %v, also %v, still %v", revealed, also, still)
	}
	narrowed(t, rt, "GET", path, "alice", "run:read", "")
	if revealed || also || !still {
		t.Errorf("a token keeping run:read alone was answered revealed %v, also %v, still %v", revealed, also, still)
	}
	narrowed(t, rt, "GET", "/api/v1/runs", "alice", "", "finance/monthly-invoicing")
	if len(listed) != 2 || !listed[0] || listed[1] {
		t.Errorf("a listing for a token within monthly-invoicing held %v", listed)
	}
}

// A stream asks again with the credential as it reads now, so a token whose scope no longer keeps
// the permission ends it, as a revoked one does.
func TestAStreamAsksAgainThroughTheScopeTheCredentialHasNow(t *testing.T) {
	var narrow sync.Mutex
	keeps := ""
	identify := func(r *http.Request) (api.Identity, error) {
		narrow.Lock()
		defer narrow.Unlock()
		if keeps != "" {
			r.Header.Set("X-Keeps", keeps)
		}
		return scoped(r)
	}
	rt, err := api.NewRouter(&recording{}, identify)
	if err != nil {
		t.Fatal(err)
	}
	var answers []bool
	rt.MustHandle("GET", "/api/v1/{namespace}/workflows/{workflow}/logs", api.Needs{Permission: api.RunRead, Scope: api.Workflow},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
			for _, now := range []string{"", "workflow:run"} {
				narrow.Lock()
				keeps = now
				narrow.Unlock()
				held, err := api.Still(r)(r.Context())
				if err != nil {
					t.Error(err)
				}
				answers = append(answers, held)
			}
		})
	narrowed(t, rt, "GET", "/api/v1/finance/workflows/monthly-invoicing/logs", "alice", "", "")
	if len(answers) != 2 || !answers[0] || answers[1] {
		t.Errorf("a stream asked again was answered %v, and its token stopped keeping run:read in between", answers)
	}
}

// A route at installation scope asks the authorizer about the installation, whatever its path
// names, so that administering it is one question with one answer, and a namespace in its path is
// the handler's and never what it was authorised by.
func TestARouteAtInstallationScopeAsksAboutTheInstallation(t *testing.T) {
	auth := &recording{}
	rt, err := api.NewRouter(auth, scoped)
	if err != nil {
		t.Fatal(err)
	}
	var handed api.Target
	rt.MustHandle("PUT", "/api/v1/namespaces/{namespace}/quotas", api.Needs{Permission: api.GrantManage, Scope: api.Installation},
		func(w http.ResponseWriter, _ *http.Request, _ api.Principal, over api.Target) { handed = over })
	narrowed(t, rt, "PUT", "/api/v1/namespaces/finance/quotas", "alice", "", "")
	if len(auth.asked) != 1 || auth.asked[0] != (api.Target{}) {
		t.Errorf("a route at installation scope asked about %v", auth.asked)
	}
	if handed != (api.Target{Namespace: "finance"}) {
		t.Errorf("the handler was handed %+v, and its path names finance", handed)
	}
	// A token reaching finance alone is not reaching the installation, whatever the path says.
	if w := narrowed(t, rt, "PUT", "/api/v1/namespaces/finance/quotas", "alice", "", "finance"); w.Code != http.StatusForbidden {
		t.Errorf("a token within finance answered %d at installation scope", w.Code)
	}
}

// A credential that came and opened nothing is refused with the sentence its identification gave,
// on every kind of route, and one that never came with the absence of one.
func TestARefusedCredentialSaysWhy(t *testing.T) {
	rt, err := api.NewRouter(&recording{}, scoped)
	if err != nil {
		t.Fatal(err)
	}
	ok := func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
		t.Error("a refused caller reached the handler")
	}
	rt.MustHandle("GET", "/api/v1/runners", api.Needs{Permission: api.GrantManage, Scope: api.Installation}, ok)
	rt.MustHandleAcross("GET", "/api/v1/runs", api.Across{Permission: api.RunRead},
		func(http.ResponseWriter, *http.Request, api.Principal, api.Target, api.Holds) {
			t.Error("a refused caller reached the listing")
		})
	for _, path := range []string{"/api/v1/runners", "/api/v1/runs"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Refused", "that token opens nothing")
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" || w.Body.String() != "{\"error\":\"that token opens nothing\"}\n" {
			t.Errorf("%s answered %d %q", path, w.Code, w.Body.String())
		}
		if code, body := reached(t, rt, "GET", path, ""); code != http.StatusUnauthorized || !strings.Contains(body, "this request carries no credential") {
			t.Errorf("%s with no credential answered %d %q", path, code, body)
		}
	}
}
