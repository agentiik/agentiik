package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// A run's trace, as the rows hold it once the run has ended.
//
// "One trace per run, one span per task": the controller exports them when a run ends, from what
// is written here rather than from what one process remembers, since the dispatches of a run are
// decided by several passes and by whichever controller held the term for each, and a requeue is
// a row of its own that no evaluator state holds once the key has moved on.

// RunTrace is a run and every dispatch of its tasks.
type RunTrace struct {
	Namespace string
	Run       agk.RunID
	Workflow  string
	Commit    string
	State     agk.RunState
	Trigger   agk.TriggerKind

	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	Dispatches []TracedDispatch
}

// TracedDispatch is one row of the tasks table: one dispatch of one key.
type TracedDispatch struct {
	// ID is the dispatch's task_id, which names its span.
	ID  string
	Key agk.TaskID

	Step    agk.Step
	Attempt int
	Shard   agk.Shard
	Requeue int
	State   agk.TaskState

	Runner   string
	ExitCode *int

	DispatchedAt time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
}

// Trace reads a run and every dispatch of its tasks, in the order a person reads them.
func (w *Wide) Trace(ctx context.Context, namespace string, run agk.RunID) (RunTrace, error) {
	r := RunTrace{Namespace: namespace, Run: run}
	var state, trigger string
	var started, finished *time.Time
	err := w.tx.QueryRow(ctx,
		`select workflow, commit, state, trigger, created_at, started_at, finished_at
		 from runs where namespace = $1 and id = $2`, namespace, string(run)).
		Scan(&r.Workflow, &r.Commit, &state, &trigger, &r.CreatedAt, &started, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunTrace{}, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return RunTrace{}, fmt.Errorf("db: the trace of run %s could not be read: %w", run, err)
	}
	if err := r.State.UnmarshalText([]byte(state)); err != nil {
		return RunTrace{}, fmt.Errorf("db: run %s is in state %q: %w", run, state, err)
	}
	if err := r.Trigger.UnmarshalText([]byte(trigger)); err != nil {
		return RunTrace{}, fmt.Errorf("db: run %s says it was started by %q: %w", run, trigger, err)
	}
	r.StartedAt, r.FinishedAt = instantOf(started), instantOf(finished)

	rows, err := w.tx.Query(ctx,
		`select id, idempotency_key, step, attempt, shard_index, shard_of, requeue, state,
		        runner, exit_code, dispatched_at, started_at, finished_at
		 from tasks where namespace = $1 and run_id = $2
		 order by step, attempt, shard_index nulls first, requeue`, namespace, string(run))
	if err != nil {
		return RunTrace{}, fmt.Errorf("db: the tasks of run %s could not be read: %w", run, err)
	}
	defer rows.Close()
	for rows.Next() {
		var d TracedDispatch
		var state string
		var index, of *int
		var runner *string
		var dispatched, started, finished *time.Time
		if err := rows.Scan(&d.ID, &d.Key, &d.Step, &d.Attempt, &index, &of, &d.Requeue, &state,
			&runner, &d.ExitCode, &dispatched, &started, &finished); err != nil {
			return RunTrace{}, fmt.Errorf("db: the tasks of run %s could not be read: %w", run, err)
		}
		if err := d.State.UnmarshalText([]byte(state)); err != nil {
			return RunTrace{}, fmt.Errorf("db: task %s is in state %q: %w", d.Key, state, err)
		}
		if index != nil && of != nil {
			d.Shard = agk.Shard{Index: *index, Of: *of}
		}
		if runner != nil {
			d.Runner = *runner
		}
		d.DispatchedAt, d.StartedAt, d.FinishedAt = instantOf(dispatched), instantOf(started), instantOf(finished)
		r.Dispatches = append(r.Dispatches, d)
	}
	if err := rows.Err(); err != nil {
		return RunTrace{}, fmt.Errorf("db: the tasks of run %s could not be read: %w", run, err)
	}
	return r, nil
}

// instantOf is a nullable instant, with null read as the zero time.
func instantOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
