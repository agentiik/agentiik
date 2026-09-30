package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// Events published to a namespace: "the API matches the event, in the request, against every event
// trigger armed in the namespace it was published into, and every one armed in another namespace
// that names it".

// publishing is hooking, with carol, who pushes team-ops' workflow, and the returned map, to which a
// test adds what a namespace grants another's built-in identity.
func publishing() (api.Authorizer, granted) {
	g := hooking().(granted)
	teamOps := api.Target{Namespace: "team-ops"}
	g["carol"] = []grant{{api.WorkflowRead, teamOps}, {api.WorkflowWrite, teamOps}}
	// One who may start the workflow's runs, and not publish into its namespace, since "a grant on
	// one workflow does not count".
	g["finance/one-workflow"] = []grant{{api.WorkflowRun, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}}
	return g, g
}

// servingEvents is a repository of finance whose default branch's head declares on, pushed.
func servingEvents(t *testing.T, on string) (*gitServer, granted) {
	t.Helper()
	auth, g := publishing()
	s := servingGit(t, auth, func(o *api.ServerOptions) { o.Hooks = clearHooks{} })
	work := s.newClone("alice")
	work.write("agentiik.yaml", triggered(on))
	work.commit("listening")
	work.must("push", "-q", "origin", "main")
	return s, g
}

// publish publishes body into finance as who, with header, and answers the recorder.
func (g *gitServer) publish(who, contentType, body string, header ...string) *httptest.ResponseRecorder {
	g.t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+who)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return g.deliver(delivery{method: "POST", path: "/api/v1/finance/events", body: body, header: h})
}

