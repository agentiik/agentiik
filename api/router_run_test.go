package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/agentiik/agentiik/api"
)

// A route about one run, whose path names the run and nothing it is of, authorised against the
// namespace and workflow the run is of.

// runsOf knows which namespace and workflow each run is of, and counts how often it is asked.
type runsOf struct {
	of    map[string]api.Target
	broke bool
	asked atomic.Int32
}

func (f *runsOf) RunOf(_ context.Context, run string) (api.Target, error) {
	f.asked.Add(1)
	if f.broke {
		return api.Target{}, context.DeadlineExceeded
	}
	of, held := f.of[run]
	if !held {
		return api.Target{}, api.ErrNoRun
	}
	return of, nil
}

// Held on one workflow, workflow:run reaches the runs of that workflow and no others, not even a
// run of a workflow of the same name in another namespace, and a run that is not there answers the
// same as one that is not reachable, body included.
func TestARouteAboutARunIsAuthorisedAgainstItsWorkflow(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	rt := router(t, holder{who: "alice", what: api.WorkflowRun, over: invoicing})
	runs := &runsOf{of: map[string]api.Target{
		"01M2Z8V1P9C4XQ7K2N4D6F8H0C": invoicing,
		"01M2Z8V1P9C4XQ7K2N4D6F8H0D": {Namespace: "finance", Workflow: "payroll"},
		"01M2Z8V1P9C4XQ7K2N4D6F8H0E": {Namespace: "team-ops", Workflow: "monthly-invoicing"},
	}}
	rt.ServeRuns(runs)

	var sawOver api.Target
	var sawRun string
	rt.MustHandle("POST", "/api/v1/runs/{run}/cancel", api.OnRun{Permission: api.WorkflowRun},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			sawOver, sawRun = over, r.PathValue("run")
			w.WriteHeader(http.StatusAccepted)
		})

	if code, body := reached(t, rt, "POST", "/api/v1/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0C/cancel", "alice"); code != http.StatusAccepted {
		t.Fatalf("a run of the workflow she holds answered %d: %s", code, body)
	}
	if sawOver != invoicing || sawRun != "01M2Z8V1P9C4XQ7K2N4D6F8H0C" {
		t.Errorf("the handler was told %+v about run %s", sawOver, sawRun)
	}

	refused, refusedBody := reached(t, rt, "POST", "/api/v1/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0D/cancel", "alice")
	absent, absentBody := reached(t, rt, "POST", "/api/v1/runs/01M2ZZZZZZZZZZZZZZZZZZZZZZ/cancel", "alice")
	elsewhere, _ := reached(t, rt, "POST", "/api/v1/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0E/cancel", "alice")
	if refused != http.StatusNotFound || absent != http.StatusNotFound || elsewhere != http.StatusNotFound {
		t.Fatalf("a run of another workflow answered %d, an absent one %d and one of a namesake in another namespace %d", refused, absent, elsewhere)
	}
	if refusedBody != absentBody {
		t.Errorf("a refusal reads %q and an absence reads %q, which tells a caller which runs exist", refusedBody, absentBody)
	}

	// A caller with no credential is told to present one before anybody looks for the run.
	before := runs.asked.Load()
	if code, _ := reached(t, rt, "POST", "/api/v1/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0C/cancel", ""); code != http.StatusUnauthorized {
		t.Errorf("a caller with no credential answered %d", code)
	}
	if runs.asked.Load() != before {
		t.Error("a run was looked up for a caller with no credential")
	}

	// And a run that could not be looked up is not a run that is not there.
	runs.broke = true
	if code, _ := reached(t, rt, "POST", "/api/v1/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0C/cancel", "alice"); code != http.StatusInternalServerError {
		t.Errorf("a lookup that failed answered %d", code)
	}

	for _, r := range rt.Routes() {
		if r.Pattern == "/api/v1/runs/{run}/cancel" && (!r.OfRun || r.Scope != api.Workflow || r.Permission != api.WorkflowRun) {
			t.Errorf("the surface lists the route as %+v", r)
		}
	}
}

