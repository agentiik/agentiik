package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// Reading runs by the identifiers a client holds: every run it can read, wherever it is; one run by
// its identifier alone; one output's envelope; one artifact by its URI.

// granted allows each principal the permissions it lists, each over a whole namespace where the
// target names no workflow and over one workflow where it does, and nothing else: the shape of
// "access is granted by binding a principal to a role, either on the whole namespace or on a single
// workflow", which a listing across namespaces asks about one workflow at a time.
type granted map[api.Principal][]grant

type grant struct {
	what api.Permission
	over api.Target
}

func (g granted) Allow(_ context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	for _, h := range g[who] {
		if h.what == what && h.over.Namespace == over.Namespace && (h.over.Workflow == "" || h.over.Workflow == over.Workflow) {
			return true, nil
		}
	}
	return false, nil
}

// someRuns is an installation holding runs in two namespaces, of two workflows in one of them.
type someRuns struct {
	pool    *db.Pool
	super   string
	store   *version.Store
	objects artifact.Objects
	signed  *artifact.Signed

	// dir is where objects keeps its bytes, for a test that has to put others there.
	dir string

	// finance are the runs of finance/monthly-invoicing, oldest first, and payroll and teamOps
	// the one run of finance/payroll and of team-ops/monthly-invoicing.
	finance []string
	payroll string
	teamOps string
}

func withSomeRuns(t *testing.T) someRuns {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	store, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	objects := artifact.Dir(dir)
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := someRuns{pool: pool, super: super, store: store, objects: objects, signed: signed, dir: dir}

	h := s.servedTo(t, everything{who: "admin"})
	start := func(namespace, workflow string) string {
		t.Helper()
		if w, _ := call(t, h, "PUT", "/api/v1/"+namespace+"/workflows/"+workflow+"/versions/"+aCommit, "admin", aPush(t)); w.Code != http.StatusOK {
			t.Fatalf("the push answered %d: %s", w.Code, w.Body)
		}
		w, started := call(t, h, "POST", "/api/v1/"+namespace+"/workflows/"+workflow+"/runs", "admin",
			api.Start{Commit: aCommit, Inputs: map[string]any{"orders": []any{}}})
		if w.Code != http.StatusAccepted {
			t.Fatalf("starting a run answered %d: %s", w.Code, w.Body)
		}
		return started["run"].(string)
	}
	s.finance = []string{start("finance", "monthly-invoicing"), start("finance", "monthly-invoicing")}
	s.payroll = start("finance", "payroll")
	s.teamOps = start("team-ops", "monthly-invoicing")

	// Created a minute apart, oldest first, so that newest first is an order a listing has to
	// keep rather than one it gets from runs created in the same instant.
	for i, run := range []string{s.finance[0], s.finance[1], s.payroll, s.teamOps} {
		s.sql(t, `update runs set created_at = $2 where id = $1`, run, aMinute(i))
	}
	return s
}

// aMinute is the moment the i-th run was created.
func aMinute(i int) time.Time {
	return time.Date(2026, 9, 24, 6, i, 0, 0, time.UTC)
}

func (s someRuns) sql(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := dbtest.Superuser(t, s.super).Exec(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

// servedTo is the API over the installation, deciding with auth, with the object routes a redirect
// leads to beside it.
func (s someRuns) servedTo(t *testing.T, auth api.Authorizer) http.Handler {
	t.Helper()
	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: s.pool, Versions: s.store, Objects: s.objects, URLs: s.signed}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, s.signed); err != nil {
		t.Fatal(err)
	}
	return rt
}

// listed is the runs a listing across the installation answered, by identifier and in order, with
// the namespace each is in.
func listed(t *testing.T, h http.Handler, as, query string) []string {
	t.Helper()
	return listedAt(t, h, as, "/api/v1/runs"+query)
}

