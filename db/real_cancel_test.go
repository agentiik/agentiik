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

// A cancellation, against a real PostgreSQL: asked for by the API on the run's row, found there by
// the controller, and carried out by the controller over the run's tasks.

// decidedAs writes one pass over theRun, taking it from sequence 0 to 1 in state.
func decidedAs(t *testing.T, pool *Pool, state agk.RunState, now time.Time, tasks ...TaskRow) {
	t.Helper()
	d := Decision{
		Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
		Document: json.RawMessage(`{"version":1}`), State: state, StartedAt: now,
		WakeAt: now.Add(time.Hour), Tasks: tasks,
	}
	if state.Terminal() {
		d.FinishedAt, d.WakeAt, d.ExpiresAt = now, time.Time{}, now.Add(7*24*time.Hour)
	}
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.SaveDecision(ctx, d)
	}); err != nil {
		t.Fatal(err)
	}
}

func askToCancel(t *testing.T, pool *Pool, namespace string, run agk.RunID, at time.Time) (agk.RunState, error) {
	t.Helper()
	var state agk.RunState
	err := pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		var err error
		state, _, err = ns.RequestCancel(ctx, run, at)
		return err
	})
	return state, err
}

func evaluated(t *testing.T, pool *Pool) Evaluation {
	t.Helper()
	var e Evaluation
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		e, err = w.Run(ctx, theRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// Asking writes down when, once, and decides nothing: the run is in the state it was in until the
// controller reads the request, and the sweep finds it however far off its clock is.
func TestACancellationIsAskedForOnceAndDecidedByNobody(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	decidedAs(t, pool, agk.Running, now)
	if swept(t, pool, now) {
		t.Fatal("a run waiting on its clock with nothing to publish was swept, so the sweep below proves nothing")
	}

	for i, at := range []time.Time{now, now.Add(time.Minute)} {
		state, err := askToCancel(t, pool, "finance", theRun, at)
		if err != nil || state != agk.Running {
			t.Fatalf("asking a %s time answered %s, %v", []string{"first", "second"}[i], state, err)
		}
	}
	e := evaluated(t, pool)
	if !e.CancelRequestedAt.Equal(now) {
		t.Errorf("the request reads as asked at %s, and it was first asked at %s", e.CancelRequestedAt, now)
	}
	if e.State != agk.Running || e.Seq != 1 {
		t.Errorf("asking moved the run to %s at sequence %d, and asking decides nothing", e.State, e.Seq)
	}
	if !swept(t, pool, now) {
		t.Error("a run somebody asked to cancel waits for its clock before the sweep finds it")
	}

	// Another namespace's run, and a name that is not a run at all, are nothing to cancel.
	if _, err := askToCancel(t, pool, "team-ops", theRun, now); !errors.Is(err, ErrNoRun) {
		t.Errorf("a run asked for under another namespace answered %v", err)
	}
	if _, err := askToCancel(t, pool, "finance", "not-a-run", now); !errors.Is(err, ErrNoRun) {
		t.Errorf("an identifier that is not one answered %v", err)
	}
}

// A run that has ended is answered in the state it ended in, and nothing is written on it.
func TestARunThatHasEndedIsLeftAsItEnded(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	decidedAs(t, pool, agk.Succeeded, now)

	state, err := askToCancel(t, pool, "finance", theRun, now)
	if err != nil || state != agk.Succeeded {
		t.Fatalf("asking to cancel a run that succeeded answered %s, %v", state, err)
	}
	if e := evaluated(t, pool); !e.CancelRequestedAt.IsZero() {
		t.Errorf("a run that had ended was asked to cancel at %s", e.CancelRequestedAt)
	}
}

// A run that has finished has started, unless it was cancelled while it was queued: that one was
// never let in, and ends with no started_at.
func TestOnlyACancelledRunEndsWithoutStarting(t *testing.T) {
	now := time.Now().UTC()
	for _, state := range []agk.RunState{agk.Cancelled, agk.Succeeded, agk.Failed, agk.TimedOut} {
		pool, _ := created(t)
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			return ns.CreateRun(ctx, aRun())
		}); err != nil {
			t.Fatal(err)
		}
		err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			return w.SaveDecision(ctx, Decision{
				Namespace: "finance", Run: theRun, Was: 0, Seq: 1,
				Document: json.RawMessage(`{"version":1}`), State: state, FinishedAt: now,
			})
		})
		if (err == nil) != (state == agk.Cancelled) {
			t.Errorf("a run ending %s without having started answered %v", state, err)
		}
	}
}