// What cannot be registered about a run: each is a route that would be authorised against a
// namespace or a workflow other than the ones its run is of, or against none.
func TestWhatCannotBeRegisteredAboutARun(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	run := api.OnRun{Permission: api.WorkflowRun}

	rt := router(t, api.DenyAll{})
	if err := rt.Handle("POST", "/api/v1/runs/{run}/cancel", run, ok); err == nil {
		t.Error("a route about a run was registered with nothing to find its workflow in")
	}

	rt.ServeRuns(&runsOf{})
	for _, c := range []struct {
		name    string
		pattern string
		guard   api.Guard
	}{
		{"no run in its path", "/api/v1/runs/cancel", run},
		{"a namespace in its path as well", "/api/v1/{namespace}/runs/{run}/cancel", run},
		{"a workflow in its path as well", "/api/v1/workflows/{workflow}/runs/{run}/cancel", run},
		{"a permission nobody documents", "/api/v1/runs/{run}/cancel", api.OnRun{Permission: "run:cancel"}},
	} {
		if err := rt.Handle("POST", c.pattern, c.guard, ok); err == nil {
			t.Errorf("a route about a run with %s was registered", c.name)
		}
	}
}

// An artifact is authorised against the workflow of the run its URI names, and a URI naming a run
// the caller cannot reach, a run that is not there, or nothing that parses as a URI all answer the
// same 404.
func TestARouteAboutAnArtifactIsAuthorisedAgainstItsRunsWorkflow(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	rt := router(t, holder{who: "alice", what: api.RunReadData, over: invoicing})
	rt.ServeRuns(&runsOf{of: map[string]api.Target{
		"01M2Z8V1P9C4XQ7K2N4D6F8H0C": invoicing,
		"01M2Z8V1P9C4XQ7K2N4D6F8H0D": {Namespace: "finance", Workflow: "payroll"},
	}})
	var saw api.Target
	rt.MustHandle("GET", "/api/v1/artifacts/{uri}", api.OnArtifact{Permission: api.RunReadData},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, over api.Target) {
			saw = over
			w.WriteHeader(http.StatusFound)
		})
	of := func(run string) string {
		return "/api/v1/artifacts/" + url.PathEscape("agk://run/"+run+"/archive/ok/invoice.pdf")
	}

	if code, body := reached(t, rt, "GET", of("01M2Z8V1P9C4XQ7K2N4D6F8H0C"), "alice"); code != http.StatusFound || saw != invoicing {
		t.Fatalf("an artifact of a run of the workflow she holds answered %d %s, over %+v", code, body, saw)
	}
	refused, refusedBody := reached(t, rt, "GET", of("01M2Z8V1P9C4XQ7K2N4D6F8H0D"), "alice")
	for _, path := range []string{
		of("01M2ZZZZZZZZZZZZZZZZZZZZZZ"),
		"/api/v1/artifacts/" + url.PathEscape("https://example.com/invoice.pdf"),
		"/api/v1/artifacts/" + url.PathEscape("agk://run/01M2Z8V1P9C4XQ7K2N4D6F8H0C/archive/ok"),
		"/api/v1/artifacts/not-a-uri",
	} {
		if code, body := reached(t, rt, "GET", path, "alice"); code != refused || body != refusedBody {
			t.Errorf("%s answered %d %q, and a refusal answers %d %q", path, code, body, refused, refusedBody)
		}
	}
	if refused != http.StatusNotFound {
		t.Errorf("an artifact of a run she cannot reach answered %d", refused)
	}
}

// What cannot be registered about an artifact, for the reasons a route about a run cannot be.
func TestWhatCannotBeRegisteredAboutAnArtifact(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	artifact := api.OnArtifact{Permission: api.RunReadData}

	rt := router(t, api.DenyAll{})
	if err := rt.Handle("GET", "/api/v1/artifacts/{uri}", artifact, ok); err == nil {
		t.Error("a route about an artifact was registered with nothing to find its run in")
	}
	rt.ServeRuns(&runsOf{})
	for _, c := range []struct{ name, pattern string }{
		{"no URI in its path", "/api/v1/artifacts/latest"},
		{"a run in its path as well", "/api/v1/runs/{run}/artifacts/{uri}"},
		{"a namespace in its path as well", "/api/v1/{namespace}/artifacts/{uri}"},
		{"a workflow in its path as well", "/api/v1/workflows/{workflow}/artifacts/{uri}"},
	} {
		if err := rt.Handle("GET", c.pattern, artifact, ok); err == nil {
			t.Errorf("a route about an artifact with %s was registered", c.name)
		}
	}
}

