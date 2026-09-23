package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// The one rule this package exists for: "Every request is authorised at the API boundary, deny by
// default: the API decides explicitly to permit or refuse, and no check is ever delegated to a
// client."

// holder allows one principal one permission over one target, and refuses everything else, which
// is what an authorizer looks like from the router's side.
type holder struct {
	who   api.Principal
	what  api.Permission
	over  api.Target
	broke bool
}

func (h holder) Allow(_ context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	if h.broke {
		return false, context.DeadlineExceeded
	}
	return who == h.who && what == h.what && over == h.over, nil
}

// bearer reads a principal out of the header, which is what v0.3.0 replaces with something that
// checks a credential.
func bearer(r *http.Request) (api.Principal, error) {
	if r.Header.Get("X-Broken") != "" {
		return "", context.DeadlineExceeded
	}
	return api.Principal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), nil
}

func router(t *testing.T, auth api.Authorizer) *api.Router {
	t.Helper()
	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func reached(t *testing.T, rt *api.Router, method, path, as string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if as != "" {
		r.Header.Set("Authorization", "Bearer "+as)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// An installation with nothing granted refuses everything, which is what deny by default means
// when there is nothing to grant yet.
func TestAnInstallationWithNoAccessModelRefusesEverything(t *testing.T) {
	rt := router(t, api.DenyAll{})
	rt.MustHandle("GET", "/api/v1/{namespace}/workflows/{workflow}",
		api.Needs{Permission: api.WorkflowRead, Scope: api.Workflow},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			t.Error("a handler ran behind DenyAll")
		})

	code, _ := reached(t, rt, "GET", "/api/v1/finance/workflows/monthly-invoicing", "alice")
	if code != http.StatusNotFound {
		t.Errorf("a refused workflow answered %d", code)
	}
}

// "An inaccessible workflow answering the same 404 as an absent one, so that probing yields
// nothing." A 403 at a namespaced scope is an oracle: ask for every name and the ones answering
// 403 are the ones that exist.
func TestWhatIsRefusedLooksLikeWhatIsAbsent(t *testing.T) {
	rt := router(t, holder{who: "alice", what: api.WorkflowRead, over: api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}})
	rt.MustHandle("GET", "/api/v1/{namespace}/workflows/{workflow}",
		api.Needs{Permission: api.WorkflowRead, Scope: api.Workflow},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			w.WriteHeader(http.StatusOK)
		})
	rt.MustHandle("GET", "/api/v1/runners",
		api.Needs{Permission: api.GrantManage, Scope: api.Installation},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			t.Error("the installation route ran for somebody who does not hold it")
		})

	// The one she holds.
	if code, _ := reached(t, rt, "GET", "/api/v1/finance/workflows/monthly-invoicing", "alice"); code != http.StatusOK {
		t.Errorf("the workflow she holds answered %d", code)
	}

	// One she does not, and one that does not exist. The two answers are the same answer,
	// body included, which is the whole of the rule.
	refused, refusedBody := reached(t, rt, "GET", "/api/v1/team-ops/workflows/nightly", "alice")
	absent, absentBody := reached(t, rt, "GET", "/api/v1/finance/workflows/nothing-of-that-name", "alice")
	if refused != http.StatusNotFound || absent != http.StatusNotFound {
		t.Fatalf("refused answered %d and absent answered %d", refused, absent)
	}
	if refusedBody != absentBody {
		t.Errorf("a refusal reads %q and an absence reads %q, which tells a caller which is which", refusedBody, absentBody)
	}

	// At the installation scope there is nothing to hide: the caller knows the installation
	// exists, so saying no tells them nothing.
	if code, _ := reached(t, rt, "GET", "/api/v1/runners", "alice"); code != http.StatusForbidden {
		t.Errorf("an installation route refused with %d", code)
	}
}

// "An unauthenticated caller: Deny by default at the API."
func TestACallerWithNoCredentialGetsNothing(t *testing.T) {
	rt := router(t, holder{who: "alice", what: api.RunRead, over: api.Target{Namespace: "finance"}})
	rt.MustHandle("GET", "/api/v1/{namespace}/runs",
		api.Needs{Permission: api.RunRead, Scope: api.Namespace},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			t.Error("a handler ran for a caller with no credential")
		})

	code, _ := reached(t, rt, "GET", "/api/v1/finance/runs", "")
	if code != http.StatusUnauthorized {
		t.Errorf("a caller with no credential answered %d", code)
	}
}

