package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// The run and its decision document, against a real PostgreSQL. What is being tested is the
// pair of guards that make two controllers deciding one run detectable: the sequence the
// document is written against, and the fact that everything of one pass lands together.

func created(t *testing.T) (*Pool, string) {
	t.Helper()
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, super
}

const theRun agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0B"

func aRun(steps ...agk.Step) NewRun {
	if len(steps) == 0 {
		steps = []agk.Step{"normalize", "invoice", "archive"}
	}
	return NewRun{
		ID: theRun, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Trigger: agk.TriggerSchedule, TriggeredBy: "cron",
		Inputs: map[string]any{"cycle": "2026-09"},
		Steps:  steps,
	}
}

// A run is created queued, with one row per step, and with no document: "Created, waiting on a
// concurrency lock or on namespace quota" is what queued means, and a document is what a
// decision leaves behind.
func TestARunIsCreatedQueuedWithEveryStep(t *testing.T) {
	pool, _ := created(t)

	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}

	var e Evaluation
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		e, err = w.Run(ctx, theRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if e.Namespace != "finance" || e.State != agk.Queued || e.Seq != 0 {
		t.Errorf("a new run reads as %+v", e)
	}
	if len(e.Document) != 0 {
		t.Errorf("a run nothing has decided carries a document: %s", e.Document)
	}
	if e.Trigger != agk.TriggerSchedule {
		t.Errorf("the trigger came back as %s", e.Trigger)
	}
	if e.Inputs["cycle"] != "2026-09" {
		t.Errorf("the inputs came back as %v", e.Inputs)
	}

	var steps int
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.tx.QueryRow(ctx,
			`select count(*) from steps where run_id = $1 and state = 'pending'`, string(theRun)).Scan(&steps)
	}); err != nil {
		t.Fatal(err)
	}
	if steps != 3 {
		t.Errorf("a run of three steps has %d step rows, and a step nobody has reached is a pending row rather than a missing one", steps)
	}
}

// The controller reaches a run without being told which namespace it is in, because a
// notification carries "an identifier and nothing else".
func TestTheControllerFindsARunWithoutItsNamespace(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}

	err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		e, err := w.Run(ctx, theRun)
		if err != nil {
			return err
		}
		if e.Namespace != "finance" {
			t.Errorf("the run was found in namespace %q", e.Namespace)
		}
		_, err = w.Run(ctx, "01M2ZZZZZZZZZZZZZZZZZZZZZZ")
		if !errors.Is(err, ErrNoRun) {
			t.Errorf("a run nobody created answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// One pass lands whole or not at all, and a pass written against a sequence the run has left is
// refused: "what makes two writers of one run detectable rather than silent".
func TestADecisionIsRefusedWhenTheRunHasMoved(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	first := Decision{
		Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
		Document: json.RawMessage(`{"version":1,"seq":1}`),
		State:    agk.Running, StartedAt: now, WakeAt: now.Add(time.Minute),
		Steps: []StepRow{{Step: "normalize", Verdict: agk.VerdictRunning, Attempts: 1, StartedAt: now}},
		Tasks: []TaskRow{{
			ID: agk.NewTaskID(theRun, "normalize", 1, agk.Shard{}), Step: "normalize",
			State: agk.TaskPending, Attempt: 1, Deadline: now.Add(time.Hour),
		}},
	}
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, first)
	}); err != nil {
		t.Fatal(err)
	}

	// The same pass again, against the sequence it was read at, which has moved.
	err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, first)
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("a decision written against a sequence the run has left answered %v", err)
	}

	// And nothing of the refused pass landed: the task from the first one is there once.
	var tasks int
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.tx.QueryRow(ctx, `select count(*) from tasks where run_id = $1`, string(theRun)).Scan(&tasks)
	}); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Errorf("after one accepted pass and one refused there are %d tasks", tasks)
	}
}