// A run is found by its identifier alone, as a route naming nothing else finds it, and an
// identifier no run was minted with is no run rather than an error PostgreSQL raises about it.
func TestARunIsFoundByItsIdentifierAlone(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	locate := func(run agk.RunID) (string, string, error) {
		var namespace, workflow string
		err := pool.Installation(t.Context(), RunRoute, func(ctx context.Context, w *Wide) error {
			var err error
			namespace, workflow, err = w.Locate(ctx, run)
			return err
		})
		return namespace, workflow, err
	}
	if namespace, workflow, err := locate(theRun); err != nil || namespace != "finance" || workflow != "monthly-invoicing" {
		t.Errorf("the run is of %q in %q, %v", workflow, namespace, err)
	}
	for _, run := range []agk.RunID{"01M2ZZZZZZZZZZZZZZZZZZZZZZ", "not-a-run", "01M2\x00", "01M2\xff", "01m2z8v1p9c4xq7k2n4d6f8h0c", ""} {
		if namespace, workflow, err := locate(run); !errors.Is(err, ErrNoRun) {
			t.Errorf("%q is of %q in %q, %v", run, workflow, namespace, err)
		}
	}

	// Asking to cancel one is refused the same way, before PostgreSQL is asked.
	for _, run := range []agk.RunID{"01M2\x00", "01M2\xff"} {
		if _, err := askToCancel(t, pool, "finance", run, time.Now()); !errors.Is(err, ErrNoRun) {
			t.Errorf("asking to cancel %q answered %v", run, err)
		}
	}
}

// Cancelling a run's tasks ends every one that is not over, and leaves a loss as the loss it was:
// the one record that a runner went quiet. What it answers is the ones a runner had redeemed, which
// are the ones to stop.
func TestCancellingTheTasksOfARunLeavesWhatEndedAsItEnded(t *testing.T) {
	pool, _ := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	code := 0
	normalize := agk.NewTaskID(theRun, "normalize", 1, agk.Shard{})
	invoice := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{})
	first := agk.NewTaskID(theRun, "archive", 1, agk.Shard{Index: 1, Of: 2})
	second := agk.NewTaskID(theRun, "archive", 1, agk.Shard{Index: 2, Of: 2})
	decidedAs(t, pool, agk.Running, now,
		TaskRow{ID: normalize, Step: "normalize", Attempt: 1, State: agk.TaskSucceeded, ExitCode: &code, StartedAt: now, FinishedAt: now},
		TaskRow{ID: invoice, Step: "invoice", Attempt: 1, State: agk.TaskLost, Runner: "runner-1", FinishedAt: now},
		TaskRow{ID: invoice, Step: "invoice", Attempt: 1, Requeue: 1, State: agk.TaskRunning, Runner: "runner-2", StartedAt: now},
		TaskRow{ID: first, Step: "archive", Attempt: 1, Shard: agk.Shard{Index: 1, Of: 2}, State: agk.TaskDispatched, DispatchedAt: now},
		TaskRow{ID: second, Step: "archive", Attempt: 1, Shard: agk.Shard{Index: 2, Of: 2}, State: agk.TaskPending},
	)

	later := now.Add(time.Minute)
	var held []agk.TaskID
	if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		held, err = w.CancelTasks(ctx, "finance", theRun, later)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0] != invoice {
		t.Errorf("cancelling the run's tasks answered %v as held by a runner, and only the requeue of %s was", held, invoice)
	}

	var d RunDetail
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		var err error
		d, err = ns.RunDetail(ctx, theRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	counts := map[agk.TaskState]int{}
	for _, task := range d.Tasks {
		counts[task.State]++
		if task.State == agk.TaskCancelled && !task.FinishedAt.Equal(later) {
			t.Errorf("%s was cancelled and says it finished at %s", task.Task, task.FinishedAt)
		}
	}
	want := map[agk.TaskState]int{agk.TaskSucceeded: 1, agk.TaskLost: 1, agk.TaskCancelled: 3}
	if len(counts) != len(want) {
		t.Errorf("the run's tasks read %v", counts)
	}
	for state, n := range want {
		if counts[state] != n {
			t.Errorf("%d tasks read %s, want %d: %v", counts[state], state, n, counts)
		}
	}
}