// A question that could not be answered is not an answer. Telling a caller they may not have
// something because the database is down is telling them something untrue.
func TestAQuestionThatCouldNotBeAnsweredIsNotARefusal(t *testing.T) {
	rt := router(t, holder{broke: true})
	rt.MustHandle("GET", "/api/v1/{namespace}/runs",
		api.Needs{Permission: api.RunRead, Scope: api.Namespace},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			t.Error("a handler ran after the authorizer failed")
		})

	if code, _ := reached(t, rt, "GET", "/api/v1/finance/runs", "alice"); code != http.StatusInternalServerError {
		t.Errorf("an authorizer that failed answered %d", code)
	}

	// And a credential that could not be checked is not a credential that failed.
	r := httptest.NewRequest("GET", "/api/v1/finance/runs", nil)
	r.Header.Set("Authorization", "Bearer alice")
	r.Header.Set("X-Broken", "yes")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("a credential that could not be checked answered %d", w.Code)
	}
}

// A handler is given the principal and the target the router resolved, so it never reads a
// namespace out of a path. One that did could read a different one from the one authorised.
func TestAHandlerIsToldWhoAndWhatRatherThanReadingIt(t *testing.T) {
	want := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	rt := router(t, holder{who: "alice", what: api.WorkflowRun, over: want})

	var sawWho api.Principal
	var sawOver api.Target
	rt.MustHandle("POST", "/api/v1/{namespace}/workflows/{workflow}/runs",
		api.Needs{Permission: api.WorkflowRun, Scope: api.Workflow},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			sawWho, sawOver = who, over
			w.WriteHeader(http.StatusAccepted)
		})

	if code, _ := reached(t, rt, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice"); code != http.StatusAccepted {
		t.Fatalf("the run was not started: %d", code)
	}
	if sawWho != "alice" || sawOver != want {
		t.Errorf("the handler was told %q, %+v", sawWho, sawOver)
	}
}

// A route outside the hook has to say what authorises it instead, at length, because "public"
// with nothing beside it is how a route that should have been guarded stops being guarded.
func TestAPublicRouteSaysWhatAuthorisesItInstead(t *testing.T) {
	rt := router(t, api.DenyAll{})

	if err := rt.Handle("POST", "/api/v1/runners", api.Public{Why: "no"},
		func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}); err == nil {
		t.Error("a public route was registered with a shrug for a reason")
	}

	ran := false
	rt.MustHandle("POST", "/api/v1/runners",
		api.Public{Why: "registration is authenticated by the join token in its body and by nothing else, because a machine that has not joined holds no credential yet"},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			ran = true
			if who != "" {
				t.Errorf("a public route was given principal %q", who)
			}
			w.WriteHeader(http.StatusCreated)
		})

	if code, _ := reached(t, rt, "POST", "/api/v1/runners", ""); code != http.StatusCreated {
		t.Errorf("a public route answered %d to a caller with no credential", code)
	}
	if !ran {
		t.Error("the public route did not run")
	}
}

// What cannot be registered. Each of these is a way a route would end up authorised against
// something other than what it serves.
func TestWhatCannotBeRegistered(t *testing.T) {
	rt := router(t, api.DenyAll{})
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}

	for _, c := range []struct {
		name    string
		method  string
		pattern string
		guard   api.Guard
		handler api.Handler
	}{
		{"no guard at all", "GET", "/api/v1/x", nil, ok},
		{"no handler", "GET", "/api/v1/x", api.Needs{Permission: api.RunRead, Scope: api.Installation}, nil},
		{"a permission nobody documents", "GET", "/api/v1/x", api.Needs{Permission: "run:everything", Scope: api.Installation}, ok},
		{"an empty permission", "GET", "/api/v1/x", api.Needs{Scope: api.Installation}, ok},
		{"a namespaced route with no namespace in it", "GET", "/api/v1/runs",
			api.Needs{Permission: api.RunRead, Scope: api.Namespace}, ok},
		{"a workflow route with no workflow in it", "GET", "/api/v1/{namespace}/workflows",
			api.Needs{Permission: api.WorkflowRead, Scope: api.Workflow}, ok},
	} {
		if err := rt.Handle(c.method, c.pattern, c.guard, c.handler); err == nil {
			t.Errorf("a route with %s was registered", c.name)
		}
	}
}

