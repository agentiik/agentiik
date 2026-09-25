package e2e

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
)

// killedWorkflow is testdata/killed/agentiik.yaml with image, the sleep brick as it was pushed,
// written where the file names sleep.
func killedWorkflow(t *testing.T, image string) string {
	t.Helper()
	doc, err := os.ReadFile("testdata/killed/agentiik.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(doc), "image: sleep\n", "image: "+image+"\n")
}

// dispatch is one row of the tasks table as the test reads it: one dispatch of one key.
type dispatch struct {
	id, key, step, state, runner string
	attempt, requeue             int

	// finished is when the dispatch ended, which for a lost one is when the sweep declared it.
	finished *time.Time

	// heard is the last thing its runner said of it, its last heartbeat or its redemption,
	// which is what the sweep counts three intervals from.
	heard *time.Time

	// redeemed is when a grant of this dispatch's own was last redeemed.
	redeemed *time.Time
}

// dispatches reads every dispatch of run, by step, each step's in the order they were handed out.
func (in *Installation) dispatches(run string) map[string][]dispatch {
	in.t.Helper()
	rows, err := in.Database().Query(in.ctx, `
		select t.id::text, t.idempotency_key, t.step::text, t.state::text, coalesce(t.runner, ''),
		       t.attempt, t.requeue, t.finished_at,
		       greatest(t.last_heartbeat_at, g.redeemed_at), g.redeemed_at
		from tasks t
		left join lateral (
		  select max(redeemed_at) as redeemed_at from task_grants
		  where namespace = t.namespace and task_id = t.id) g on true
		where t.namespace = $1 and t.run_id = $2
		order by t.step, t.attempt, t.requeue`, Namespace, run)
	if err != nil {
		in.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]dispatch{}
	for rows.Next() {
		var d dispatch
		if err := rows.Scan(&d.id, &d.key, &d.step, &d.state, &d.runner, &d.attempt, &d.requeue, &d.finished, &d.heard, &d.redeemed); err != nil {
			in.t.Fatal(err)
		}
		out[d.step] = append(out[d.step], d)
	}
	if err := rows.Err(); err != nil {
		in.t.Fatal(err)
	}
	return out
}

// sweepMargin is how long past three silent intervals a loss may take to be declared: the
// controller sweeps every ten seconds, and a pass takes a moment of its own.
const sweepMargin = 20 * time.Second

// Issue #162: a runner killed mid-step leaves a run that resumes from the state it was left in,
// with nobody rescuing it.
//
// A three-step workflow is started, and once one runner's daemon runs the middle step's container
// the test SIGKILLs that runner's agent and leaves its daemon up, with the container still running
// on it. What the documentation says happens next is held against the tasks table and against
// both daemons: the dispatch is declared lost three heartbeat intervals after its runner last
// spoke of it; the key is requeued under a new task_id, which the other runner redeems with a
// grant of that dispatch's own and runs on its own daemon; the run succeeds; the first step ran
// once, one task row and one container, since a requeue hands out the lost key and nothing
// before it; and the killed key has two dispatches, having spent one of max_requeues.
func TestARunnerKilledMidStepLosesItsDispatchAndTheRunSucceedsOnTheOther(t *testing.T) {
	in := Stand(t)

	tag, pinned := in.Brick("sleep")
	manifest, err := os.ReadFile("testdata/bricks/sleep/brick.yaml")
	if err != nil {
		t.Fatal(err)
	}
	commit := randomHex(20)
	in.Push("killed", commit, killedWorkflow(t, tag), map[string][]byte{tag: manifest}, map[string]string{tag: pinned})

	// A second early, since a daemon's events are asked for by the second.
	since := time.Now().Add(-time.Second)
	run := in.Start("killed", commit, map[string]any{
		"orders": []any{map[string]any{"ref": "a"}, map[string]any{"ref": "b"}},
	})

	// The middle step's container running on one of the two daemons, which is the runner killed.
	var killed, other *Runner
	var key string
	eventually(in.ctx, t, 3*time.Minute, "a runner's daemon ran the step held", func() error {
		for i, r := range in.Runners {
			running, err := r.Running(in.ctx, run)
			if err != nil {
				return err
			}
			if keys := running["held"]; len(keys) > 0 {
				killed, other, key = r, in.Runners[1-i], keys[0]
				return nil
			}
		}
		return errors.New("neither daemon runs it yet")
	})
	if err := killed.Kill(in.ctx); err != nil {
		t.Fatal(err)
	}
	killedAt := time.Now()
	t.Logf("runner %s (%s) was killed at %s while its daemon ran %s", killed.Name, killed.ID, killedAt.UTC().Format(time.RFC3339Nano), key)

	// Its daemon is up, and the container with it: the agent is what died, and nothing told
	// the daemon.
	if running, err := killed.Running(in.ctx, run); err != nil || !slices.Contains(running["held"], key) {
		t.Fatalf("runner %s's daemon runs %v of the run once its agent was killed (%v), and the daemon was left up with the container on it", killed.Name, running, err)
	}

	// Nobody touches the run from here: it is read, and nothing else.
	ended := in.Wait(run, 5*time.Minute)
	if ended.State != "succeeded" {
		t.Fatalf("run %s ended %s: %s", run, ended.State, ended.Answer)
	}

	all := in.dispatches(run)

	// The key held has two dispatches: the one lost with the runner killed, and its requeue.
	held := all["held"]
	if len(held) != 2 {
		t.Fatalf("step held has %d dispatches, want the one lost and its requeue: %+v", len(held), held)
	}
	lost, again := held[0], held[1]
	if lost.key != key || again.key != key || lost.attempt != 1 || again.attempt != 1 {
		t.Errorf("the dispatches of step held are %s attempt %d and %s attempt %d, and a requeue keeps the key %s and its attempt: a loss spends no retry.max attempt", lost.key, lost.attempt, again.key, again.attempt, key)
	}
	if lost.state != "lost" || lost.runner != killed.ID || lost.requeue != 0 {
		t.Errorf("the first dispatch of %s is %+v, want lost, bound to runner %s, requeue 0", key, lost, killed.ID)
	}
	if again.state != "succeeded" || again.runner != other.ID || again.requeue != 1 {
		t.Errorf("the second dispatch of %s is %+v, want succeeded, bound to runner %s, requeue 1", key, again, other.ID)
	}
	if again.id == lost.id {
		t.Errorf("the requeue of %s kept the task_id %s, and a requeue takes a new one", key, lost.id)
	}

	// Lost three heartbeat intervals after the killed runner last spoke of it, and not long
	// after that: the controller's sweep is what notices a silence.
	switch {
	case lost.finished == nil || lost.heard == nil:
		t.Errorf("the lost dispatch records no end or nothing its runner said of it: %+v", lost)
	case lost.finished.Sub(*lost.heard) < db.LostAfter:
		t.Errorf("the dispatch was declared lost %s after its runner last spoke of it, and three missed intervals are %s", lost.finished.Sub(*lost.heard), db.LostAfter)
	case lost.finished.After(killedAt.Add(db.LostAfter + sweepMargin)):
		t.Errorf("the dispatch was declared lost %s after its runner was killed, and three missed intervals and a sweep are within %s", lost.finished.Sub(killedAt), db.LostAfter+sweepMargin)
	}

	// Redeemed by the other runner with a grant of the requeue's own, once the loss was
	// declared: the grant the killed runner redeemed is its dispatch's and opens nothing else.
	switch {
	case lost.redeemed == nil:
		t.Errorf("the lost dispatch records no redemption, and runner %s ran its container", killed.Name)
	case again.redeemed == nil:
		t.Errorf("the requeue records no redemption of a grant of its own, and runner %s ran it", other.Name)
	case lost.finished != nil && again.redeemed.Before(*lost.finished):
		t.Errorf("the requeue was redeemed at %s, before the loss it answers was declared at %s", again.redeemed, lost.finished)
	}

	// One of max_requeues spent, and no more.
	if again.requeue >= graph.DefaultMaxRequeues {
		t.Errorf("the requeue is the %d of its key, and max_requeues allows %d", again.requeue, graph.DefaultMaxRequeues)
	}

	// Each step around it ran once: one row, on whichever runner took it, and after is
	// necessarily the survivor's.
	before, after := all["before"], all["after"]
	if len(before) != 1 || before[0].state != "succeeded" {
		t.Errorf("step before has the dispatches %+v, want one that succeeded: a requeue hands out the lost key and nothing upstream of it", before)
	}
	if len(after) != 1 || after[0].state != "succeeded" || after[0].runner != other.ID {
		t.Errorf("step after has the dispatches %+v, want one that succeeded on runner %s", after, other.ID)
	}

	// And the daemons agree: the first step's container was created once, on either; the held
	// key's once on each, the killed runner's and then the other's.
	created := map[string]map[string][]string{}
	for _, r := range in.Runners {
		c, err := r.Created(in.ctx, run, since)
		if err != nil {
			t.Fatal(err)
		}
		created[r.Name] = c
	}
	if n := len(created[killed.Name]["before"]) + len(created[other.Name]["before"]); len(before) == 1 && n != 1 {
		t.Errorf("the daemons created %d containers for step before (%v), want one", n, created)
	}
	for _, r := range []*Runner{killed, other} {
		if got := created[r.Name]["held"]; !slices.Equal(got, []string{key}) {
			t.Errorf("runner %s's daemon created %q for step held, want one container for %s", r.Name, got, key)
		}
	}
}

func TestTheKilledWorkflowLoadsWithItsMiddleStepRequeuedOnALoss(t *testing.T) {
	const image = "registry:5000/agk-e2e/sleep:test"
	tree := fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(killedWorkflow(t, image))}}
	wf, err := graph.Load(tree, "agentiik.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, step := range wf.Steps {
		if step.Image != image || !slices.Equal(step.RunsOn, []string{Label}) {
			t.Errorf("step %s names %q on %q, and every step is the sleep brick on the runners' label %s", name, step.Image, step.RunsOn, Label)
		}
	}
	if held, ok := wf.Steps["held"]; !ok {
		t.Fatal("the workflow has no step held")
	} else if !held.Idempotent || !slices.Contains(held.Retry.On, agk.FailureLost) {
		t.Errorf("step held is %+v, and a lost dispatch is requeued only where the step is idempotent and retry.on names lost", held)
	}
}
