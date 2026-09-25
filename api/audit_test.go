package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The acts the audit log records in v0.2.0, each in the transaction of the act: "manual trigger,
// approval, cancellation, secret write, runner policy change, runner drain and revocation".

// audited is the whole audit log, verified, as the export would read it.
func audited(t *testing.T, pool *db.Pool) []audit.Entry {
	t.Helper()
	entries, err := pool.AuditTrail().After(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(entries); err != nil {
		t.Fatalf("the audit log does not verify: %s", err)
	}
	return entries
}

// detailOf is an entry's detail, read back.
func detailOf(t *testing.T, e audit.Entry) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(e.Detail), &d); err != nil {
		t.Fatalf("entry %d carries a detail that is not JSON: %s", e.Seq, e.Detail)
	}
	return d
}

// refuseAppends makes every append fail, as a database that cannot record one would, so that a test
// can see the act fail with it.
func refuseAppends(t *testing.T, super string) {
	t.Helper()
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(),
		`alter table audit_log add constraint no_appends check (false) not valid`); err != nil {
		t.Fatal(err)
	}
}

// A run started by hand and asked to cancel twice, once while it runs and once after it ended, is
// three entries in its namespace, each naming who asked and the run.
func TestARunStartedAndCancelledIsRecorded(t *testing.T) {
	o := withOneRun(t)
	h := o.servedTo(t, everything{who: "admin"})
	if w, _ := call(t, h, "POST", o.cancel(), "admin", nil); w.Code != http.StatusAccepted {
		t.Fatalf("cancelling answered %d: %s", w.Code, w.Body)
	}
	if _, err := dbtest.Superuser(t, o.super).Exec(t.Context(), `update runs set state = 'cancelled', finished_at = now() where id = $1`, o.run); err != nil {
		t.Fatal(err)
	}
	if w, _ := call(t, h, "POST", o.cancel(), "admin", nil); w.Code != http.StatusAccepted {
		t.Fatalf("cancelling again answered %d: %s", w.Code, w.Body)
	}

	got := audited(t, o.pool)
	if len(got) != 3 {
		t.Fatalf("a run started and cancelled twice made %d entries", len(got))
	}
	for i, want := range []struct{ action, result string }{
		{audit.RunTrigger, audit.Done}, {audit.RunCancel, audit.Done}, {audit.RunCancel, audit.Unchanged},
	} {
		e := got[i]
		if e.Action != want.action || e.Result != want.result || e.Actor != "admin" || e.Namespace != "finance" || e.Target != o.run {
			t.Errorf("entry %d is %s %s by %s in %q on %s, want %s %s on %s", e.Seq, e.Action, e.Result, e.Actor, e.Namespace, e.Target, want.action, want.result, o.run)
		}
		if d := detailOf(t, e); d["workflow"] != "monthly-invoicing" {
			t.Errorf("entry %d names the workflow %v", e.Seq, d["workflow"])
		}
	}
	if d := detailOf(t, got[0]); d["commit"] != aCommit {
		t.Errorf("the manual trigger names the commit %v", d["commit"])
	}
}

// A secret written, rotated and removed is three entries naming the secret and never its value.
func TestASecretWriteIsRecordedWithoutItsValue(t *testing.T) {
	store := &sealing{}
	h, pool := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Values: store})
	const value = "sk_live_never_in_the_log"
	for _, body := range []string{
		`{"provider":"builtin","value":"` + value + `"}`,
		`{"provider":"builtin","value":"` + value + `2"}`,
	} {
		if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", body); w.Code >= 300 {
			t.Fatalf("writing the secret answered %d: %s", w.Code, w.Body)
		}
	}
	if w := sent(t, h, "DELETE", "/api/v1/finance/secrets/billing", "alice", ""); w.Code != http.StatusNoContent {
		t.Fatalf("removing the secret answered %d: %s", w.Code, w.Body)
	}

	got := audited(t, pool)
	if len(got) != 3 {
		t.Fatalf("two writes and a removal made %d entries", len(got))
	}
	for i, action := range []string{audit.SecretWrite, audit.SecretWrite, audit.SecretDelete} {
		if e := got[i]; e.Action != action || e.Target != "billing" || e.Namespace != "finance" || e.Actor != "alice" {
			t.Errorf("entry %d is %s on %s in %s by %s, want %s", e.Seq, e.Action, e.Target, e.Namespace, e.Actor, action)
		}
		if strings.Contains(got[i].Detail, "sk_live") {
			t.Fatalf("entry %d carries the secret's value: %s", got[i].Seq, got[i].Detail)
		}
	}
	if d := detailOf(t, got[0]); d["provider"] != "builtin" || d["value_written"] != true || d["created"] != true {
		t.Errorf("the first write is recorded as %v", d)
	}
	if d := detailOf(t, got[1]); d["created"] != false {
		t.Errorf("the rotation is recorded as %v", d)
	}
}