// Two routes net/http cannot serve together are an error from Handle rather than a panic out of it:
// each matches a path the other does, /api/v1/objects/runs, and neither is the more specific. The
// route refused is not part of the surface either, since nothing answers it.
func TestARouteTheMuxCannotServeBesideAnotherIsAnError(t *testing.T) {
	rt := router(t, api.DenyAll{})
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	rt.MustHandle("GET", "/api/v1/{namespace}/runs", api.Needs{Permission: api.RunRead, Scope: api.Namespace}, ok)

	why := api.Public{Why: "a route that stands in for another one in a test, and is never served to anybody"}
	if err := rt.Handle("GET", "/api/v1/objects/{key...}", why, ok); err == nil || !strings.Contains(err.Error(), "/api/v1/objects/{key...}") {
		t.Errorf("a route the mux refuses was answered %v", err)
	}
	for _, r := range rt.Routes() {
		if r.Pattern == "/api/v1/objects/{key...}" {
			t.Error("a route the mux refused is listed as served")
		}
	}
}

// The surface is readable, which is what lets a test assert about every route at once and what
// lets an installation print what it serves.
func TestTheSurfaceCanBeReadBack(t *testing.T) {
	rt := router(t, api.DenyAll{})
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	rt.MustHandle("GET", "/api/v1/{namespace}/runs", api.Needs{Permission: api.RunRead, Scope: api.Namespace}, ok)
	rt.MustHandle("GET", "/api/v1/runners", api.Needs{Permission: api.GrantManage, Scope: api.Installation}, ok)
	rt.MustHandle("GET", "/healthz", api.Public{Why: "a health check answers whether this process is up, which is not something anybody learns anything from"}, ok)

	routes := rt.Routes()
	if len(routes) != 3 {
		t.Fatalf("the surface holds %d routes", len(routes))
	}

	// Every route either needs a documented permission or says why it needs none. This is
	// the assertion an installation's own test makes over its whole surface.
	for _, r := range routes {
		switch {
		case r.Public && len(r.Why) < 20:
			t.Errorf("%s %s is public and says %q", r.Method, r.Pattern, r.Why)
		case !r.Public && !r.Permission.Valid():
			t.Errorf("%s %s needs %q", r.Method, r.Pattern, r.Permission)
		}
	}

	// And the copy is a copy.
	routes[0].Permission = "run:everything"
	if rt.Routes()[0].Permission == "run:everything" {
		t.Error("the surface can be edited by whoever reads it")
	}
}

// The nine atoms, held to the page. A permission invented here is one nothing documents and one
// no role includes.
func TestThePermissionsAreThePageOwn(t *testing.T) {
	want := []string{
		"workflow:read", "workflow:run", "workflow:write", "workflow:delete",
		"run:read", "run:read_data", "secret:use", "secret:write", "grant:manage",
	}
	if len(api.Permissions) != len(want) {
		t.Fatalf("this package has %d permissions and the page names %d", len(api.Permissions), len(want))
	}
	for i, w := range want {
		if string(api.Permissions[i]) != w {
			t.Errorf("permission %d is %q and the page names %q", i, api.Permissions[i], w)
		}
	}
	if api.Permission("run:read_data").Valid() != true || api.Permission("run:everything").Valid() {
		t.Error("Valid does not answer for the nine")
	}
}

// A refusal says nothing a caller could learn from, and is not cached by anything in between.
func TestARefusalSaysNothingUseful(t *testing.T) {
	rt := router(t, api.DenyAll{})
	rt.MustHandle("GET", "/api/v1/{namespace}/workflows/{workflow}",
		api.Needs{Permission: api.WorkflowRead, Scope: api.Workflow},
		func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {})

	r := httptest.NewRequest("GET", "/api/v1/finance/workflows/monthly-invoicing", nil)
	r.Header.Set("Authorization", "Bearer alice")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("a refusal is cached as %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("a refusal is not JSON: %s", w.Body)
	}
	if len(body) != 1 {
		t.Errorf("a refusal carries %d fields: %v", len(body), body)
	}
	for _, leak := range []string{"finance", "monthly-invoicing", "alice", "workflow:read"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("a refusal names %q, which is something the caller was asking about", leak)
		}
	}
}
