package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
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

// bearer reads a principal out of the header, checking nothing, which is what the router looks
// like from a test of the routes rather than of a credential: Principals is what checks one.
func bearer(r *http.Request) (api.Identity, error) {
	if r.Header.Get("X-Broken") != "" {
		return api.Identity{}, context.DeadlineExceeded
	}
	return api.Identity{Principal: api.Principal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))}, nil
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

// The router tells a route whether its caller may spend what a read spends, an artifact's fetch,
// from the credential it identified: not where it is a session carried from elsewhere, and so not
// on a request the router did not serve, which a handler must not take for permission.
func TestARouteIsToldWhetherItsCallerSpends(t *testing.T) {
	rt, err := api.NewRouter(holder{who: "alice", what: api.RunRead, over: api.Target{Namespace: "finance"}}, func(r *http.Request) (api.Identity, error) {
		as, err := bearer(r)
		as.Elsewhere = r.Header.Get("X-Elsewhere") != ""
		return as, err
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.MustHandle("GET", "/api/v1/{namespace}/runs",
		api.Needs{Permission: api.RunRead, Scope: api.Namespace},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			if api.Spends(r) {
				w.Header().Set("X-Spends", "yes")
			}
		})
	for elsewhere, want := range map[string]string{"": "yes", "yes": ""} {
		r := httptest.NewRequest("GET", "/api/v1/finance/runs", nil)
		r.Header.Set("Authorization", "Bearer alice")
		r.Header.Set("X-Elsewhere", elsewhere)
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Header().Get("X-Spends") != want {
			t.Errorf("a caller marked elsewhere %q was answered %d, spending %q", elsewhere, w.Code, w.Header().Get("X-Spends"))
		}
	}
	if api.Spends(httptest.NewRequest("GET", "/api/v1/finance/runs", nil)) {
		t.Error("a request the router did not serve spends")
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
// each matches a path the other does, /api/v1/finance/runs/latest, and neither is the more
// specific. The route refused is not part of the surface either, since nothing answers it.
func TestARouteTheMuxCannotServeBesideAnotherIsAnError(t *testing.T) {
	rt := router(t, api.DenyAll{})
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	rt.MustHandle("GET", "/api/v1/{namespace}/runs/{run}", api.Needs{Permission: api.RunRead, Scope: api.Namespace}, ok)

	clash := "/api/v1/{namespace}/{what}/latest"
	if err := rt.Handle("GET", clash, api.Needs{Permission: api.RunRead, Scope: api.Namespace}, ok); err == nil || !strings.Contains(err.Error(), clash) {
		t.Errorf("a route the mux refuses was answered %v", err)
	}
	for _, r := range rt.Routes() {
		if r.Pattern == clash {
			t.Error("a route the mux refused is listed as served")
		}
	}
}

// The documentation's surface holds routes net/http cannot serve on one mux: GET /api/v1/runs/{id}
// and GET /api/v1/{ns}/runs both match /api/v1/runs/runs, and GET /api/v1/artifacts/{uri} beside
// either matches /api/v1/artifacts/runs. So they are served together, and a path whose first
// segment after /api/v1/ is one of the API's own words is answered by the route of that word, the
// word written with its letters escaped included, while every other path there reaches the
// namespaced routes.
func TestTheAPIsOwnWordsAreServedBesideTheNamespacedRoutes(t *testing.T) {
	rt := router(t, everything{who: "alice"})
	rt.ServeRuns(&runsOf{of: map[string]api.Target{
		"01M2Z8V1P9C4XQ7K2N4D6F8H0C": {Namespace: "finance", Workflow: "monthly-invoicing"},
	}})
	answering := func(what string) api.Handler {
		return func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
			w.Write([]byte(what))
		}
	}
	for _, r := range []struct {
		pattern string
		guard   api.Guard
		what    string
	}{
		{"/api/v1/{namespace}/runs", api.Needs{Permission: api.RunRead, Scope: api.Namespace}, "listing"},
		{"/api/v1/{namespace}/secrets", api.Needs{Permission: api.RunRead, Scope: api.Namespace}, "secrets"},
		{"/api/v1/runs/{run}", api.OnRun{Permission: api.RunRead}, "run"},
		{"/api/v1/artifacts/{uri}", api.OnArtifact{Permission: api.RunRead}, "artifact"},
	} {
		if err := rt.Handle("GET", r.pattern, r.guard, answering(r.what)); err != nil {
			t.Fatalf("%s could not be served beside the others: %v", r.pattern, err)
		}
	}

	for _, c := range []struct{ path, want string }{
		{"/api/v1/finance/runs", "listing"},
		{"/api/v1/finance/secrets", "secrets"},
		{"/api/v1/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0C", "run"},
		{"/api/v1/run%73/01M2Z8V1P9C4XQ7K2N4D6F8H0C", "run"},
		{"/api/v1/artifacts/" + url.PathEscape("agk://run/01M2Z8V1P9C4XQ7K2N4D6F8H0C/archive/ok/invoice.pdf"), "artifact"},
	} {
		if code, body := reached(t, rt, "GET", c.path, "alice"); code != http.StatusOK || body != c.want {
			t.Errorf("%s answered %d %q, want the %s route", c.path, code, body, c.want)
		}
	}

	// A namespace named after one of the words is one whose routes nothing reaches: the paths
	// are the word's, and answer as its route does, here as a run nobody minted.
	for _, path := range []string{"/api/v1/runs/runs", "/api/v1/runs/secrets", "/api/v1/artifacts/runs"} {
		if code, body := reached(t, rt, "GET", path, "alice"); code != http.StatusNotFound || body == "listing" || body == "secrets" {
			t.Errorf("%s answered %d %q, and reached a namespaced route", path, code, body)
		}
	}
}

// A word the API routes on is a namespace nobody can reach, so a route is registered under a
// word only once the word is reserved, and the reserved list is fixed rather than read off the
// routes: otherwise a new route would quietly take an existing namespace's paths.
func TestARouteOnlyTakesAWordAlreadyReserved(t *testing.T) {
	rt := router(t, api.DenyAll{})
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	err := rt.Handle("GET", "/api/v1/widgets/{id}", api.Needs{Permission: api.RunRead, Scope: api.Installation}, ok)
	if err == nil || !strings.Contains(err.Error(), "widgets") {
		t.Fatalf("a route on an unreserved word was registered: %v", err)
	}
	for _, word := range agk.ReservedNamespaces {
		if err := rt.Handle("GET", "/api/v1/"+word+"/probe", api.Needs{Permission: api.RunRead, Scope: api.Installation}, ok); err != nil {
			t.Errorf("a route on the reserved word %s was refused: %v", word, err)
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
	rt.MustHandle("GET", "/auth/healthz", api.Public{Why: "a health check answers whether this process is up, which is not something anybody learns anything from"}, ok)

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

// A route that needs a second permission for some requests asks about the one its guard names as
// Also, for its own caller, over the target it was authorised against. A route naming none, and a
// request no router served, are answered false, so a handler asking where nothing was declared
// refuses. A permission nobody documents is refused at registration, and the surface lists it.
func TestARouteAsksOnlyAboutWhatItAlsoNeeds(t *testing.T) {
	finance := api.Target{Namespace: "finance"}
	auth := &asked{Authorizer: denying{
		allowed: granted{
			"alice": {{api.WorkflowWrite, finance}, {api.SecretUse, finance}},
			"bob":   {{api.WorkflowWrite, finance}},
		},
	}}
	rt := router(t, auth)

	var answer bool
	var failure error
	asking := func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
		answer, failure = api.HoldsAlso(r)(r.Context())
	}
	rt.MustHandle("PUT", "/api/v1/{namespace}/workflows/{workflow}/versions/{commit}", api.Needs{Permission: api.WorkflowWrite, Scope: api.Workflow, Also: api.SecretUse}, asking)
	rt.MustHandle("PUT", "/api/v1/{namespace}/workflows/{workflow}/triggers", api.Needs{Permission: api.WorkflowWrite, Scope: api.Workflow}, asking)

	for _, c := range []struct {
		who, path string
		want      bool
	}{
		{"alice", "/api/v1/finance/workflows/monthly-invoicing/versions/" + aCommit, true},
		{"bob", "/api/v1/finance/workflows/monthly-invoicing/versions/" + aCommit, false},
		{"alice", "/api/v1/finance/workflows/monthly-invoicing/triggers", false},
	} {
		auth.questions = nil
		if code, body := reached(t, rt, "PUT", c.path, c.who); code != http.StatusOK {
			t.Fatalf("%s reaching %s was answered %d: %s", c.who, c.path, code, body)
		}
		if answer != c.want || failure != nil {
			t.Errorf("%s at %s was answered %v, %v", c.who, c.path, answer, failure)
		}
		// Asked over the workflow the route was authorised against, and only where declared.
		also := 0
		for _, q := range auth.questions {
			if q.what == api.SecretUse {
				also++
				if q.over != (api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}) {
					t.Errorf("secret:use was asked over %v", q.over)
				}
			}
		}
		if declared := strings.Contains(c.path, "/versions/"); (also == 1) != declared {
			t.Errorf("%s at %s asked about secret:use %d times", c.who, c.path, also)
		}
	}
	if held, err := api.HoldsAlso(httptest.NewRequest("PUT", "/api/v1/finance/workflows/monthly-invoicing/triggers", nil))(t.Context()); held || err != nil {
		t.Errorf("a request no router served was answered %t, %v", held, err)
	}

	// A request built from one a route needing secret:use was given, and served again as a
	// facade over the API serves one, asks what its own route declared: nothing.
	rt.MustHandle("PUT", "/api/v1/{namespace}/workflows/{workflow}/facade", api.Needs{Permission: api.WorkflowWrite, Scope: api.Workflow, Also: api.SecretUse},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
			again := r.Clone(r.Context())
			again.URL.Path = "/api/v1/finance/workflows/monthly-invoicing/triggers"
			rt.ServeHTTP(w, again)
		})
	answer = true
	if code, _ := reached(t, rt, "PUT", "/api/v1/finance/workflows/monthly-invoicing/facade", "alice"); code != http.StatusOK || answer || failure != nil {
		t.Errorf("a route needing nothing more, reached through one needing secret:use, was answered %v, %v", answer, failure)
	}

	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	if err := rt.Handle("PUT", "/api/v1/{namespace}/workflows/{workflow}/other", api.Needs{Permission: api.WorkflowWrite, Scope: api.Workflow, Also: "secret:everything"}, ok); err == nil {
		t.Error("a route was registered needing as well a permission nobody documents")
	}
	for _, route := range rt.Routes() {
		want := api.Permission("")
		if strings.Contains(route.Pattern, "/versions/") || strings.HasSuffix(route.Pattern, "/facade") {
			want = api.SecretUse
		}
		if route.Also != want {
			t.Errorf("the surface lists %s as needing %q as well", route.Pattern, route.Also)
		}
	}
}