// listedAt is listed for a listing at any path.
func listedAt(t *testing.T, h http.Handler, as, path string) []string {
	t.Helper()
	w, _ := call(t, h, "GET", path, as, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("listing %s answered %d: %s", path, w.Code, w.Body)
	}
	var answer struct {
		Runs []struct {
			Namespace string `json:"namespace"`
			Run       string `json:"run"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range answer.Runs {
		out = append(out, r.Namespace+"/"+r.Run)
	}
	return out
}

func same(a, b []string) bool {
	return strings.Join(a, " ") == strings.Join(b, " ")
}

// "GET /api/v1/runs: Runs, filtered by namespace, workflow, state and time", across every namespace
// the caller can read. run:read held on a namespace lists every workflow's runs there, held on one
// workflow lists that workflow's, and anything else lists nothing; a namespace the caller cannot read
// lists what one that does not exist lists.
func TestRunsAreListedAcrossEveryNamespaceTheCallerCanRead(t *testing.T) {
	s := withSomeRuns(t)
	h := s.servedTo(t, granted{
		"alice": {{api.RunRead, api.Target{Namespace: "finance"}}},
		"bob": {
			{api.RunRead, api.Target{Namespace: "team-ops", Workflow: "monthly-invoicing"}},
			{api.RunRead, api.Target{Namespace: "finance", Workflow: "payroll"}},
		},
		"olivia": {
			{api.WorkflowRun, api.Target{Namespace: "finance"}},
			{api.RunReadData, api.Target{Namespace: "team-ops"}},
		},
	})

	for _, c := range []struct {
		as, query string
		want      []string
	}{
		{"alice", "", []string{"finance/" + s.payroll, "finance/" + s.finance[1], "finance/" + s.finance[0]}},
		{"bob", "", []string{"team-ops/" + s.teamOps, "finance/" + s.payroll}},
		{"olivia", "", []string{}},
		{"alice", "?workflow=payroll", []string{"finance/" + s.payroll}},
		{"alice", "?namespace=finance&workflow=monthly-invoicing", []string{"finance/" + s.finance[1], "finance/" + s.finance[0]}},
		{"bob", "?namespace=finance", []string{"finance/" + s.payroll}},
		{"bob", "?workflow=monthly-invoicing", []string{"team-ops/" + s.teamOps}},
	} {
		if got := listed(t, h, c.as, c.query); !same(got, c.want) {
			t.Errorf("%s listing %q was answered %v, want %v", c.as, c.query, got, c.want)
		}
	}

	// A namespace she cannot read answers what one nobody made answers, body and all.
	refused, _ := call(t, h, "GET", "/api/v1/runs?namespace=team-ops", "alice", nil)
	absent, _ := call(t, h, "GET", "/api/v1/runs?namespace=nowhere", "alice", nil)
	if refused.Code != http.StatusOK || refused.Body.String() != absent.Body.String() {
		t.Errorf("a namespace she cannot read answered %d %s, and one that does not exist %s", refused.Code, refused.Body, absent.Body)
	}

	if w, _ := call(t, h, "GET", "/api/v1/runs", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a caller with no credential answered %d", w.Code)
	}
}

// The namespaced routes answer a namespace's runs per workflow, as the ones across the installation
// do, rather than per namespace: "a workflow-scope grant only adds; only an explicit deny removes,
// and it wins over any allow at any scope". run:read held on the namespace and denied on one
// workflow lists and reads none of that workflow's runs, and a run of it reads exactly as a run
// nobody started; run:read held on one workflow alone lists and reads that workflow's runs there;
// and a namespace the caller holds nothing in lists what one that does not exist lists, whatever
// its query names.
func TestANamespacesRunsAreReadPerWorkflow(t *testing.T) {
	s := withSomeRuns(t)
	finance := api.Target{Namespace: "finance"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	h := s.servedTo(t, denying{
		allowed: granted{
			"alice": {{api.RunRead, finance}},
			"bob":   {{api.RunRead, payroll}},
			"carol": {{api.RunRead, api.Target{Namespace: "team-ops"}}},
		},
		denied: granted{"alice": {{api.RunRead, payroll}}},
	})

	for _, c := range []struct {
		as, path string
		want     []string
	}{
		{"alice", "/api/v1/finance/runs", []string{"finance/" + s.finance[1], "finance/" + s.finance[0]}},
		{"alice", "/api/v1/finance/runs?workflow=payroll", []string{}},
		{"bob", "/api/v1/finance/runs", []string{"finance/" + s.payroll}},
		{"bob", "/api/v1/finance/runs?workflow=monthly-invoicing", []string{}},
		{"carol", "/api/v1/finance/runs", []string{}},
		{"carol", "/api/v1/finance/runs?namespace=team-ops", []string{}},
		{"carol", "/api/v1/team-ops/runs", []string{"team-ops/" + s.teamOps}},
	} {
		if got := listedAt(t, h, c.as, c.path); !same(got, c.want) {
			t.Errorf("%s listing %s was answered %v, want %v", c.as, c.path, got, c.want)
		}
	}
	unreadable, _ := call(t, h, "GET", "/api/v1/finance/runs", "carol", nil)
	nowhere, _ := call(t, h, "GET", "/api/v1/nowhere/runs", "carol", nil)
	if unreadable.Body.String() != nowhere.Body.String() {
		t.Errorf("a namespace she holds nothing in lists %s, and one that does not exist %s", unreadable.Body, nowhere.Body)
	}

	absent, _ := call(t, h, "GET", "/api/v1/finance/runs/01M2ZZZZZZZZZZZZZZZZZZZZZZ", "alice", nil)
	if absent.Code != http.StatusNotFound {
		t.Fatalf("a run nobody started answered %d", absent.Code)
	}
	for _, c := range []struct {
		as, run string
		read    bool
	}{
		{"alice", s.finance[0], true},
		{"alice", s.payroll, false},
		{"bob", s.payroll, true},
		{"bob", s.finance[0], false},
		{"carol", s.finance[0], false},
	} {
		w, detail := call(t, h, "GET", "/api/v1/finance/runs/"+c.run, c.as, nil)
		switch {
		case c.read && (w.Code != http.StatusOK || detail["run"] != c.run):
			t.Errorf("%s reading run %s was answered %d: %s", c.as, c.run, w.Code, w.Body)
		case !c.read && (w.Code != absent.Code || w.Body.String() != absent.Body.String()):
			t.Errorf("%s, who cannot read run %s, was answered %d %s, and a run nobody started %d %s", c.as, c.run, w.Code, w.Body, absent.Code, absent.Body)
		}
	}
}

// A listing is narrowed by state, by when its runs were created, both bounds included, and by a
// limit, newest first throughout; a state or a time that is none is refused, and a name no
// namespace could have lists nothing rather than failing.
func TestAListingIsNarrowedByStateAndTime(t *testing.T) {
	s := withSomeRuns(t)
	h := s.servedTo(t, everything{who: "alice"})
	s.sql(t, `update runs set state = 'succeeded', started_at = $2, finished_at = $2 where id = $1`, s.finance[0], aMinute(0))

	at := func(i int) string { return url.QueryEscape(aMinute(i).Format(time.RFC3339)) }
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"?state=succeeded", []string{"finance/" + s.finance[0]}},
		{"?state=queued&namespace=finance", []string{"finance/" + s.payroll, "finance/" + s.finance[1]}},
		{"?limit=2", []string{"team-ops/" + s.teamOps, "finance/" + s.payroll}},
		{"?since=" + at(1) + "&until=" + at(2), []string{"finance/" + s.payroll, "finance/" + s.finance[1]}},
		{"?until=" + at(0), []string{"finance/" + s.finance[0]}},
		{"?since=" + at(3), []string{"team-ops/" + s.teamOps}},
		{"?namespace=%00", []string{}},
		{"?workflow=%ff", []string{}},
	} {
		if got := listed(t, h, "alice", c.query); !same(got, c.want) {
			t.Errorf("listing %q was answered %v, want %v", c.query, got, c.want)
		}
	}
	for _, query := range []string{"?state=finished", "?since=yesterday", "?until=2026-09-24"} {
		if w, _ := call(t, h, "GET", "/api/v1/runs"+query, "alice", nil); w.Code != http.StatusBadRequest {
			t.Errorf("listing %q answered %d: %s", query, w.Code, w.Body)
		}
	}
}

// "GET /api/v1/runs/{id}: Run state, per-step state, envelope digests", by the identifier alone,
// which is all a push notification carries. It answers what the namespaced route answers, the
// namespace included, to whoever holds run:read on the run's workflow, and a run the caller cannot
// read answers what a run nobody started answers.
func TestARunIsReadByItsIdentifierAlone(t *testing.T) {
	s := withSomeRuns(t)
	run := s.finance[0]
	h := s.servedTo(t, granted{
		"alice":  {{api.RunRead, api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}}},
		"bob":    {{api.RunRead, api.Target{Namespace: "finance", Workflow: "payroll"}}, {api.RunRead, api.Target{Namespace: "team-ops"}}},
		"olivia": {{api.WorkflowRun, api.Target{Namespace: "finance"}}, {api.RunReadData, api.Target{Namespace: "finance"}}},
		"admin":  {{api.RunRead, api.Target{Namespace: "finance"}}},
	})

	w, detail := call(t, h, "GET", "/api/v1/runs/"+run, "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the run read by its identifier answered %d: %s", w.Code, w.Body)
	}
	if detail["namespace"] != "finance" || detail["run"] != run || detail["workflow"] != "monthly-invoicing" {
		t.Errorf("the run reads %v", detail)
	}
	namespaced, _ := call(t, h, "GET", "/api/v1/finance/runs/"+run, "admin", nil)
	if namespaced.Code != http.StatusOK || namespaced.Body.String() != w.Body.String() {
		t.Errorf("the namespaced route answered %d %s, and by identifier %s", namespaced.Code, namespaced.Body, w.Body)
	}

	absent, _ := call(t, h, "GET", "/api/v1/runs/01M2ZZZZZZZZZZZZZZZZZZZZZZ", "alice", nil)
	for _, as := range []string{"bob", "olivia"} {
		refused, _ := call(t, h, "GET", "/api/v1/runs/"+run, as, nil)
		if refused.Code != http.StatusNotFound || refused.Body.String() != absent.Body.String() {
			t.Errorf("%s, who cannot read the run, was answered %d %s, and a run nobody started %d %s", as, refused.Code, refused.Body, absent.Code, absent.Body)
		}
	}
	for _, id := range []string{"%00", "%ff", "not-a-run"} {
		if w, _ := call(t, h, "GET", "/api/v1/runs/"+id, "alice", nil); w.Code != http.StatusNotFound {
			t.Errorf("a run named %s answered %d: %s", id, w.Code, w.Body)
		}
	}
}

// denying is granted with a deny beside it, which wins over any allow at any scope, as a deny does:
// "Grants add up; a deny is the only thing that subtracts."
type denying struct {
	allowed, denied granted
}

func (d denying) Allow(ctx context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	if denied, _ := d.denied.Allow(ctx, who, what, over); denied {
		return false, nil
	}
	return d.allowed.Allow(ctx, who, what, over)
}

// failingOn is an authorizer that cannot answer about one permission and answers as another does
// about the rest.
type failingOn struct {
	api.Authorizer
	what api.Permission
}

func (f failingOn) Allow(ctx context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	if what == f.what {
		return false, context.DeadlineExceeded
	}
	return f.Authorizer.Allow(ctx, who, what, over)
}

// "run:read: See run state, per-step state, timings and log lines", and "run:read_data: See envelope
// contents and download artifacts, not only state and digests." A run's inputs are what its first
// steps are handed as envelope contents, so a run read with run:read alone answers everything else
// and names no inputs, by either route, whether run:read is held on the namespace or on the
// workflow. run:read_data held on the namespace or on the run's own workflow shows them, held on
// another workflow does not, and denied on the run's workflow it withholds them even through the
// route authorised over the whole namespace. A question about it that could not be answered is a
// 500 rather than a guess either way, and no listing carries inputs to anybody.
func TestARunsInputsAreShownOnlyToWhoeverHoldsRunReadData(t *testing.T) {
	s := withSomeRuns(t)
	run := s.finance[0]
	finance := api.Target{Namespace: "finance"}
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	payroll := api.Target{Namespace: "finance", Workflow: "payroll"}
	allowed := granted{
		"alice": {{api.RunRead, finance}},
		"bob":   {{api.RunRead, invoicing}},
		"carol": {{api.RunRead, finance}, {api.RunReadData, finance}},
		"dave":  {{api.RunRead, invoicing}, {api.RunReadData, invoicing}},
		"erin":  {{api.RunRead, finance}, {api.RunReadData, invoicing}},
		"frank": {{api.RunRead, finance}, {api.RunReadData, payroll}},
		"grace": {{api.RunRead, finance}, {api.RunReadData, finance}},
	}
	h := s.servedTo(t, denying{allowed: allowed, denied: granted{"grace": {{api.RunReadData, invoicing}}}})

	byID, namespaced := "/api/v1/runs/"+run, "/api/v1/finance/runs/"+run
	_, whole := call(t, h, "GET", byID, "carol", nil)
	if inputs, _ := whole["inputs"].(map[string]any); len(inputs) != 1 || inputs["orders"] == nil {
		t.Fatalf("the run was started with inputs and reads %v", whole)
	}
	delete(whole, "inputs")
	for _, c := range []struct {
		as, path string
		shown    bool
	}{
		{"alice", byID, false},
		{"alice", namespaced, false},
		{"bob", byID, false},
		{"carol", byID, true},
		{"carol", namespaced, true},
		{"dave", byID, true},
		{"erin", byID, true},
		{"erin", namespaced, true},
		{"frank", byID, false},
		{"frank", namespaced, false},
		{"grace", byID, false},
		{"grace", namespaced, false},
	} {
		w, detail := call(t, h, "GET", c.path, c.as, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s reading %s was answered %d: %s", c.as, c.path, w.Code, w.Body)
			continue
		}
		_, named := detail["inputs"]
		if named != c.shown {
			t.Errorf("%s reading %s was answered the inputs %t, want %t: %s", c.as, c.path, named, c.shown, w.Body)
		}
		delete(detail, "inputs")
		if !reflect.DeepEqual(detail, whole) {
			t.Errorf("%s reading %s was answered %v, and the rest of the run is %v", c.as, c.path, detail, whole)
		}
	}

	broken := s.servedTo(t, failingOn{Authorizer: allowed, what: api.RunReadData})
	for _, path := range []string{byID, namespaced} {
		if w, _ := call(t, broken, "GET", path, "carol", nil); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "orders") {
			t.Errorf("reading %s where run:read_data could not be asked about was answered %d: %s", path, w.Code, w.Body)
		}
	}

	for _, path := range []string{"/api/v1/runs", "/api/v1/finance/runs"} {
		if w, _ := call(t, h, "GET", path, "carol", nil); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "inputs") || strings.Contains(w.Body.String(), "orders") {
			t.Errorf("listing %s was answered %d: %s", path, w.Code, w.Body)
		}
	}
}

// finished ends a run as succeeded, its archive step having published envelope on ok, which the
// workflow's output invoices is a view of, and puts the envelope where a runner would have.
func (s someRuns) finished(t *testing.T, run string, envelope agk.Envelope) string {
	t.Helper()
	digest, size, err := artifact.PutEnvelope(t.Context(), s.objects, "finance", envelope)
	if err != nil {
		t.Fatal(err)
	}
	ports, _ := json.Marshal(map[string]any{"ok": map[string]any{"digest": "sha256:" + digest, "size": size, "items": len(envelope.Items)}})
	outputs, _ := json.Marshal(map[string]any{"invoices": map[string]any{"step": "archive", "port": "ok", "count": len(envelope.Items)}})
	s.sql(t, `update steps set ports = $2, state = 'succeeded' where run_id = $1 and step = 'archive'`, run, ports)
	s.sql(t, `update runs set state = 'succeeded', started_at = now(), finished_at = now(), outputs = $2 where id = $1`, run, outputs)
	return digest
}

func anEnvelope(run string) agk.Envelope {
	return agk.Envelope{
		Meta: agk.Meta{RunID: agk.RunID(run), Step: "archive", Port: "ok", Attempt: 1, Count: 1, ProducedAt: aMinute(9)},
		Items: []agk.Item{{
			ID: "01JMZ8W4K7A1B2C3D4E5F6G7H8", Data: map[string]any{"customer_id": "C-1042", "total": 1290.5},
			Files: []agk.File{},
		}},
	}
}

// "GET /api/v1/runs/{id}/outputs/{name}: One workflow output's envelope. Requires run:read_data."
// run:read alone answers what a run nobody started answers; an output the run has not recorded,
// because it has not ended or the workflow declares no such output, is a 404 of its own; one whose
// envelope was purged is 410, since it existed and is finished.
func TestAWorkflowOutputIsItsEnvelope(t *testing.T) {
	s := withSomeRuns(t)
	run := s.finance[0]
	envelope := anEnvelope(run)
	s.finished(t, run, envelope)
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	h := s.servedTo(t, granted{
		"alice": {{api.RunReadData, invoicing}},
		"dave":  {{api.RunRead, invoicing}, {api.WorkflowRun, invoicing}},
	})

	w, _ := call(t, h, "GET", "/api/v1/runs/"+run+"/outputs/invoices", "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the output answered %d: %s", w.Code, w.Body)
	}
	var want bytes.Buffer
	envelope.Encode(&want)
	if w.Body.String() != want.String() || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("the output reads %s as %q, want %s", w.Body, w.Header().Get("Content-Type"), want.String())
	}

	absent, _ := call(t, h, "GET", "/api/v1/runs/01M2ZZZZZZZZZZZZZZZZZZZZZZ/outputs/invoices", "alice", nil)
	refused, _ := call(t, h, "GET", "/api/v1/runs/"+run+"/outputs/invoices", "dave", nil)
	if refused.Code != http.StatusNotFound || refused.Body.String() != absent.Body.String() {
		t.Errorf("dave, holding run:read and not run:read_data, was answered %d %s, and a run nobody started %s", refused.Code, refused.Body, absent.Body)
	}
	for _, path := range []string{
		"/api/v1/runs/" + run + "/outputs/receipts",
		"/api/v1/runs/" + run + "/outputs/%ff",
		"/api/v1/runs/" + s.finance[1] + "/outputs/invoices",
	} {
		if w, _ := call(t, h, "GET", path, "alice", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s answered %d: %s", path, w.Code, w.Body)
		}
	}

	s.sql(t, `update steps set envelopes_purged_at = now() where run_id = $1`, run)
	if w, _ := call(t, h, "GET", "/api/v1/runs/"+run+"/outputs/invoices", "alice", nil); w.Code != http.StatusGone {
		t.Errorf("an output whose envelope was purged answered %d: %s", w.Code, w.Body)
	}
}

// anArtifact records one artifact of run on archive/ok, with the fetch budget given, and puts its
// bytes in the store.
func (s someRuns) anArtifact(t *testing.T, run, name string, fetches int, content []byte) agk.URI {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if err := s.objects.Put(t.Context(), artifact.Key("finance", digest), bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	u := agk.URI{Run: agk.RunID(run), Step: "archive", Port: "ok", Name: name}
	if err := s.pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.WriteArtifact(ctx, db.Reference{URI: u, Digest: digest, Size: int64(len(content)), MediaType: "text/html", For: time.Hour, Fetches: fetches})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return u
}

func artifactPath(u agk.URI) string { return "/api/v1/artifacts/" + url.PathEscape(u.String()) }

// "no fetch budget: 302 to a short-lived presigned URL. The bytes never touch the control plane."
// The URL is one of the object route's, for this run, and works; the redirect is kept by no cache.
// An artifact that expired is 410, one never written is 404, and one of a run the caller holds
// run:read on and not run:read_data answers what one never written does.
func TestAnArtifactWithNoBudgetIsARedirect(t *testing.T) {
	s := withSomeRuns(t)
	run := s.finance[0]
	content := []byte("<script>alert(1)</script> invoice 2026-01")
	u := s.anArtifact(t, run, "invoice.html", 0, content)
	invoicing := api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	h := s.servedTo(t, granted{
		"alice": {{api.RunReadData, invoicing}},
		"dave":  {{api.RunRead, invoicing}},
	})

	before := time.Now()
	w, _ := call(t, h, "GET", artifactPath(u), "alice", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("an artifact with no budget answered %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the redirect may be cached: %q", got)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(location.String(), "https://agentiik.example.com/objects/finance/sha256/") {
		t.Fatalf("the redirect leads to %q", w.Header().Get("Location"))
	}
	if location.Query().Get("run") != run {
		t.Errorf("the URL is signed for run %q", location.Query().Get("run"))
	}
	expires, _ := strconv.ParseInt(location.Query().Get("expires"), 10, 64)
	if lasts := time.Unix(expires, 0).Sub(before); lasts <= 0 || lasts > 5*time.Minute+time.Second {
		t.Errorf("the URL works for %s, and a presigned URL for an artifact expires in minutes", lasts)
	}
	followed, _ := call(t, h, "GET", location.RequestURI(), "", nil)
	if followed.Code != http.StatusOK || followed.Body.String() != string(content) {
		t.Errorf("following the redirect answered %d %q", followed.Code, followed.Body)
	}

	absent, _ := call(t, h, "GET", artifactPath(agk.URI{Run: u.Run, Step: "archive", Port: "ok", Name: "nothing.pdf"}), "alice", nil)
	if absent.Code != http.StatusNotFound {
		t.Errorf("an artifact never written answered %d: %s", absent.Code, absent.Body)
	}
	refused, _ := call(t, h, "GET", artifactPath(u), "dave", nil)
	if refused.Code != http.StatusNotFound || refused.Body.String() != absent.Body.String() {
		t.Errorf("dave, holding run:read and not run:read_data, was answered %d %s, and an artifact never written %s", refused.Code, refused.Body, absent.Body)
	}
	elsewhere := agk.URI{Run: agk.RunID(s.teamOps), Step: "archive", Port: "ok", Name: "invoice.html"}
	if w, _ := call(t, h, "GET", artifactPath(elsewhere), "alice", nil); w.Code != http.StatusNotFound || w.Body.String() != absent.Body.String() {
		t.Errorf("an artifact of a run in another namespace answered %d %s", w.Code, w.Body)
	}

	// Past its duration it is gone, whether or not a sweep has retired it yet: "a duration
	// bounds how long they may be fetched".
	s.sql(t, `update artifacts set expires_at = now() - interval '1 second' where run_id = $1`, run)
	if w, _ := call(t, h, "GET", artifactPath(u), "alice", nil); w.Code != http.StatusGone {
		t.Errorf("an artifact past its duration answered %d: %s", w.Code, w.Body)
	}
	s.sql(t, `update artifacts set status = 'expired', retired_at = now() where run_id = $1`, run)
	if w, _ := call(t, h, "GET", artifactPath(u), "alice", nil); w.Code != http.StatusGone {
		t.Errorf("an expired artifact answered %d: %s", w.Code, w.Body)
	}
}

// failing is a client that goes away before the bytes arrive.
type failing struct{ *httptest.ResponseRecorder }

func (failing) Write([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// "budget left: The bytes themselves, served by the API. budget spent: 410 Gone." A fetch counts
// when the response completes: a HEAD fetches nothing and a transfer the client abandons spends
// nothing, while each whole one spends one. What is served is bytes a browser neither renders nor
// sniffs, since it is served from the API's own origin.
func TestAnArtifactWithABudgetIsServedAndCountedWhenItCompletes(t *testing.T) {
	s := withSomeRuns(t)
	content := []byte("<script>alert(1)</script> payslip 2026-01")
	u := s.anArtifact(t, s.finance[0], "payslip.html", 2, content)
	h := s.servedTo(t, everything{who: "alice"})
	request := func(method string) *http.Request {
		r := httptest.NewRequest(method, artifactPath(u), nil)
		r.Header.Set("Authorization", "Bearer alice")
		r.Header.Set("Range", "bytes=0-9")
		return r
	}

	head := httptest.NewRecorder()
	h.ServeHTTP(head, request("HEAD"))
	if head.Code != http.StatusOK || head.Header().Get("Content-Length") != strconv.Itoa(len(content)) {
		t.Errorf("a HEAD answered %d with %q", head.Code, head.Header().Get("Content-Length"))
	}
	h.ServeHTTP(failing{httptest.NewRecorder()}, request("GET"))

	for i := range 2 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request("GET"))
		if w.Code != http.StatusOK || w.Body.String() != string(content) {
			t.Fatalf("fetch %d of a budget of two answered %d %q", i+1, w.Code, w.Body)
		}
		for header, want := range map[string]string{
			"Content-Type":           "application/octet-stream",
			"X-Content-Type-Options": "nosniff",
			"Cache-Control":          "no-store",
			"Content-Disposition":    `attachment; filename=payslip.html`,
		} {
			if got := w.Header().Get(header); got != want {
				t.Errorf("%s is %q, want %q", header, got, want)
			}
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request("GET"))
	if w.Code != http.StatusGone {
		t.Errorf("a spent budget answered %d: %s", w.Code, w.Body)
	}

	// And one with fetches left is gone once its duration has run out, sweep or no sweep.
	lapsing := s.anArtifact(t, s.finance[0], "lapsing.txt", 3, []byte("payslip 2026-02"))
	s.sql(t, `update artifacts set expires_at = now() - interval '1 second' where name = 'lapsing.txt'`)
	if w, _ := call(t, h, "GET", artifactPath(lapsing), "alice", nil); w.Code != http.StatusGone {
		t.Errorf("a budget past its duration answered %d: %s", w.Code, w.Body)
	}
}

// Bytes that are not the ones the digest names are no fetch of the artifact: a store that handed
// back something else under its key has served nothing the budget was for, so the fetch is given
// back.
func TestBytesThatAreNotTheArtifactSpendNothing(t *testing.T) {
	s := withSomeRuns(t)
	content := []byte("payslip 2026-01")
	u := s.anArtifact(t, s.finance[0], "payslip.txt", 1, content)
	sum := sha256.Sum256(content)
	stored := filepath.Join(s.dir, filepath.FromSlash(artifact.Key("finance", hex.EncodeToString(sum[:]))))
	corrupt := bytes.ToUpper(content)
	if err := os.Remove(stored); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	h := s.servedTo(t, everything{who: "alice"})
	if w, _ := call(t, h, "GET", artifactPath(u), "alice", nil); w.Code != http.StatusOK || w.Body.String() != string(corrupt) {
		t.Fatalf("the fetch answered %d %q", w.Code, w.Body)
	}
	var left *int
	if err := dbtest.Superuser(t, s.super).QueryRow(t.Context(), `select fetches_left from artifacts where name = 'payslip.txt' and status = 'live'`).Scan(&left); err != nil || left == nil || *left != 1 {
		t.Errorf("bytes other than the artifact's spent its fetch: %v, %v", left, err)
	}
}

// leaving is a client that has everything and goes, as curl does: the request's context is
// cancelled the moment the last byte is written, before anything after it runs.
type leaving struct {
	*httptest.ResponseRecorder
	left, of int
	gone     context.CancelFunc
}

func (l *leaving) Write(b []byte) (int, error) {
	n, err := l.ResponseRecorder.Write(b)
	if l.left += n; l.left >= l.of {
		l.gone()
	}
	return n, err
}

// A client that has the whole artifact has spent a fetch, even where it closes its connection the
// moment the last byte arrives, which cancels the request before the fetch is recorded.
func TestAFetchReceivedWholeIsSpentWhenTheClientLeavesAtOnce(t *testing.T) {
	s := withSomeRuns(t)
	content := []byte("payslip 2026-01")
	u := s.anArtifact(t, s.finance[0], "payslip.txt", 1, content)
	h := s.servedTo(t, everything{who: "alice"})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, "GET", artifactPath(u), nil)
	r.Header.Set("Authorization", "Bearer alice")
	w := &leaving{ResponseRecorder: httptest.NewRecorder(), of: len(content), gone: cancel}
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != string(content) {
		t.Fatalf("the fetch answered %d %q", w.Code, w.Body)
	}
	if again, _ := call(t, h, "GET", artifactPath(u), "alice", nil); again.Code != http.StatusGone {
		t.Errorf("a one-shot artifact received whole answered %d the second time: %s", again.Code, again.Body)
	}
}

// stalling is a client that receives the first bytes and then waits until it is let go.
type stalling struct {
	*httptest.ResponseRecorder
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stalling) Write(b []byte) (int, error) {
	s.once.Do(func() {
		close(s.writing)
		<-s.release
	})
	return s.ResponseRecorder.Write(b)
}

// The last fetch of a budget is served once. Two fetches of it at once would each find one left
// before either counted it, and both be served, so the second is told at once, without waiting on
// the first, that it is being served, and once the first has completed a third is told the budget is
// spent.
func TestTheLastFetchOfABudgetIsServedOnce(t *testing.T) {
	s := withSomeRuns(t)
	content := []byte("payslip 2026-01")
	u := s.anArtifact(t, s.finance[0], "payslip.txt", 1, content)
	h := s.servedTo(t, everything{who: "alice"})
	request := func() *http.Request {
		r := httptest.NewRequest("GET", artifactPath(u), nil)
		r.Header.Set("Authorization", "Bearer alice")
		return r
	}

	first := &stalling{ResponseRecorder: httptest.NewRecorder(), writing: make(chan struct{}), release: make(chan struct{})}
	var done sync.WaitGroup
	done.Go(func() { h.ServeHTTP(first, request()) })
	<-first.writing

	second := httptest.NewRecorder()
	h.ServeHTTP(second, request())
	if second.Code != http.StatusConflict {
		t.Errorf("a second fetch of the last one, while the first was being served, answered %d %q", second.Code, second.Body)
	}
	asking := request()
	asking.Method = "HEAD"
	head := httptest.NewRecorder()
	h.ServeHTTP(head, asking)
	if head.Code != http.StatusConflict {
		t.Errorf("a HEAD of the last one, while it was being served, answered %d where a GET answers 409", head.Code)
	}
	close(first.release)
	done.Wait()

	if first.Code != http.StatusOK || first.Body.String() != string(content) {
		t.Errorf("the first fetch answered %d %q", first.Code, first.Body)
	}
	third := httptest.NewRecorder()
	h.ServeHTTP(third, request())
	if third.Code != http.StatusGone {
		t.Errorf("a fetch of a budget of one once it was served answered %d %q", third.Code, third.Body)
	}
}