// structured is an event written whole, as application/cloudevents+json.
func structured(t *testing.T, attributes map[string]any) string {
	t.Helper()
	event := map[string]any{"specversion": "1.0", "id": "a3f9c1e", "source": "/erp/orders", "type": "com.example.order.approved"}
	for k, v := range attributes {
		if v == nil {
			delete(event, k)
			continue
		}
		event[k] = v
	}
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const cloudEvent = "application/cloudevents+json"

// runsAnswered is the count a publication is answered with.
func runsAnswered(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	if w.Code != http.StatusAccepted {
		t.Fatalf("the event was answered %d: %s", w.Code, w.Body)
	}
	var answer struct {
		Runs *int `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || answer.Runs == nil {
		t.Fatalf("the event was answered %s", w.Body)
	}
	return *answer.Runs
}

// An event published starts a run of each trigger hearing it, attributed to the namespace's
// built-in identity, its inputs filled by map and the event frozen on it; published again within
// the day it is answered as the first was and starts nothing; and one no trigger hears starts none.
func TestAnEventStartsARunOfEachTriggerHearingIt(t *testing.T) {
	g, _ := servingEvents(t, `  event:
    - type: com.example.order.approved
      map:
        orders: ${{ event.data.orders }}
        cycle: ${{ event.subject }}
`)
	body := structured(t, map[string]any{"subject": "2026-09", "data": map[string]any{"orders": []any{"o-1", "o-2"}}, "comexampletenant": "acme"})
	if n := runsAnswered(t, g.publish("finance/deployer", cloudEvent, body)); n != 1 {
		t.Fatalf("the event started %d runs, and one trigger hears it", n)
	}
	runs := g.started()
	if len(runs) != 1 || !strings.Contains(runs[0], " event finance/agentiik ") ||
		!strings.Contains(runs[0], `"orders": ["o-1", "o-2"]`) || !strings.Contains(runs[0], `"cycle": "2026-09"`) {
		t.Fatalf("the runs are %q", runs)
	}

	var frozen string
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(), `select (trigger_context -> 'event')::text from runs`).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(frozen), &event); err != nil {
		t.Fatal(err)
	}
	if event["id"] != "a3f9c1e" || event["subject"] != "2026-09" || event["specversion"] != "1.0" || event["data"] == nil {
		t.Errorf("the event frozen on the run is %s", frozen)
	}
	if _, kept := event["comexampletenant"]; kept {
		t.Errorf("an extension attribute was kept on the run: %s", frozen)
	}

	var detail string
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(),
		`select actor || ' ' || (detail::jsonb - 'workflow' - 'commit')::text from audit_log where action = 'run.trigger'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if want := `finance/agentiik {"id": "a3f9c1e", "source": "/erp/orders", "publisher": "finance/deployer", "published_in": "finance", "trigger_kind": "event"}`; detail != want {
		t.Errorf("run.trigger records %s, want %s", detail, want)
	}

	// The same source and id: answered with what the first started, and nothing more.
	if n := runsAnswered(t, g.publish("finance/deployer", cloudEvent, body)); n != 1 {
		t.Errorf("the event published again was answered %d runs, and the first started one", n)
	}
	if runs := g.started(); len(runs) != 1 {
		t.Errorf("the event published again started a run: %q", runs)
	}
	// Another id is another event, and another type one nobody hears.
	if n := runsAnswered(t, g.publish("finance/deployer", cloudEvent, structured(t, map[string]any{"id": "b7c2", "subject": "2026-10", "data": map[string]any{"orders": []any{}}}))); n != 1 {
		t.Errorf("another event started %d runs", n)
	}
	if n := runsAnswered(t, g.publish("finance/deployer", cloudEvent, structured(t, map[string]any{"id": "c9d4", "type": "com.example.order.cancelled"}))); n != 0 {
		t.Errorf("an event nobody hears started %d runs", n)
	}
	if runs := g.started(); len(runs) != 2 {
		t.Errorf("the runs are %q", runs)
	}
}

// "It is one CloudEvents 1.0 event in either of the modes its HTTP binding defines", and what the
// specification refuses, a batch and data that is neither JSON nor text are refused.
func TestAnEventIsPublishedInEitherModeAndRefusedOtherwise(t *testing.T) {
	g, _ := servingEvents(t, `  event:
    - type: com.example.order.approved
      map:
        orders: ${{ event.data.orders }}
`)
	binary := g.publish("finance/deployer", "application/json", `{"orders": ["o-1"]}`,
		"ce-specversion", "1.0", "ce-id", "b-1", "ce-source", "/erp/orders", "ce-type", "com.example.order.approved", "ce-time", "2026-09-30T10:00:00Z")
	if n := runsAnswered(t, binary); n != 1 {
		t.Fatalf("an event in the binary mode started %d runs", n)
	}
	var datacontenttype, at string
	if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(),
		`select trigger_context -> 'event' ->> 'datacontenttype', trigger_context -> 'event' ->> 'time' from runs`).Scan(&datacontenttype, &at); err != nil {
		t.Fatal(err)
	}
	if datacontenttype != "application/json" || at != "2026-09-30T10:00:00Z" {
		t.Errorf("the event frozen reads datacontenttype %q and time %q", datacontenttype, at)
	}

	for name, c := range map[string]struct {
		contentType, body string
		header            []string
		want              int
	}{
		"a batch":                   {"application/cloudevents-batch+json", "[" + structured(t, nil) + "]", nil, http.StatusUnsupportedMediaType},
		"data in base64":            {cloudEvent, structured(t, map[string]any{"data_base64": "AAEC"}), nil, http.StatusUnsupportedMediaType},
		"binary data":               {"application/octet-stream", "\xff\xfe", []string{"ce-specversion", "1.0", "ce-id", "b-2", "ce-source", "/erp/orders", "ce-type", "com.example.order.approved"}, http.StatusUnsupportedMediaType},
		"no id":                     {cloudEvent, structured(t, map[string]any{"id": nil}), nil, http.StatusBadRequest},
		"another specversion":       {cloudEvent, structured(t, map[string]any{"specversion": "0.3"}), nil, http.StatusBadRequest},
		"a time not RFC 3339":       {cloudEvent, structured(t, map[string]any{"time": "yesterday"}), nil, http.StatusBadRequest},
		"an attribute not a string": {cloudEvent, structured(t, map[string]any{"subject": 12}), nil, http.StatusBadRequest},
		"an id past its bound":      {cloudEvent, structured(t, map[string]any{"id": strings.Repeat("i", 256)}), nil, http.StatusBadRequest},
		"no event at all":           {"application/json", `{"orders": []}`, nil, http.StatusBadRequest},
		"two documents":             {cloudEvent, structured(t, nil) + structured(t, nil), nil, http.StatusBadRequest},
	} {
		if w := g.publish("finance/deployer", c.contentType, c.body, c.header...); w.Code != c.want {
			t.Errorf("%s was answered %d, want %d: %s", name, w.Code, c.want, w.Body)
		}
	}
	if runs := g.started(); len(runs) != 1 {
		t.Errorf("a refused event started a run: %q", runs)
	}
}

// eventFiring is what the event trigger of finance's workflow records of its last firing.
func (g *gitServer) eventFiring() (firedRun, skipped string) {
	g.t.Helper()
	if err := dbtest.Superuser(g.t, g.super).QueryRow(g.t.Context(),
		`select coalesce(fired_run, ''), coalesce(skipped, '') from triggers where kind = 'event' and namespace = 'finance'`).Scan(&firedRun, &skipped); err != nil {
		g.t.Fatal(err)
	}
	return firedRun, skipped
}

// A filter answering false starts nothing and records nothing; one that fails to evaluate, and a map
// whose inputs are refused, start nothing either and are recorded on the trigger as a skipped firing.
func TestAFilterDecidesAndWhatStartsNothingIsRecorded(t *testing.T) {
	g, _ := servingEvents(t, `  event:
    - type: com.example.order.approved
      filter: ${{ event.data.amount > 0 }}
      map:
        cycle: ${{ event.data.cycle }}
`)
	publish := func(id string, data map[string]any) int {
		return runsAnswered(t, g.publish("finance/deployer", cloudEvent, structured(t, map[string]any{"id": id, "data": data})))
	}
	if n := publish("f-1", map[string]any{"amount": 0, "cycle": "2026-09"}); n != 0 {
		t.Errorf("an event the filter refuses started %d runs", n)
	}
	if run, skipped := g.eventFiring(); run != "" || skipped != "" {
		t.Errorf("an event the filter refuses recorded %q, %q", run, skipped)
	}
	if n := publish("f-2", map[string]any{"amount": "much", "cycle": "2026-09"}); n != 0 {
		t.Errorf("an event the filter cannot read started %d runs", n)
	}
	if _, skipped := g.eventFiring(); !strings.Contains(skipped, "the filter could not be evaluated over the event") {
		t.Errorf("a filter failing recorded %q", skipped)
	}
	if n := publish("f-3", map[string]any{"amount": 5, "cycle": 12}); n != 0 {
		t.Errorf("an event whose inputs are refused started %d runs", n)
	}
	if _, skipped := g.eventFiring(); !strings.Contains(skipped, "no run could be started") {
		t.Errorf("inputs refused recorded %q", skipped)
	}
	if n := publish("f-4", map[string]any{"amount": 5, "cycle": "2026-09"}); n != 1 {
		t.Errorf("an event the filter accepts started %d runs", n)
	}
	if run, skipped := g.eventFiring(); run == "" || skipped != "" {
		t.Errorf("the run started recorded %q, %q", run, skipped)
	}
}

// Another namespace's events are heard where the trigger names that namespace and that namespace
// grants the listening namespace's built-in identity workflow:read, "so that a grant revoked stops the
// events at once"; and a namespace naming none hears its own alone.
func TestAnotherNamespacesEventsAreHeardWhereItGrantsRead(t *testing.T) {
	g, grants := servingEvents(t, `  event:
    - type: com.example.order.approved
`)
	if err := g.pool.In(t.Context(), "team-ops", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		_, err := ns.RecordImages(ctx, "monthly-invoicing", "carol", time.Time{}, db.Images{Manifests: map[string][]byte{image: []byte(brickManifest)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	work := g.newClone("carol")
	work.must("remote", "set-url", "origin", strings.Replace(g.url, "http://", "http://agk:carol@", 1)+"/team-ops/monthly-invoicing.git")
	work.write("agentiik.yaml", strings.Replace(triggered(`  event:
    - type: com.example.order.approved
      namespace: finance
    - type: com.example.order.approved
`), "namespace: finance }", "namespace: team-ops }", 1))
	work.commit("listening to finance")
	work.must("push", "-q", "origin", "main")

	namespaces := func() string {
		var out string
		if err := dbtest.Superuser(t, g.super).QueryRow(t.Context(),
			`select coalesce(string_agg(namespace || ' ' || triggered_by, ', ' order by namespace), '') from runs`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if n := runsAnswered(t, g.publish("finance/deployer", cloudEvent, structured(t, map[string]any{"id": "x-1"}))); n != 1 {
		t.Errorf("finance granting team-ops nothing, the event started %d runs", n)
	}
	if got := namespaces(); got != "finance finance/agentiik" {
		t.Errorf("the runs are %q", got)
	}

	grants["team-ops/agentiik"] = []grant{{api.WorkflowRead, api.Target{Namespace: "finance"}}}
	if n := runsAnswered(t, g.publish("finance/deployer", cloudEvent, structured(t, map[string]any{"id": "x-2"}))); n != 2 {
		t.Errorf("finance granting team-ops read, the event started %d runs, and two triggers hear it", n)
	}
	if got := namespaces(); got != "finance finance/agentiik, finance finance/agentiik, team-ops team-ops/agentiik" {
		t.Errorf("the runs are %q", got)
	}

	// A grant on one workflow is not read access to the namespace's events.
	grants["team-ops/agentiik"] = []grant{{api.WorkflowRead, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}}
	if n := runsAnswered(t, g.publish("finance/deployer", cloudEvent, structured(t, map[string]any{"id": "x-3"}))); n != 1 {
		t.Errorf("finance granting team-ops read on one workflow, the event started %d runs", n)
	}
}

// Publishing takes workflow:run at namespace scope, "since an event reaches every workflow
// listening to the namespace": one held on a single workflow is not it.
func TestPublishingTakesWorkflowRunOnTheNamespace(t *testing.T) {
	g, _ := servingEvents(t, `  event:
    - type: com.example.order.approved
`)
	for who, want := range map[string]int{"finance/one-workflow": http.StatusNotFound, "bob": http.StatusNotFound, "finance/deployer": http.StatusAccepted} {
		if w := g.publish(who, cloudEvent, structured(t, map[string]any{"id": "p-" + who})); w.Code != want {
			t.Errorf("%s publishing was answered %d, want %d: %s", who, w.Code, want, w.Body)
		}
	}
}
