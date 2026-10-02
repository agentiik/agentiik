package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// A namespace's variables: written whole, one at a time, by whoever holds workflow:write there, read
// by whoever holds workflow:read there, and read by a run, when it is created, as its namespace
// shows them to its workflow, under its file's own.

// withVariables is the variable routes over a database holding finance and team-ops.
func withVariables(t *testing.T, auth api.Authorizer) (http.Handler, *db.Pool, string) {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	rt := router(t, auth)
	if _, err := api.NewVariables(rt, api.VariableOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	return rt, pool, super
}

// A variable is created, read in the listing and alone, replaced and removed, each value as it was
// written: a string, a list, null, an int kept an int and an exponent written out as the double it
// is.
func TestAVariableIsWrittenReadBackAndRemoved(t *testing.T) {
	h, _, _ := withVariables(t, everything{who: "alice"})

	w := sent(t, h, "PUT", "/api/v1/finance/variables/ledger_url", "alice", `{"value": "https://ledger.example.com/api", "visibility": "all"}`)
	if w.Code != http.StatusCreated || w.Header().Get("Location") != "/api/v1/finance/variables/ledger_url" {
		t.Fatalf("creating a variable answered %d at %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	if created["name"] != "ledger_url" || created["value"] != "https://ledger.example.com/api" || created["visibility"] != "all" || created["updated_by"] != "alice" || created["updated_at"] == nil {
		t.Errorf("the variable was answered as %v", created)
	}
	if _, named := created["workflows"]; named {
		t.Errorf("a variable every workflow reads was answered naming workflows: %v", created)
	}

	for name, body := range map[string]string{
		"reminder_days": `{"value": [7, 14, 30], "visibility": "selected", "workflows": ["payment-reminders", "monthly-invoicing"]}`,
		"rounding":      `{"value": null, "visibility": "all"}`,
		"threshold":     `{"value": 1e3, "visibility": "all"}`,
	} {
		if w := sent(t, h, "PUT", "/api/v1/finance/variables/"+name, "alice", body); w.Code != http.StatusCreated {
			t.Fatalf("creating %s answered %d: %s", name, w.Code, w.Body)
		}
	}

	w = sent(t, h, "GET", "/api/v1/finance/variables", "alice", "")
	if w.Code != http.StatusOK {
		t.Fatalf("the listing answered %d: %s", w.Code, w.Body)
	}
	for _, want := range []string{
		`"name":"reminder_days","value":[7,14,30],"visibility":"selected","workflows":["monthly-invoicing","payment-reminders"]`,
		`"name":"rounding","value":null,"visibility":"all"`,
		`"name":"threshold","value":1000.0,"visibility":"all"`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("the listing does not hold %s: %s", want, w.Body)
		}
	}
	var listing struct {
		Variables []api.NamespaceVariable `json:"variables"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range listing.Variables {
		names = append(names, v.Name)
	}
	if !slices.Equal(names, []string{"ledger_url", "reminder_days", "rounding", "threshold"}) {
		t.Errorf("the listing names %q, by name", names)
	}

	// Replaced whole, selected for none, which keeps it aside, and answered 200.
	w = sent(t, h, "PUT", "/api/v1/finance/variables/reminder_days", "alice", `{"value": 45, "visibility": "selected", "workflows": []}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"value":45,"visibility":"selected","workflows":[]`) {
		t.Fatalf("replacing a variable answered %d: %s", w.Code, w.Body)
	}
	if w := sent(t, h, "GET", "/api/v1/finance/variables/reminder_days", "alice", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"workflows":[]`) {
		t.Errorf("the replaced variable reads %d: %s", w.Code, w.Body)
	}
	// Another namespace holds none of finance's.
	if w := sent(t, h, "GET", "/api/v1/team-ops/variables/reminder_days", "alice", ""); w.Code != http.StatusNotFound {
		t.Errorf("finance's variable read in team-ops answered %d: %s", w.Code, w.Body)
	}

	if w := sent(t, h, "DELETE", "/api/v1/finance/variables/reminder_days", "alice", ""); w.Code != http.StatusNoContent {
		t.Fatalf("removing a variable answered %d: %s", w.Code, w.Body)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if w := sent(t, h, method, "/api/v1/finance/variables/reminder_days", "alice", ""); w.Code != http.StatusNotFound {
			t.Errorf("%s of a removed variable answered %d", method, w.Code)
		}
	}
	if w := sent(t, h, "DELETE", "/api/v1/finance/variables/rounding", "alice", `{"why": "x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a removal carrying a body answered %d: %s", w.Code, w.Body)
	}
}

// A variable written in part, or past what one may be, is refused before anything is written,
// with 400 for what is wrong and 413 for what is larger than the route reads.
func TestAVariableWrittenInPartIsRefused(t *testing.T) {
	h, pool, _ := withVariables(t, everything{who: "alice"})
	many := make([]string, 1025)
	for i := range many {
		many[i] = fmt.Sprintf("w%d", i)
	}
	manyNames, _ := json.Marshal(many)
	for _, c := range []struct {
		why, name, body string
		want            int
	}{
		{"a name with a dot", "ledger.url", `{"value": 1, "visibility": "all"}`, 400},
		{"a name past 255 characters", strings.Repeat("v", 256), `{"value": 1, "visibility": "all"}`, 400},
		{"no value", "x", `{"visibility": "all"}`, 400},
		{"no visibility", "x", `{"value": 1}`, 400},
		{"a visibility of no kind", "x", `{"value": 1, "visibility": "some"}`, 400},
		{"workflows beside all", "x", `{"value": 1, "visibility": "all", "workflows": ["payroll"]}`, 400},
		{"an empty list beside all", "x", `{"value": 1, "visibility": "all", "workflows": []}`, 400},
		{"selected naming no list", "x", `{"value": 1, "visibility": "selected"}`, 400},
		{"selected naming null", "x", `{"value": 1, "visibility": "selected", "workflows": null}`, 400},
		{"a workflow's name off its grammar", "x", `{"value": 1, "visibility": "selected", "workflows": ["two words"]}`, 400},
		{"a workflow named twice", "x", `{"value": 1, "visibility": "selected", "workflows": ["payroll", "payroll"]}`, 400},
		{"a field nobody reads", "x", `{"value": 1, "visibility": "all", "note": "x"}`, 400},
		{"U+0000 in a string", "x", `{"value": "a\u0000b", "visibility": "all"}`, 400},
		{"a number no float holds", "x", `{"value": 1e400, "visibility": "all"}`, 400},
		{"a body after the document", "x", `{"value": 1, "visibility": "all"} {}`, 400},
		{"a value past 64 KiB", "x", `{"value": "` + strings.Repeat("v", 64<<10) + `", "visibility": "all"}`, 413},
		{"workflows past 1,024", "x", `{"value": 1, "visibility": "selected", "workflows": ` + string(manyNames) + `}`, 413},
		{"a body past 512 KiB", "x", `{"value": "` + strings.Repeat("v", 600<<10) + `", "visibility": "all"}`, 413},
	} {
		if w := sent(t, h, "PUT", "/api/v1/finance/variables/"+c.name, "alice", c.body); w.Code != c.want {
			t.Errorf("%s answered %d, want %d: %s", c.why, w.Code, c.want, w.Body)
		}
	}
	// A value just within the bound is taken.
	if w := sent(t, h, "PUT", "/api/v1/finance/variables/x", "alice", `{"value": "`+strings.Repeat("v", 64<<10-2)+`", "visibility": "all"}`); w.Code != http.StatusCreated {
		t.Errorf("a value of 64 KiB answered %d", w.Code)
	}
	var held []db.Variable
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		held, err = ns.Variables(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Bytes != 64<<10 {
		t.Errorf("the refusals left the namespace holding %d variables", len(held))
	}
}

// Reading the variables takes workflow:read at namespace scope and writing one workflow:write
// there; neither secret:write nor a grant on one workflow is either. Each principal is sent the four
// against a variable that exists, since a refusal answers 404 as an absence does.
func TestTheVariablesNeedWhatTheirRoutesSay(t *testing.T) {
	finance := api.Target{Namespace: "finance"}
	const (
		list   = "GET /api/v1/finance/variables"
		read   = "GET /api/v1/finance/variables/currency"
		write  = "PUT /api/v1/finance/variables/currency"
		remove = "DELETE /api/v1/finance/variables/currency"
	)
	refused := map[string]int{list: 404, read: 404, write: 404, remove: 404}
	for _, c := range []struct {
		who  string
		auth api.Authorizer
		want map[string]int
	}{
		{"nobody", api.DenyAll{}, refused},
		{"bob", holder{who: "bob", what: api.WorkflowRead, over: finance}, map[string]int{list: 200, read: 200, write: 404, remove: 404}},
		{"carol", holder{who: "carol", what: api.WorkflowWrite, over: finance}, map[string]int{list: 404, read: 404, write: 200, remove: 204}},
		{"dave", holder{who: "dave", what: api.SecretWrite, over: finance}, refused},
		{"erin", holder{who: "erin", what: api.WorkflowRead, over: api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}, refused},
	} {
		h, pool, _ := withVariables(t, c.auth)
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			_, _, err := ns.SetVariable(ctx, db.Variable{Name: "currency", Value: json.RawMessage(`"EUR"`), Bytes: 5, Values: 1, Visibility: "all", UpdatedBy: "alice"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, route := range []string{list, read, write, remove} {
			method, path, _ := strings.Cut(route, " ")
			body := ""
			if method == "PUT" {
				body = `{"value": "CHF", "visibility": "all"}`
			}
			if w := sent(t, h, method, path, c.who, body); w.Code != c.want[route] {
				t.Errorf("%s: %s answered %d, want %d", c.who, route, w.Code, c.want[route])
			}
		}
	}
}

// A write is recorded as variable.write with its visibility, its workflows, whether it was created
// and the SHA-256 of its value written as compact JSON, never the value; a removal as
// variable.delete.
func TestAVariableWriteIsAuditedWithoutItsValue(t *testing.T) {
	h, pool, _ := withVariables(t, everything{who: "alice"})
	for _, body := range []string{
		`{"value": {"zone": "eu-west", "hosts": ["a", "b"]}, "visibility": "all"}`,
		`{"value": {"hosts" : [ "a" , "b" ], "zone":"eu-west"}, "visibility": "selected", "workflows": ["payroll"]}`,
	} {
		if w := sent(t, h, "PUT", "/api/v1/finance/variables/ledger", "alice", body); w.Code >= 300 {
			t.Fatalf("writing answered %d: %s", w.Code, w.Body)
		}
	}
	if w := sent(t, h, "DELETE", "/api/v1/finance/variables/ledger", "alice", ""); w.Code != http.StatusNoContent {
		t.Fatalf("removing answered %d: %s", w.Code, w.Body)
	}
	sum := sha256.Sum256([]byte(`{"hosts":["a","b"],"zone":"eu-west"}`))
	want := hex.EncodeToString(sum[:])
	entries := audited(t, pool)
	if len(entries) != 3 {
		t.Fatalf("three acts appended %d entries", len(entries))
	}
	for i, e := range entries[:2] {
		d := detailOf(t, e)
		if e.Action != audit.VariableWrite || e.Target != "ledger" || e.Actor != "alice" || e.Namespace != "finance" || d["sha256"] != want || d["created"] != (i == 0) {
			t.Errorf("write %d is recorded as %s %s by %s in %s with %v", i, e.Action, e.Target, e.Actor, e.Namespace, d)
		}
		if strings.Contains(e.Detail, "eu-west") {
			t.Errorf("write %d recorded the value: %s", i, e.Detail)
		}
	}
	if d := detailOf(t, entries[0]); d["visibility"] != "all" || d["workflows"] != nil {
		t.Errorf("the first write is recorded with %v", d)
	}
	if d := detailOf(t, entries[1]); d["visibility"] != "selected" || fmt.Sprint(d["workflows"]) != "[payroll]" {
		t.Errorf("the second write is recorded with %v", d)
	}
	if e := entries[2]; e.Action != audit.VariableDelete || e.Target != "ledger" || e.Namespace != "finance" {
		t.Errorf("the removal is recorded as %s %s in %s", e.Action, e.Target, e.Namespace)
	}
}

// A namespace holding as many variables as it may takes no more, 409 with nothing written, and
// still takes one replaced.
func TestANamespaceFullOfVariablesTakesNoMore(t *testing.T) {
	h, _, super := withVariables(t, everything{who: "alice"})
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`insert into namespace_variables (namespace, name, value, value_bytes, value_count, visibility, updated_by)
		 select 'finance', 'v' || i, 'true', 4, 1, 'all', 'alice' from generate_series(1, $1) i`, db.VariablesMax); err != nil {
		t.Fatal(err)
	}
	if w := sent(t, h, "PUT", "/api/v1/finance/variables/one_more", "alice", `{"value": 1, "visibility": "all"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "nothing was written") {
		t.Errorf("one variable too many answered %d: %s", w.Code, w.Body)
	}
	if w := sent(t, h, "PUT", "/api/v1/finance/variables/v1", "alice", `{"value": false, "visibility": "all"}`); w.Code != http.StatusOK {
		t.Errorf("a variable replaced in a full namespace answered %d: %s", w.Code, w.Body)
	}
}

// keptBy is what a run kept of its namespace's variables, as the column holds it.
func keptBy(t *testing.T, super, run string) string {
	t.Helper()
	var kept *string
	if err := dbtest.Superuser(t, super).QueryRow(t.Context(),
		`select namespace_vars::text from runs where id = $1`, run).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept == nil {
		return "null"
	}
	return *kept
}

// A run reads, when it is created, the variables its namespace shows its workflow, less those its
// file writes, and keeps them; a replay reads what the run read, however the variables changed
// since, and a run started after the change reads them as they stand.
func TestARunKeepsTheVariablesItsNamespaceShowedIt(t *testing.T) {
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt := router(t, everything{who: "alice"})
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: pool, Versions: store, Objects: artifact.Dir(t.TempDir())}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewVariables(rt, api.VariableOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	const at = "/api/v1/finance/workflows/monthly-invoicing"
	if w, _ := call(t, rt, "PUT", at+"/versions/"+aCommit, "alice", aPushOf(t, workflowDocument+"vars:\n  currency: EUR\n")); w.Code != http.StatusOK {
		t.Fatalf("the push answered %d: %s", w.Code, w.Body)
	}
	start := func() string {
		t.Helper()
		w, started := call(t, rt, "POST", at+"/runs", "alice", api.Start{Commit: aCommit, Inputs: map[string]any{"orders": []any{}}})
		if w.Code != http.StatusAccepted {
			t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
		}
		return started["run"].(string)
	}

	// Before any variable: the run keeps nothing, and its replay, later, reads nothing either.
	bare := start()
	for name, body := range map[string]string{
		"currency":     `{"value": "CHF", "visibility": "all"}`,
		"ledger_url":   `{"value": "https://ledger.example.com/api", "visibility": "all"}`,
		"dunning_days": `{"value": 45, "visibility": "selected", "workflows": ["monthly-invoicing"]}`,
		"payroll_day":  `{"value": 25, "visibility": "selected", "workflows": ["payroll"]}`,
	} {
		if w := sent(t, rt, "PUT", "/api/v1/finance/variables/"+name, "alice", body); w.Code != http.StatusCreated {
			t.Fatalf("setting %s answered %d: %s", name, w.Code, w.Body)
		}
	}
	if got := keptBy(t, super, bare); got != "null" {
		t.Errorf("the run started before any variable kept %s", got)
	}
	run := start()
	const want = `{"ledger_url": "https://ledger.example.com/api", "dunning_days": 45}`
	if got := keptBy(t, super, run); got != want {
		t.Errorf("the run kept %s, want %s: currency is its file's, and payroll_day another workflow's", got, want)
	}

	// The variables change; the replays read what their runs read.
	sent(t, rt, "PUT", "/api/v1/finance/variables/ledger_url", "alice", `{"value": "https://elsewhere.example.com", "visibility": "all"}`)
	sent(t, rt, "DELETE", "/api/v1/finance/variables/dunning_days", "alice", "")
	for of, wants := range map[string]string{run: want, bare: "null"} {
		ended(t, super, of)
		w, answer := call(t, rt, "POST", "/api/v1/runs/"+of+"/replay", "alice", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("the replay answered %d: %s", w.Code, w.Body)
		}
		if got := keptBy(t, super, answer["run"].(string)); got != wants {
			t.Errorf("the replay of %s kept %s, and the run it replays read %s", of, got, wants)
		}
	}
	if got := keptBy(t, super, start()); got != `{"ledger_url": "https://elsewhere.example.com"}` {
		t.Errorf("a run started after the change kept %s", got)
	}
}

// A webhook's map reads the namespace's variables under vars beside the file's own, before any run
// exists, and the run it starts keeps the ones it read.
func TestAWebhooksMapReadsItsNamespacesVariables(t *testing.T) {
	g := servingHooks(t, `  webhook:
    - path: /invoicing
      auth: none
      map:
        orders: ${{ vars.seed }}
`)
	if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, _, err := ns.SetVariable(ctx, db.Variable{
			Name: "seed", Value: json.RawMessage(`[{"customer_id":"C-7"}]`), Bytes: 23, Values: 3,
			Visibility: "selected", Workflows: []string{"monthly-invoicing"}, UpdatedBy: "alice",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w := g.deliver(delivery{method: "POST", path: "/hooks/finance/invoicing", body: `{}`})
	if w.Code != http.StatusAccepted {
		t.Fatalf("the delivery answered %d: %s", w.Code, w.Body)
	}
	runs := g.started()
	if len(runs) != 1 || !strings.HasSuffix(runs[0], `{"orders": [{"customer_id": "C-7"}]}`) {
		t.Fatalf("the delivery started %q", runs)
	}
	run, _, _ := strings.Cut(runs[0], " ")
	if got := keptBy(t, g.super, run); got != `{"seed": [{"customer_id": "C-7"}]}` {
		t.Errorf("the run the webhook started kept %s", got)
	}
}

// An event trigger's filter and map read the namespace's variables of the workflow listening, before
// any run exists, and the run the event starts keeps the ones they read.
func TestAnEventsFilterReadsItsNamespacesVariables(t *testing.T) {
	g, _ := servingEvents(t, `  event:
    - type: com.example.order.approved
      filter: ${{ event.data.amount >= vars.threshold }}
      map:
        cycle: ${{ vars.cycle }}
`)
	for name, value := range map[string]string{"threshold": `100`, "cycle": `"2026-09"`} {
		if err := g.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			_, _, err := ns.SetVariable(ctx, db.Variable{Name: name, Value: json.RawMessage(value), Bytes: len(value), Values: 1, Visibility: "all", UpdatedBy: "alice"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(id string, amount int) int {
		return runsAnswered(t, g.publish("finance/deployer", cloudEvent, structured(t, map[string]any{"id": id, "data": map[string]any{"amount": amount}})))
	}
	if n := publish("v-1", 50); n != 0 {
		t.Errorf("an event below the namespace's threshold started %d runs", n)
	}
	if n := publish("v-2", 150); n != 1 {
		t.Fatalf("an event above the namespace's threshold started %d runs", n)
	}
	runs := g.started()
	if len(runs) != 1 || !strings.Contains(runs[0], `"cycle": "2026-09"`) {
		t.Fatalf("the event started %q", runs)
	}
	run, _, _ := strings.Cut(runs[0], " ")
	if got := keptBy(t, g.super, run); got != `{"cycle": "2026-09", "threshold": 100}` {
		t.Errorf("the run the event started kept %s", got)
	}
}
