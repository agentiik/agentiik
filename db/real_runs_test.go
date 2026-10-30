package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
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
		Trigger: agk.TriggerSchedule,
		Inputs:  json.RawMessage(`{"cycle": "2026-09"}`),
		Steps:   steps,
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

// A step's reason is written with its verdict and read back in the run's detail: why a step failed
// before any task started, which no task row and no log can say. A step that gave none reads none,
// and is answered without the key.
func TestAStepsReasonIsReadInTheRunsDetail(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	const refused = "graph: step normalize: port orders: inputs feeds the port a number: a port carries a batch of items, so the value is an object or a list of objects"
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, Decision{
			Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
			Document: json.RawMessage(`{"version":1,"seq":1}`),
			State:    agk.Failed, StartedAt: now, FinishedAt: now,
			Steps: []StepRow{
				{Step: "normalize", Verdict: agk.VerdictFailed, FinishedAt: now, Reason: refused},
				{Step: "invoice", Verdict: agk.VerdictSkipped, FinishedAt: now, Reason: "when [succeeded] does not admit normalize, which failed"},
				{Step: "archive", Verdict: agk.VerdictPending},
			},
		})
	}); err != nil {
		t.Fatal(err)
	}

	var d RunDetail
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		d, err = ns.RunDetail(ctx, theRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := map[agk.Step]string{}
	for _, s := range d.Steps {
		got[s.Step] = s.Reason
	}
	want := map[agk.Step]string{"normalize": refused, "invoice": "when [succeeded] does not admit normalize, which failed", "archive": ""}
	if !maps.Equal(got, want) {
		t.Errorf("the detail reads the reasons %q, want %q", got, want)
	}
	for _, s := range d.Steps {
		answered, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if has := bytes.Contains(answered, []byte(`"reason"`)); has != (s.Reason != "") {
			t.Errorf("the step %s is answered as %s", s.Step, answered)
		}
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
		if _, err := w.Lose(ctx, "finance", key, first, "runner-2", now); !errors.Is(err, ErrNotHeld) {
			t.Errorf("a runner that never held the task declared it lost, answering %v", err)
		}
		if _, err := w.Lose(ctx, "finance", key, "not-a-row", "runner-1", now); !errors.Is(err, ErrNotHeld) {
			t.Errorf("a loss naming no dispatch of the key answered %v", err)
		}
		if moved, err := w.Lose(ctx, "finance", key, first, "runner-1", now); err != nil || !moved {
			t.Errorf("the runner holding the task could not declare it lost: moved %v, %v", moved, err)
		}
		if moved, err := w.Lose(ctx, "finance", key, first, "runner-1", now); err != nil || moved {
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

	// The runner that lost the first dispatch takes the requeue, and its loss of the first
	// comes round again. It names the first, which is lost already, and the requeue that
	// runner now holds is left as it is.
	decide(time.Time{}, TaskRow{State: agk.TaskDispatched, Runner: "runner-1", Requeue: 1, DispatchedAt: now})
	wide(func(ctx context.Context, w *Wide) error {
		if moved, err := w.Lose(ctx, "finance", key, first, "runner-1", now); err != nil || moved {
			t.Errorf("a loss of the first dispatch moved %v once the same runner held the requeue, answering %v", moved, err)
		}
		return nil
	})
	var requeue string
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.tx.QueryRow(ctx, `select state::text from tasks where id = $1`, second).Scan(&requeue)
	}); err != nil {
		t.Fatal(err)
	}
	if requeue != "dispatched" {
		t.Errorf("the requeue reads %s after a loss of the dispatch before it", requeue)
	}
}

