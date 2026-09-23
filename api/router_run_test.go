package api_test

import (
	"context"
	"net/http"
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