// A pool created, a join token issued from it, and a runner drained and revoked are entries on the
// installation, and an order given again is recorded as having changed nothing. The join token is
// recorded by its identifier and never by itself.
func TestRunnerPolicyAndOrdersAreRecorded(t *testing.T) {
	h, pool := withRunners(t)
	if w, _ := call(t, h, "POST", "/api/v1/runner-pools", "admin", api.RunnerPool{Pool: api.Pool{
		Name: "gpu", Labels: []string{"gpu=a100"}, Namespaces: []string{"finance"}, Ceilings: &api.Ceilings{CPU: "8"},
	}}); w.Code != http.StatusCreated {
		t.Fatalf("creating a pool answered %d: %s", w.Code, w.Body)
	}
	w, issued := call(t, h, "POST", "/api/v1/runner-pools/gpu/join-tokens", "admin", api.Issue{Labels: []string{"gpu=a100"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("issuing a token answered %d: %s", w.Code, w.Body)
	}
	token := issued["join_token"].(map[string]any)

	runner, _ := joined(t, h, pool)
	for _, verb := range []string{"drain", "drain", "revoke", "revoke"} {
		if w, _ := call(t, h, "POST", "/api/v1/runners/"+runner+"/"+verb, "admin", api.Order{Reason: "retired"}); w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", verb, w.Code, w.Body)
		}
	}

	got := audited(t, pool)
	want := []struct{ action, target, result string }{
		{audit.RunnerPoolCreate, "gpu", audit.Done},
		{audit.JoinTokenIssue, token["id"].(string), audit.Done},
		{audit.RunnerDrain, runner, audit.Done},
		{audit.RunnerDrain, runner, audit.Unchanged},
		{audit.RunnerRevoke, runner, audit.Done},
		{audit.RunnerRevoke, runner, audit.Unchanged},
	}
	if len(got) != len(want) {
		t.Fatalf("the acts made %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		e := got[i]
		if e.Action != w.action || e.Target != w.target || e.Result != w.result || e.Namespace != "" || e.Actor != "admin" {
			t.Errorf("entry %d is %s %s on %s in %q by %s, want %s %s on %s on the installation", e.Seq, e.Action, e.Result, e.Target, e.Namespace, e.Actor, w.action, w.result, w.target)
		}
	}
	if strings.Contains(got[1].Detail, token["token"].(string)) {
		t.Fatalf("the join token is in the audit log: %s", got[1].Detail)
	}
	created := detailOf(t, got[0])["pool"].(map[string]any)
	if fmt.Sprint(created["namespaces"]) != "[finance]" || created["resource_ceilings"].(map[string]any)["cpu"] != "8" {
		t.Errorf("the pool is recorded as %v", created)
	}
	if d := detailOf(t, got[4]); d["reason"] != "retired" || d["results_accepted_until"] == nil {
		t.Errorf("the revocation is recorded as %v", d)
	}
}

// An act whose entry cannot be appended is not done: the audit log refusing is the act refused, so
// no act lands unrecorded.
func TestAnActThatCannotBeRecordedIsNotDone(t *testing.T) {
	t.Run("a cancellation", func(t *testing.T) {
		o := withOneRun(t)
		refuseAppends(t, o.super)
		if w, _ := call(t, o.servedTo(t, everything{who: "admin"}), "POST", o.cancel(), "admin", nil); w.Code != http.StatusInternalServerError {
			t.Fatalf("a cancellation that could not be recorded answered %d", w.Code)
		}
		if o.requested(t) != nil {
			t.Fatal("a cancellation that could not be recorded was written on the run")
		}
	})
	t.Run("a secret write", func(t *testing.T) {
		pool, super := dbtest.Open(t)
		if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
			t.Fatal(err)
		}
		rt := router(t, everything{who: "alice"})
		if _, err := api.NewDeclarations(rt, api.DeclarationOptions{Pool: pool, Values: &sealing{}}); err != nil {
			t.Fatal(err)
		}
		refuseAppends(t, super)
		if w := sent(t, rt, "PUT", "/api/v1/finance/secrets/billing", "alice", `{"provider":"builtin","value":"x"}`); w.Code != http.StatusInternalServerError {
			t.Fatalf("a secret write that could not be recorded answered %d", w.Code)
		}
		if w := sent(t, rt, "GET", "/api/v1/finance/secrets/billing", "alice", ""); w.Code != http.StatusNotFound {
			t.Fatalf("a secret write that could not be recorded left a declaration: %d", w.Code)
		}
	})
	t.Run("a revocation", func(t *testing.T) {
		h, pool, super := runnersOn(t, everything{who: "admin"})
		runner, _ := joined(t, h, pool)
		refuseAppends(t, super)
		if w, _ := call(t, h, "POST", "/api/v1/runners/"+runner+"/revoke", "admin", api.Order{Reason: "leaked"}); w.Code != http.StatusInternalServerError {
			t.Fatalf("a revocation that could not be recorded answered %d", w.Code)
		}
		if held := inventoried(t, h, runner); held["state"] != "ready" {
			t.Fatalf("a revocation that could not be recorded left the runner %v", held["state"])
		}
	})
}

// Runs started at once through the API are each recorded, one after another, in one chain.
func TestRunsStartedAtOnceAreRecordedInOneChain(t *testing.T) {
	o := withOneRun(t)
	h := o.servedTo(t, everything{who: "admin"})
	const runs = 24
	var wg sync.WaitGroup
	codes := make(chan int, runs)
	for range runs {
		wg.Go(func() {
			w, _ := call(t, h, "POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "admin",
				api.Start{Commit: aCommit, Inputs: map[string]any{"orders": []any{}}})
			codes <- w.Code
		})
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusAccepted {
			t.Fatalf("a run started beside others answered %d", code)
		}
	}
	if got := audited(t, o.pool); len(got) != runs+1 {
		t.Fatalf("%d runs started at once, and one before, made %d entries", runs, len(got))
	}
	var count int
	if err := o.pool.Installation(t.Context(), db.AuditLog, func(ctx context.Context, w *db.Wide) error {
		return w.VerifyAuditLog(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := dbtest.Superuser(t, o.super).QueryRow(t.Context(), `select count(*) from runs`).Scan(&count); err != nil || count != runs+1 {
		t.Fatalf("%d runs were created: %v", count, err)
	}
}
