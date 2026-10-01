package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// ended ends a run as the controller would, the steps named succeeded and the rest left as they are.
func ended(t *testing.T, super, run string, succeeded ...string) {
	t.Helper()
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(),
		`update runs set state = 'succeeded', started_at = coalesce(started_at, now()), finished_at = now() where id = $1`, run); err != nil {
		t.Fatal(err)
	}
	for _, step := range succeeded {
		if _, err := conn.Exec(t.Context(),
			`update steps set state = 'succeeded', started_at = now(), finished_at = now() where run_id = $1 and step = $2`, run, step); err != nil {
			t.Fatal(err)
		}
	}
}

// "POST /api/v1/runs/{id}/replay: Replays from a named step, reusing everything upstream." A replay
// is a new run of the commit the run it replays pinned, over its inputs, naming the run and the
// step; it is recorded as run.trigger; and it is refused while the run goes on, from a step the
// version does not have, from a step above which something never ended or was cancelled with its
// run, and from any step once the run is replayable from the start only.
func TestARunIsReplayedFromAStep(t *testing.T) {
	o := withOneRun(t)
	h := o.servedTo(t, everything{who: "admin"})
	replay := "/api/v1/runs/" + o.run + "/replay"

	if w, _ := call(t, h, "POST", replay, "admin", api.Replay{Step: "archive"}); w.Code != http.StatusConflict {
		t.Errorf("a replay of a run still going answered %d: %s", w.Code, w.Body)
	}
	ended(t, o.super, o.run, "normalize", "archive")

	w, answer := call(t, h, "POST", replay, "admin", api.Replay{Step: "archive"})
	if w.Code != http.StatusAccepted || answer["commit"] != aCommit || answer["replay_of"] != o.run || answer["replay_from"] != "archive" {
		t.Fatalf("a replay from archive answered %d: %s", w.Code, w.Body)
	}
	again, _ := answer["run"].(string)
	if again == "" || again == o.run || !strings.HasSuffix(w.Header().Get("Location"), "/runs/"+again) {
		t.Errorf("the replay is run %q at %s", again, w.Header().Get("Location"))
	}
	w, detail := call(t, h, "GET", "/api/v1/runs/"+again, "admin", nil)
	if w.Code != http.StatusOK || detail["replay_of"] != o.run || detail["replay_from"] != "archive" || detail["commit"] != aCommit || detail["state"] != "queued" {
		t.Errorf("the replay reads %d: %s", w.Code, w.Body)
	}
	if inputs, _ := detail["inputs"].(map[string]any); inputs == nil || inputs["orders"] == nil {
		t.Errorf("the replay's inputs are not the run's: %v", detail["inputs"])
	}
	got := audited(t, o.pool)
	last := got[len(got)-1]
	if d := detailOf(t, last); last.Action != audit.RunTrigger || last.Target != again || d["replay_of"] != o.run || d["step"] != "archive" || d["commit"] != aCommit {
		t.Errorf("the replay is recorded as %s on %s with %v", last.Action, last.Target, d)
	}

	// From the start, it names the run and no step.
	w, answer = call(t, h, "POST", replay, "admin", nil)
	if w.Code != http.StatusAccepted || answer["replay_of"] != o.run || answer["replay_from"] != nil {
		t.Errorf("a replay from the start answered %d: %s", w.Code, w.Body)
	}

	if w, _ := call(t, h, "POST", replay, "admin", api.Replay{Step: "nothing"}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a replay from a step the version lacks answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, h, "POST", replay, "admin", `{"step":"archive","reason":"x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a replay asked with a field nobody knows answered %d: %s", w.Code, w.Body)
	}
	conn := dbtest.Superuser(t, o.super)
	if _, err := conn.Exec(t.Context(), `update steps set state = 'pending', started_at = null, finished_at = null where run_id = $1 and step = 'normalize'`, o.run); err != nil {
		t.Fatal(err)
	}
	if w, _ := call(t, h, "POST", replay, "admin", api.Replay{Step: "archive"}); w.Code != http.StatusConflict {
		t.Errorf("a replay from a step whose step above never ended answered %d: %s", w.Code, w.Body)
	}
	// Nor from below a step the run's ending cancelled, which published nothing to reuse.
	if _, err := conn.Exec(t.Context(), `update steps set state = 'cancelled', started_at = now(), finished_at = now() where run_id = $1 and step = 'normalize'`, o.run); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `update runs set state = 'cancelled' where id = $1`, o.run); err != nil {
		t.Fatal(err)
	}
	if w, _ := call(t, h, "POST", replay, "admin", api.Replay{Step: "archive"}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "step normalize, above archive, did not finish") {
		t.Errorf("a replay from a step whose step above was cancelled with its run answered %d: %s", w.Code, w.Body)
	}
	ended(t, o.super, o.run, "normalize")
	if _, err := conn.Exec(t.Context(), `update runs set replay_from_start_only = true where id = $1`, o.run); err != nil {
		t.Fatal(err)
	}
	if w, _ := call(t, h, "POST", replay, "admin", api.Replay{Step: "archive"}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "from the start only") {
		t.Errorf("a replay from a step of a run replayable from the start only answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, h, "POST", replay, "admin", nil); w.Code != http.StatusAccepted {
		t.Errorf("a replay from the start of a run replayable from the start only answered %d: %s", w.Code, w.Body)
	}
}

// "A run, and a replay of it months later, reads the commit it started from, whatever the branch
// does next": the run pinned the head of main, main moved on, and the replay is of the commit the run
// pinned and not of main.
func TestAReplayIsOfTheCommitTheRunPinnedAfterTheBranchMovedOn(t *testing.T) {
	g := servingGit(t, holdingRepositories())
	work := g.newClone("alice")
	work.write("agentiik.yaml", workflowDocument)
	first := work.commit("first")
	work.must("push", "-q", "origin", "main")
	const at = "/api/v1/finance/workflows/monthly-invoicing"
	w, started := call(t, g.h, "POST", at+"/runs", "alice", map[string]any{"inputs": map[string]any{"orders": []any{}}})
	if w.Code != http.StatusAccepted || started["commit"] != first {
		t.Fatalf("the run answered %d: %s", w.Code, w.Body)
	}
	run := started["run"].(string)

	work.write("scripts/later.sh", "true\n")
	later := work.commit("later")
	work.must("push", "-q", "origin", "main")
	if later == first {
		t.Fatal("the branch did not move")
	}
	ended(t, g.super, run, "normalize", "archive")

	w, answer := call(t, g.h, "POST", "/api/v1/runs/"+run+"/replay", "alice", api.Replay{Step: agk.Step("archive")})
	if w.Code != http.StatusAccepted || answer["commit"] != first {
		t.Errorf("the replay answered %d, of the commit %v and not %s: %s", w.Code, answer["commit"], first, w.Body)
	}
}