// A task is written by the identifier it is known by everywhere else, so the same task decided
// twice is one row moving rather than two rows disagreeing.
func TestATaskIsOneRowHoweverOftenItIsWritten(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}

	id := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{Index: 3, Of: 8})
	now := time.Now().UTC().Truncate(time.Millisecond)
	code := 0
	log, err := agk.NewLogURI(id)
	if err != nil {
		t.Fatal(err)
	}

	for seq, task := range []TaskRow{
		{ID: id, Step: "invoice", State: agk.TaskPending, Attempt: 1, Shard: agk.Shard{Index: 3, Of: 8}},
		{ID: id, Step: "invoice", State: agk.TaskDispatched, Attempt: 1, Shard: agk.Shard{Index: 3, Of: 8},
			Runner: "runner-dmz-02", DispatchedAt: now, Deadline: now.Add(time.Hour)},
		{ID: id, Step: "invoice", State: agk.TaskSucceeded, Attempt: 1, Shard: agk.Shard{Index: 3, Of: 8},
			ExitCode: &code, StartedAt: now, FinishedAt: now.Add(time.Minute),
			Log: log, LogLines: 412},
	} {
		d := Decision{
			Namespace: "finance", Run: theRun, Was: seq, Seq: seq + 1,
			Document: json.RawMessage(`{"version":1}`), State: agk.Running, StartedAt: now,
			Tasks: []TaskRow{task},
		}
		if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			return w.SaveDecision(ctx, d)
		}); err != nil {
			t.Fatalf("pass %d: %s", seq+1, err)
		}
	}

	var rows int
	var state, runner, uri string
	var dispatched *time.Time
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if err := ns.tx.QueryRow(ctx, `select count(*) from tasks where run_id = $1`, string(theRun)).Scan(&rows); err != nil {
			return err
		}
		return ns.tx.QueryRow(ctx,
			`select state, runner, log_uri, dispatched_at from tasks where idempotency_key = $1`,
			string(id)).Scan(&state, &runner, &uri, &dispatched)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("one task written three times is %d rows", rows)
	}
	if state != "succeeded" {
		t.Errorf("the task ended in %q", state)
	}
	// The moments a task passed through are kept rather than overwritten by a later pass
	// that does not carry them.
	if runner != "runner-dmz-02" || dispatched == nil {
		t.Errorf("the task forgot who held it and when: runner %q, dispatched %v", runner, dispatched)
	}
	if uri != log.String() {
		t.Errorf("the log is at %q", uri)
	}
}

// "A requeue after loss keeps the idempotency key and takes a new task_id." The dispatch that was
// lost stays as it was, and the requeue is a row of its own under the same key; in between, a
// decision that has not yet heard of the loss does not write over it.
func TestARequeueIsARowOfItsOwnUnderTheSameKey(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	key := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{Index: 3, Of: 8})
	now := time.Now().UTC().Truncate(time.Millisecond)
	seq := 0
	decide := func(wake time.Time, task TaskRow) {
		t.Helper()
		task.ID, task.Step, task.Attempt, task.Shard = key, "invoice", 1, agk.Shard{Index: 3, Of: 8}
		if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			return w.SaveDecision(ctx, Decision{
				Namespace: "finance", Run: theRun, Was: seq, Seq: seq + 1,
				Document: json.RawMessage(`{"version":1}`), State: agk.Running, StartedAt: now,
				WakeAt: wake, Tasks: []TaskRow{task},
			})
		}); err != nil {
			t.Fatalf("pass %d: %s", seq+1, err)
		}
		seq++
	}
	wide := func(fn func(ctx context.Context, w *Wide) error) {
		t.Helper()
		if err := pool.Installation(t.Context(), ControllerSweep, fn); err != nil {
			t.Fatal(err)
		}
	}

	decide(now.Add(time.Hour), TaskRow{State: agk.TaskPending})
	decide(now.Add(time.Hour), TaskRow{State: agk.TaskDispatched, Runner: "runner-1", DispatchedAt: now})
	var first string
	wide(func(ctx context.Context, w *Wide) error {
		var err error
		if first, err = w.TaskRow(ctx, "finance", key); err != nil {
			return err
		}
		_, err = w.Published(ctx, "finance", []agk.TaskID{key}, now)
		return err
	})
	if swept(t, pool, now) {
		t.Fatal("a run waiting on its clock with its one task handed out was swept, so the sweep below proves nothing")
	}

	// Only the runner holding the dispatch can say it lost it, and saying so twice moves
	// nothing the second time.
	wide(func(ctx context.Context, w *Wide) error {
		if _, err := w.Lose(ctx, "finance", key, "runner-2", now); !errors.Is(err, ErrNotHeld) {
			t.Errorf("a runner that never held the task declared it lost, answering %v", err)
		}
		if moved, err := w.Lose(ctx, "finance", key, "runner-1", now); err != nil || !moved {
			t.Errorf("the runner holding the task could not declare it lost: moved %v, %v", moved, err)
		}
		if moved, err := w.Lose(ctx, "finance", key, "runner-1", now); err != nil || moved {
			t.Errorf("a loss declared twice moved %v the second time, answering %v", moved, err)
		}
		return nil
	})
	if !swept(t, pool, now) {
		t.Error("a run whose task was declared lost waits for its clock, and nothing hears of the loss until then")
	}

	// A decision read before the loss still has the dispatch running. The loss stands, and
	// the run is left for the next sweep, which is what hears of it.
	decide(now.Add(time.Hour), TaskRow{State: agk.TaskRunning, StartedAt: now})
	var losses []Loss
	wide(func(ctx context.Context, w *Wide) error {
		var err error
		losses, err = w.Losses(ctx, "finance", theRun)
		return err
	})
	if len(losses) != 1 || losses[0].Task != key || losses[0].Requeue != 0 || losses[0].At.IsZero() {
		t.Fatalf("the losses read %+v, want the first dispatch of %s", losses, key)
	}
	if !swept(t, pool, now) {
		t.Error("a run holding a loss its last decision wrote over is not swept until the clock that decision set")
	}

	// Heard, and requeued: a row of its own, under the same key.
	decide(time.Time{}, TaskRow{State: agk.TaskPending, Requeue: 1})
	var second string
	wide(func(ctx context.Context, w *Wide) error {
		var err error
		if second, err = w.TaskRow(ctx, "finance", key); err != nil {
			return err
		}
		losses, err = w.Losses(ctx, "finance", theRun)
		return err
	})
	if second == first {
		t.Errorf("the requeue is row %s, which is the row that was lost", second)
	}
	if len(losses) != 0 {
		t.Errorf("a loss already requeued is still named: %+v", losses)
	}

	var rows []string
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		r, err := ns.tx.Query(ctx,
			`select id || ' ' || requeue || ' ' || state || ' ' || coalesce(runner, '-') from tasks
			 where idempotency_key = $1 order by requeue`, string(key))
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{first + " 0 lost runner-1", second + " 1 pending -"}
	if len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Errorf("the key holds %q, want %q", rows, want)
	}
}