// A run's ending ends every task of it still in flight, whatever the verdict: timed_out for a run
// past its deadline, and cancelled for every other, a run that succeeded or failed with a step a
// merge: first superseded still running included. What it answers is the ones a runner had
// redeemed, to be stopped, and the code a stopped container exits with still lands on its row. A
// run that has not ended ends nothing.
func TestARunsEndingEndsItsTasksStillInFlightWhateverItsVerdict(t *testing.T) {
	normalize := agk.NewTaskID(theRun, "normalize", 1, agk.Shard{})
	invoice := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{})
	archive := agk.NewTaskID(theRun, "archive", 1, agk.Shard{})
	for _, c := range []struct {
		run  agk.RunState
		want agk.TaskState
	}{
		{agk.Succeeded, agk.TaskCancelled},
		{agk.Failed, agk.TaskCancelled},
		{agk.Cancelled, agk.TaskCancelled},
		{agk.TimedOut, agk.TaskTimedOut},
		{agk.Running, agk.TaskRunning},
	} {
		t.Run(c.run.String(), func(t *testing.T) {
			pool, _ := created(t)
			if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
				return ns.CreateRun(ctx, aRun())
			}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			code := 0
			decidedAs(t, pool, c.run, now,
				TaskRow{ID: normalize, Step: "normalize", Attempt: 1, State: agk.TaskSucceeded, ExitCode: &code, StartedAt: now, FinishedAt: now},
				TaskRow{ID: invoice, Step: "invoice", Attempt: 1, State: agk.TaskRunning, Runner: "runner-1", DispatchedAt: now, StartedAt: now},
				TaskRow{ID: archive, Step: "archive", Attempt: 1, State: agk.TaskPending},
			)

			later := now.Add(time.Minute)
			var held []agk.TaskID
			var stopped bool
			err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
				var err error
				if held, err = w.EndTasks(ctx, "finance", theRun, later); err != nil {
					return err
				}
				row, err := w.TaskRow(ctx, "finance", invoice)
				if err != nil {
					return err
				}
				exited := 143
				stopped, err = w.StopReport(ctx, "finance", invoice, row, "runner-1", Stopped{ExitCode: &exited, StartedAt: now})
				return err
			})
			if !c.run.Terminal() {
				if err == nil {
					t.Error("the tasks of a run still running were ended under it")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(held) != 1 || held[0] != invoice {
					t.Errorf("ending the tasks of a %s run answered %v as held by a runner, and only %s was", c.run, held, invoice)
				}
				if !stopped {
					t.Errorf("the code of the container a %s run stopped found no row to land on", c.run)
				}
			}

			var d RunDetail
			if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
				var err error
				d, err = ns.RunDetail(ctx, theRun)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(d.Tasks) != 3 {
				t.Fatalf("the run has %d tasks", len(d.Tasks))
			}
			want := map[agk.TaskID]agk.TaskState{normalize: agk.TaskSucceeded, invoice: c.want, archive: c.want}
			if !c.run.Terminal() {
				want[archive] = agk.TaskPending
			}
			for _, task := range d.Tasks {
				if task.State != want[task.Task] {
					t.Errorf("in a %s run %s reads %s, want %s", c.run, task.Task, task.State, want[task.Task])
				}
				if task.Task == invoice && c.run.Terminal() && (task.ExitCode == nil || *task.ExitCode != 143 || !task.FinishedAt.Equal(later)) {
					t.Errorf("in a %s run the stopped task reads exit %v, finished at %s", c.run, task.ExitCode, task.FinishedAt)
				}
			}
		})
	}
}

