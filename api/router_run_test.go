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

// A route about a run whose path names the namespace as well, as the one a Location names a run by
// does, is authorised against the run's own workflow, and a run of another namespace asked for under
// this one answers what a run that is not there answers, body included, before anything is asked
// about the namespace it is in.
func TestARouteAboutARunUnderANamespaceFindsItThereAlone(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	elsewhere := api.Target{Namespace: "team-ops", Workflow: "monthly-invoicing"}
	asked := granted{"alice": {{api.RunRead, invoicing}, {api.RunRead, elsewhere}}}
	rt := router(t, asked)
	rt.ServeRuns(&runsOf{of: map[string]api.Target{
		"01M2Z8V1P9C4XQ7K2N4D6F8H0C": invoicing,
		"01M2Z8V1P9C4XQ7K2N4D6F8H0D": {Namespace: "finance", Workflow: "payroll"},
		"01M2Z8V1P9C4XQ7K2N4D6F8H0E": elsewhere,
	}})
	var saw api.Target
	rt.MustHandle("GET", "/api/v1/{namespace}/runs/{run}", api.OnRun{Permission: api.RunRead},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, over api.Target) {
			saw = over
			w.WriteHeader(http.StatusOK)
		})

	if code, body := reached(t, rt, "GET", "/api/v1/finance/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0C", "alice"); code != http.StatusOK || saw != invoicing {
		t.Fatalf("a run of the one workflow she holds run:read on answered %d %s, over %+v", code, body, saw)
	}
	absent, absentBody := reached(t, rt, "GET", "/api/v1/finance/runs/01M2ZZZZZZZZZZZZZZZZZZZZZZ", "alice")
	for _, path := range []string{
		"/api/v1/finance/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0D",
		"/api/v1/finance/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0E",
		"/api/v1/team-ops/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0C",
	} {
		if code, body := reached(t, rt, "GET", path, "alice"); code != absent || body != absentBody {
			t.Errorf("%s answered %d %q, and a run that is not there %d %q", path, code, body, absent, absentBody)
		}
	}
	if absent != http.StatusNotFound {
		t.Errorf("a run that is not there answered %d", absent)
	}

	for _, route := range rt.Routes() {
		if route.Pattern == "/api/v1/{namespace}/runs/{run}" && (!route.OfRun || route.Scope != api.Workflow) {
			t.Errorf("the surface lists the route as %+v", route)
		}
	}
}

// What cannot be registered about a run: each is a route that would be authorised against a
// workflow other than the one its run is of, or against none.
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
		{"a workflow in its path as well", "/api/v1/workflows/{workflow}/runs/{run}/cancel", run},
		{"a namespace and a workflow in its path as well", "/api/v1/{namespace}/workflows/{workflow}/runs/{run}/cancel", run},
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
		func(w http.ResponseWriter, r *http.Request, who api.Principal, within api.Target, holds api.Holds) {
			if within != (api.Target{}) {
				t.Errorf("a route across the installation was handed %+v", within)
			}
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

// A route answering across the namespace its path names is handed that namespace, and what it may
// ask answers about the workflows there, one at a time, and about nothing outside it: a namespace
// its caller holds nothing in is still served, and lists what one that does not exist lists.
func TestARouteAcrossANamespaceAsksAboutItAlone(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	elsewhere := api.Target{Namespace: "team-ops", Workflow: "monthly-invoicing"}
	rt := router(t, granted{"alice": {{api.RunRead, invoicing}, {api.RunRead, elsewhere}}})

	var saw api.Target
	var answers []bool
	var failures []error
	rt.MustHandleAcross("GET", "/api/v1/{namespace}/runs", api.Across{Permission: api.RunRead},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, within api.Target, holds api.Holds) {
			saw, answers, failures = within, nil, nil
			for _, over := range []api.Target{invoicing, payroll, elsewhere, {}} {
				held, err := holds(r.Context(), over)
				answers, failures = append(answers, held), append(failures, err)
			}
			w.WriteHeader(http.StatusOK)
		})

	if code, body := reached(t, rt, "GET", "/api/v1/finance/runs", "alice"); code != http.StatusOK || saw != (api.Target{Namespace: "finance"}) {
		t.Fatalf("the route answered %d %s, handed %+v", code, body, saw)
	}
	if len(answers) != 4 || !answers[0] || answers[1] || answers[2] || answers[3] {
		t.Errorf("holds answered %v about the workflow she holds, one she does not, one she holds in another namespace and the installation", answers)
	}
	if len(failures) != 4 || failures[0] != nil || failures[1] != nil || failures[2] == nil || failures[3] == nil {
		t.Errorf("asking inside the namespace, in another and about the installation failed with %v", failures)
	}
	if code, _ := reached(t, rt, "GET", "/api/v1/nowhere/runs", "alice"); code != http.StatusOK || saw != (api.Target{Namespace: "nowhere"}) {
		t.Errorf("a namespace she holds nothing in answered %d, handed %+v", code, saw)
	}
	saw = api.Target{}
	if code, _ := reached(t, rt, "GET", "/api/v1/finance/runs", ""); code != http.StatusUnauthorized || saw != (api.Target{}) {
		t.Errorf("a caller with no credential answered %d and reached the handler", code)
	}
	for _, route := range rt.Routes() {
		if route.Pattern == "/api/v1/{namespace}/runs" && (!route.Across || route.Permission != api.RunRead) {
			t.Errorf("the surface lists the route as %+v", route)
		}
	}
}