// A route answering across the installation is given what its caller may be asked about, for its
// own permission and its own caller, and never a target it did not ask about: a caller with no
// credential is refused before it runs, and a target naming no namespace, which is the
// installation, is an error rather than an answer.
func TestARouteAcrossTheInstallationAsksAboutItsOwnPermission(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	rt := router(t, granted{"alice": {{api.RunReadData, invoicing}, {api.RunRead, payroll}}})

	var answers []bool
	var asked error
	rt.MustHandleAcross("GET", "/api/v1/runs", api.Across{Permission: api.RunReadData},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, holds api.Holds) {
			answers = nil
			for _, over := range []api.Target{invoicing, payroll} {
				allowed, err := holds(r.Context(), over)
				if err != nil {
					t.Fatal(err)
				}
				answers = append(answers, allowed)
			}
			_, asked = holds(r.Context(), api.Target{})
			w.WriteHeader(http.StatusOK)
		})

	if code, _ := reached(t, rt, "GET", "/api/v1/runs", "alice"); code != http.StatusOK {
		t.Fatalf("the route answered %d", code)
	}
	if len(answers) != 2 || !answers[0] || answers[1] {
		t.Errorf("holds answered %v about a workflow she holds run:read_data on and one she holds only run:read on", answers)
	}
	if asked == nil {
		t.Error("holds answered about the installation itself")
	}
	if code, _ := reached(t, rt, "GET", "/api/v1/runs", "bob"); code != http.StatusOK || answers[0] {
		t.Errorf("holds answered for bob what alice holds: %v", answers)
	}

	answers = nil
	if code, _ := reached(t, rt, "GET", "/api/v1/runs", ""); code != http.StatusUnauthorized || answers != nil {
		t.Errorf("a caller with no credential answered %d and reached the handler: %v", code, answers)
	}
	r := httptest.NewRequest("GET", "/api/v1/runs", nil)
	r.Header.Set("X-Broken", "yes")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError || answers != nil {
		t.Errorf("a credential that could not be checked answered %d", w.Code)
	}

	for _, route := range rt.Routes() {
		if route.Pattern == "/api/v1/runs" && (!route.Across || route.Permission != api.RunReadData) {
			t.Errorf("the surface lists the route as %+v", route)
		}
	}
}

// What cannot be registered across the installation: a route whose path names a target has one
// to authorise before it runs, and a guard of this kind given to Handle has no Holds to hand on.
func TestWhatCannotBeRegisteredAcrossTheInstallation(t *testing.T) {
	across := api.Across{Permission: api.RunRead}
	rt := router(t, api.DenyAll{})
	rt.ServeRuns(&runsOf{})
	listing := func(http.ResponseWriter, *http.Request, api.Principal, api.Holds) {}
	for _, pattern := range []string{"/api/v1/{namespace}/runs", "/api/v1/workflows/{workflow}/runs", "/api/v1/runs/{run}/steps", "/api/v1/artifacts/{uri}/like"} {
		if err := rt.HandleAcross("GET", pattern, across, listing); err == nil {
			t.Errorf("%s was registered across the installation", pattern)
		}
	}
	if err := rt.HandleAcross("GET", "/api/v1/runs", api.Across{Permission: "run:everything"}, listing); err == nil {
		t.Error("a route across the installation needing a permission nobody documents was registered")
	}
	if err := rt.HandleAcross("GET", "/api/v1/runs", across, nil); err == nil {
		t.Error("a route across the installation with no handler was registered")
	}
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	if err := rt.Handle("GET", "/api/v1/runs", across, ok); err == nil {
		t.Error("a route across the installation was registered with a handler given no Holds")
	}
}