// A run decided with nothing on the clock waits on its tasks in flight, and is not what a sweep
// comes round for, however often it does: it has been decided, which a run with no clock at all
// had once read as not. What it waits for makes it due again: a loss, reported or declared.
func TestADecidedRunWithNothingOnTheClockIsNotSwept(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	key := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{})
	var row string
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		if err := w.SaveDecision(ctx, Decision{
			Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
			Document: json.RawMessage(`{"version":1}`), State: agk.Running, StartedAt: now,
			Tasks: []TaskRow{{ID: key, Step: "invoice", State: agk.TaskDispatched, Attempt: 1, Runner: "runner-1", DispatchedAt: now}},
		}); err != nil {
			return err
		}
		var err error
		if row, err = w.TaskRow(ctx, "finance", key); err != nil {
			return err
		}
		_, err = w.Published(ctx, "finance", []agk.TaskID{key}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, 10 * time.Second, time.Hour, 24 * time.Hour} {
		if swept(t, pool, now.Add(at)) {
			t.Fatalf("a run decided with nothing on the clock and its one task handed out was swept %s later", at)
		}
	}

	later := now.Add(time.Minute)
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		_, err := w.Lose(ctx, "finance", key, row, "runner-1", later)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !swept(t, pool, later) {
		t.Error("a run whose runner reported its task lost is not swept, and nothing else hears of the loss")
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

// The next clock is the earliest among the runs that have not ended, past the moment it is asked
// from: one already come round is the sweep's, and a finished run's clock waits on nothing.
func TestTheNextClockIsTheEarliestOfTheRunsStillGoing(t *testing.T) {
	pool, _ := created(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, r := range []struct {
		wake  time.Duration
		state agk.RunState
	}{
		{3 * time.Hour, agk.Running},
		{time.Hour, agk.Waiting},
		{-time.Minute, agk.Running},
		{30 * time.Minute, agk.Succeeded},
	} {
		run := aRun()
		run.ID = agk.NewRunID()
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.CreateRun(ctx, run)
		}); err != nil {
			t.Fatal(err)
		}
		d := Decision{
			Namespace: "finance", Run: run.ID, Was: 0, Seq: 1,
			Document: json.RawMessage(`{"version":1}`), State: r.state, StartedAt: now,
			WakeAt: now.Add(r.wake),
		}
		if r.state.Terminal() {
			d.FinishedAt = now
		}
		if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			return w.SaveDecision(ctx, d)
		}); err != nil {
			t.Fatal(err)
		}
	}

	next := func(after time.Time) time.Time {
		t.Helper()
		var at time.Time
		if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			var err error
			at, err = w.NextWake(ctx, after)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if got, want := next(now), now.Add(time.Hour); !got.Equal(want) {
		t.Errorf("the next clock after now is %s, want the waiting run's, an hour on, %s", got, want)
	}
	if got, want := next(now.Add(time.Hour)), now.Add(3*time.Hour); !got.Equal(want) {
		t.Errorf("the next clock after the waiting run's is %s, want the one after it, %s", got, want)
	}
	if got := next(now.Add(3 * time.Hour)); !got.IsZero() {
		t.Errorf("past every clock, the next one is %s", got)
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
			StartedAt: now.Add(-time.Hour), FinishedAt: now, Retain: 7 * 24 * time.Hour,
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

// "Envelopes and logs ... live by the workflow's defaults.retain, resolved to one date when the run
// finishes", capped by the namespace's max_retention_days, which bounds a workflow that declares
// none as well: a run that asks for less keeps what it asked for.
func TestAFinishedRunKeepsItsEnvelopesWithinTheNamespaceCeiling(t *testing.T) {
	pool, super := created(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `update namespaces set max_retention_days = 30 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}

	const day = 24 * time.Hour
	finished := time.Now().UTC().Truncate(time.Millisecond)
	for _, c := range []struct {
		name         string
		retain, want time.Duration
	}{
		{"asking-for-less", 7 * day, 7 * day},
		{"asking-for-more", 90 * day, 30 * day},
		{"asking-for-nothing", 0, 30 * day},
	} {
		r := aRun()
		r.ID = agk.NewRunID()
		var expires time.Time
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.CreateRun(ctx, r)
		})
		if err == nil {
			err = pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
				if err := w.SaveDecision(ctx, Decision{
					Namespace: "finance", Run: r.ID, Was: 0, Seq: 1,
					Document: json.RawMessage(`{"version":1}`), State: agk.Succeeded,
					StartedAt: finished.Add(-time.Hour), FinishedAt: finished, Retain: c.retain,
				}); err != nil {
					return err
				}
				return w.tx.QueryRow(ctx, `select expires_at from runs where id = $1`, string(r.ID)).Scan(&expires)
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := expires.Sub(finished); got != c.want {
			t.Errorf("%s: a run keeping its envelopes for %s under a ceiling of 30 days keeps them %s after it finished", c.name, c.retain, got)
		}
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