// What cannot be registered across the installation: a route whose path names a target narrower
// than a namespace has one to authorise before it runs, and a guard of this kind given to Handle
// has no Holds to hand on.
func TestWhatCannotBeRegisteredAcrossTheInstallation(t *testing.T) {
	across := api.Across{Permission: api.RunRead}
	rt := router(t, api.DenyAll{})
	rt.ServeRuns(&runsOf{})
	listing := func(http.ResponseWriter, *http.Request, api.Principal, api.Target, api.Holds) {}
	for _, pattern := range []string{"/api/v1/{namespace}/workflows/{workflow}/runs", "/api/v1/workflows/{workflow}/runs", "/api/v1/runs/{run}/steps", "/api/v1/{namespace}/runs/{run}/steps", "/api/v1/artifacts/{uri}/like"} {
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

// A route that answers some callers more than others asks about the one permission its guard names
// as Reveals, for its own caller, over targets in the namespace it was authorised in and in no
// other. A route naming none, and a request no router served, are answered false rather than asked
// about, so a handler asking where nothing was declared withholds. A permission nobody documents
// is refused at registration, and the surface lists what each route reveals more to.
func TestARouteAsksOnlyAboutWhatItReveals(t *testing.T) {
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	elsewhere := api.Target{Namespace: "team-ops", Workflow: "monthly-invoicing"}
	rt := router(t, granted{"alice": {
		{api.RunRead, api.Target{Namespace: "finance"}},
		{api.RunReadData, invoicing},
		{api.RunReadData, elsewhere},
		{api.WorkflowRun, payroll},
	}})

	var answers []bool
	var failures []error
	asking := func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
		answers, failures = nil, nil
		for _, over := range []api.Target{invoicing, payroll, elsewhere} {
			held, err := api.Revealing(r)(r.Context(), over)
			answers, failures = append(answers, held), append(failures, err)
		}
	}
	rt.MustHandle("GET", "/api/v1/{namespace}/runs/{run}", api.Needs{Permission: api.RunRead, Scope: api.Namespace, Reveals: api.RunReadData}, asking)
	rt.MustHandle("GET", "/api/v1/{namespace}/runs", api.Needs{Permission: api.RunRead, Scope: api.Namespace}, asking)

	if code, body := reached(t, rt, "GET", "/api/v1/finance/runs/01M2Z8V1P9C4XQ7K2N4D6F8H0C", "alice"); code != http.StatusOK {
		t.Fatalf("the route answered %d: %s", code, body)
	}
	if len(answers) != 3 || !answers[0] || answers[1] || answers[2] {
		t.Errorf("a route revealing run:read_data was answered %v about a workflow she holds it on, one she holds workflow:run on, and one she holds it on in another namespace", answers)
	}
	if len(failures) != 3 || failures[0] != nil || failures[1] != nil || failures[2] == nil {
		t.Errorf("asking about the namespace it was authorised in and about another failed with %v", failures)
	}

	if code, _ := reached(t, rt, "GET", "/api/v1/finance/runs", "alice"); code != http.StatusOK {
		t.Fatalf("the route answered %d", code)
	}
	for i, held := range answers {
		if held || failures[i] != nil {
			t.Errorf("a route revealing nothing was answered %v and %v", answers, failures)
			break
		}
	}
	if held, err := api.Revealing(httptest.NewRequest("GET", "/api/v1/finance/runs", nil))(t.Context(), invoicing); held || err != nil {
		t.Errorf("a request no router served was answered %t, %v", held, err)
	}

	// A request built from one a revealing route was given, and served again as a facade over
	// the API serves one, asks what its own route declared.
	rt.MustHandle("GET", "/api/v1/{namespace}/facade", api.Needs{Permission: api.RunRead, Scope: api.Namespace, Reveals: api.RunReadData},
		func(w http.ResponseWriter, r *http.Request, _ api.Principal, _ api.Target) {
			again := r.Clone(r.Context())
			again.URL.Path = "/api/v1/finance/runs"
			rt.ServeHTTP(w, again)
		})
	answers = nil
	if code, _ := reached(t, rt, "GET", "/api/v1/finance/facade", "alice"); code != http.StatusOK || len(answers) != 3 || answers[0] {
		t.Errorf("a route revealing nothing, reached through one revealing run:read_data, was answered %v", answers)
	}

	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	rt.ServeRuns(&runsOf{})
	for _, c := range []struct {
		pattern string
		guard   api.Guard
	}{
		{"/api/v1/{namespace}/runs/{run}/steps", api.Needs{Permission: api.RunRead, Scope: api.Namespace, Reveals: "run:everything"}},
		{"/api/v1/runs/{run}", api.OnRun{Permission: api.RunRead, Reveals: "run:everything"}},
	} {
		if err := rt.Handle("GET", c.pattern, c.guard, ok); err == nil {
			t.Errorf("%s was registered revealing more to a permission nobody documents", c.pattern)
		}
	}
	for _, route := range rt.Routes() {
		want := api.Permission("")
		if route.Pattern == "/api/v1/{namespace}/runs/{run}" || route.Pattern == "/api/v1/{namespace}/facade" {
			want = api.RunReadData
		}
		if route.Reveals != want {
			t.Errorf("the surface lists %s as revealing more to %q", route.Pattern, route.Reveals)
		}
	}
}