// The sweep finds what a notification would have found, and the three cases it exists for.
func TestTheSweepFindsWhatANotificationWouldHave(t *testing.T) {
	pool, super := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// A run nothing has decided has no wake time and is actionable at once. The seed holds
	// two runs of its own, so what matters is whether ours is among them.
	if !swept(t, pool, now) {
		t.Fatal("a run nothing has decided was not swept")
	}

	// Decided, with its clock set in the future and every task published: nothing to do.
	id := agk.NewTaskID(theRun, "normalize", 1, agk.Shard{})
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		if err := w.SaveDecision(ctx, Decision{
			Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
			Document: json.RawMessage(`{"version":1}`), State: agk.Running, StartedAt: now,
			WakeAt: now.Add(time.Hour),
			Tasks:  []TaskRow{{ID: id, Step: "normalize", State: agk.TaskPending, Attempt: 1}},
		}); err != nil {
			return err
		}
		n, err := w.Published(ctx, "finance", []agk.TaskID{id}, now)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("stamping one published task stamped %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if swept(t, pool, now) {
		t.Fatal("a run waiting on its clock with everything published was swept")
	}

	// Its clock comes round.
	if !swept(t, pool, now.Add(2*time.Hour)) {
		t.Fatal("a run whose clock came round was not swept")
	}

	// And the outbox: a task whose message never went, with the clock still in the future.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(),
		`update tasks set published_at = null where run_id = $1`, string(theRun)); err != nil {
		t.Fatal(err)
	}
	if !swept(t, pool, now) {
		t.Fatal("a run holding a task whose message never went was not swept")
	}

	var waiting []agk.TaskID
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		waiting, err = w.Unpublished(ctx, "finance", theRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 || waiting[0] != id {
		t.Errorf("the unpublished tasks are %v", waiting)
	}
}

// A terminal run is not swept, however long ago its clock was. It also starts before the row
// that records it was written, which is legitimate: the controller's clock and the database's
// are two clocks and neither orders the other.
func TestAFinishedRunIsNotSwept(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, Decision{
			Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
			Document: json.RawMessage(`{"version":1}`), State: agk.Succeeded,
			StartedAt: now.Add(-time.Hour), FinishedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour),
			Outputs: map[string]any{"invoices": "sha256:aaa"},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if swept(t, pool, now.Add(time.Hour)) {
		t.Fatal("a run that had finished was swept")
	}

	// And the run now carries what the two purges wait for.
	var expires *time.Time
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.tx.QueryRow(ctx, `select expires_at from runs where id = $1`, string(theRun)).Scan(&expires)
	}); err != nil {
		t.Fatal(err)
	}
	if expires == nil {
		t.Error("a finished run has no expiry, and the envelope and log purges both wait on one")
	}
}

// swept says whether this test's own run is among the actionable ones. The seed writes two
// runs of its own, and a test that counted would be testing the seed.
func swept(t *testing.T, pool *Pool, now time.Time) bool {
	t.Helper()
	for _, id := range actionable(t, pool, now) {
		if id == theRun {
			return true
		}
	}
	return false
}

func actionable(t *testing.T, pool *Pool, now time.Time) []agk.RunID {
	t.Helper()
	var out []agk.RunID
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		out, err = w.Actionable(ctx, now, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