// A task the controller stops as superseded or sibling_failed while its run goes on is written
// cancelled by the decision that sends the stop. Where the sweep declared its dispatch lost after
// that decision was read, the loss stands: the runner said nothing, which is what a loss is, so
// the heartbeat's cancel never names it. A runner's own report of how its stopped container exited
// is another matter: the dispatch was not lost after all.
func TestAStopWrittenOverALossKeepsTheLoss(t *testing.T) {
	invoice := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{})
	stopped := 143
	for _, c := range []struct {
		name string
		row  TaskRow
		want agk.TaskState
	}{
		{"stopped by the controller", TaskRow{State: agk.TaskCancelled}, agk.TaskLost},
		{"reported by its runner", TaskRow{State: agk.TaskCancelled, ExitCode: &stopped}, agk.TaskCancelled},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, super := created(t)
			if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
				return ns.CreateRun(ctx, aRun())
			}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			decidedAs(t, pool, agk.Running, now,
				TaskRow{ID: invoice, Step: "invoice", Attempt: 1, State: agk.TaskRunning, Runner: "runner-1", DispatchedAt: now})
			conn, err := pgx.Connect(t.Context(), super)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(context.Background())
			if _, err := conn.Exec(t.Context(),
				`update tasks set state = 'lost', finished_at = $2 where idempotency_key = $1`, string(invoice), now); err != nil {
				t.Fatal(err)
			}

			row := c.row
			row.ID, row.Step, row.Attempt, row.FinishedAt = invoice, "invoice", 1, now.Add(time.Second)
			if row.ExitCode != nil {
				row.StartedAt = now
			}
			if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
				return w.SaveDecision(ctx, Decision{
					Namespace: "finance", Run: theRun, Was: 1, Seq: 2,
					Document: json.RawMessage(`{"version":1}`), State: agk.Running, StartedAt: now,
					WakeAt: now.Add(time.Hour), Tasks: []TaskRow{row},
				})
			}); err != nil {
				t.Fatal(err)
			}

			var state string
			var wake *time.Time
			if err := conn.QueryRow(t.Context(),
				`select t.state, r.wake_at from tasks t join runs r on r.id = t.run_id where t.idempotency_key = $1`,
				string(invoice)).Scan(&state, &wake); err != nil {
				t.Fatal(err)
			}
			if state != c.want.String() {
				t.Errorf("a lost dispatch written cancelled, %s, reads %s, want %s", c.name, state, c.want)
			}
			// And the run keeps the clock the decision gave it. The evaluator ended the task
			// as its stop went out and takes nothing from its loss, so a pass woken for it
			// decides nothing, and each such pass once wrote the row again and woke the run
			// again.
			if wake == nil || !wake.Equal(now.Add(time.Hour)) {
				t.Errorf("the run waits until %v, want the %s the decision set: a loss of a task already stopped is nothing for a pass to hear", wake, now.Add(time.Hour))
			}
		})
	}
}

// A stopped container's report lands once, from the runner the dispatch is bound to, and brings
// its log and usage with it. A report with no exit code still brings them, since the controller
// hears nothing else of a task that was over when it came, and leaves the code for one that has.
func TestAStopReportLandsItsLogAndUsageOnceFromItsRunner(t *testing.T) {
	invoice := agk.NewTaskID(theRun, "invoice", 1, agk.Shard{})
	pool, super := created(t)
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, aRun())
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	decidedAs(t, pool, agk.Running, now,
		TaskRow{ID: invoice, Step: "invoice", Attempt: 1, State: agk.TaskCancelled, Runner: "runner-1", DispatchedAt: now, FinishedAt: now})
	log, err := agk.NewLogURI(invoice)
	if err != nil {
		t.Fatal(err)
	}

	report := func(runner string, r Stopped) bool {
		t.Helper()
		var took bool
		if err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
			row, err := w.TaskRow(ctx, "finance", invoice)
			if err != nil {
				return err
			}
			took, err = w.StopReport(ctx, "finance", invoice, row, runner, r)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return took
	}
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	type reported struct {
		code  *int
		lines int
		cut   bool
		usage map[string]any
	}
	read := func() reported {
		t.Helper()
		var r reported
		if err := conn.QueryRow(t.Context(),
			`select exit_code, coalesce(log_lines, 0), log_truncated, usage from tasks where idempotency_key = $1`,
			string(invoice)).Scan(&r.code, &r.lines, &r.cut, &r.usage); err != nil {
			t.Fatal(err)
		}
		return r
	}

	if report("runner-2", Stopped{Usage: map[string]any{"cpu_seconds": 9.0}}) {
		t.Error("a report from a runner the dispatch is not bound to landed")
	}
	if !report("runner-1", Stopped{Log: log, LogLines: 42, LogCut: true, Usage: map[string]any{"cpu_seconds": 1.5}}) {
		t.Error("a report with no exit code found no row to land on")
	}
	if got := read(); got.code != nil || got.lines != 42 || !got.cut || got.usage["cpu_seconds"] != 1.5 {
		t.Errorf("after a report with no code the stopped task reads exit %v, %d lines, cut %t, usage %v", got.code, got.lines, got.cut, got.usage)
	}

	exited, again := 143, 137
	if !report("runner-1", Stopped{ExitCode: &exited, StartedAt: now, Usage: map[string]any{"cpu_seconds": 2.0}}) {
		t.Error("the exit code found no row to land on")
	}
	if report("runner-1", Stopped{ExitCode: &again, StartedAt: now}) {
		t.Error("a second exit code landed on a row that had one")
	}
	if got := read(); got.code == nil || *got.code != 143 || got.lines != 42 || !got.cut || got.usage["cpu_seconds"] != 1.5 {
		t.Errorf("the stopped task reads exit %v, %d lines, cut %t, usage %v", got.code, got.lines, got.cut, got.usage)
	}
}
