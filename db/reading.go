package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// Reading a run, which is what everything with a screen does.
//
// The console lists "runs by namespace, workflow and state, with the failing and waiting runs
// surfaced first" and shows "per-step state, timings, the step that failed, its exit code and the
// tail of its log". None of that is the evaluator's state: it is the projection the decisions
// wrote, which is what the projection is for.

// RunQuery is how a listing is narrowed.
type RunQuery struct {
	Workflow string
	State    string
	Limit    int
}

// RunSummary is one row of a listing.
type RunSummary struct {
	Run      agk.RunID    `json:"run"`
	Workflow string       `json:"workflow"`
	Commit   string       `json:"commit"`
	State    agk.RunState `json:"state"`

	Trigger     agk.TriggerKind `json:"trigger"`
	TriggeredBy string          `json:"triggered_by,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// StepSummary is one step of a run, as a screen shows it.
type StepSummary struct {
	Step     agk.Step    `json:"step"`
	Verdict  agk.Verdict `json:"verdict"`
	Attempts int         `json:"attempts"`

	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`

	// Ports are the envelope digests the step published, which is what the database keeps of
	// them. Never the items.
	Ports map[agk.Port]Envelope `json:"ports,omitempty"`
}

// TaskSummary is one task, which is one shard of one attempt.
type TaskSummary struct {
	Task  agk.TaskID    `json:"task"`
	Step  agk.Step      `json:"step"`
	State agk.TaskState `json:"state"`

	Attempt int        `json:"attempt"`
	Shard   *agk.Shard `json:"shard,omitempty"`

	Runner   string `json:"runner,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`

	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// RunDetail is a run and what became of every part of it.
type RunDetail struct {
	RunSummary

	Inputs  map[string]any `json:"inputs,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`

	// ReplayFromStartOnly is the marker the artifact purge sets: "once any input to a step
	// it could restart from is gone, the run is marked as replayable from the start only,
	// and the console says so where it would otherwise offer the step".
	ReplayFromStartOnly bool `json:"replay_from_start_only"`

	Steps []StepSummary `json:"steps"`
	Tasks []TaskSummary `json:"tasks"`
}

// Runs lists what a namespace holds, newest first.
//
// "with the failing and waiting runs surfaced first" is the console's ordering and not this one:
// what is ordered here is time, because a listing that reordered itself by state would be a
// listing whose second page overlapped its first. The surfacing is the client's to do over what
// it was given.
func (n *NS) Runs(ctx context.Context, q RunQuery) ([]RunSummary, error) {
	if q.Limit < 1 || q.Limit > 500 {
		q.Limit = 50
	}
	if q.State != "" {
		var state agk.RunState
		if err := state.UnmarshalText([]byte(q.State)); err != nil {
			return nil, fmt.Errorf("db: %q is not a run state: %w", q.State, err)
		}
	}

	rows, err := n.tx.Query(ctx, `
		select id, workflow, commit, state, trigger, triggered_by,
		       created_at, started_at, finished_at
		from runs
		where namespace = $1
		  and ($2 = '' or workflow = $2)
		  and ($3 = '' or state = $3)
		order by created_at desc, id desc
		limit $4`, n.namespace, q.Workflow, q.State, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: the runs could not be read: %w", err)
	}
	defer rows.Close()

	out := []RunSummary{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WorkflowOf answers which workflow a run of this namespace is of.
//
// It is what a route about one run is authorised against. A permission such as workflow:run can be
// held on a single workflow, and the path of such a route names the run and not its workflow. The
// identifier is compared as text, because it comes from a path and the column's domain would
// refuse one that is not a ULID with an error rather than find nothing.
func (n *NS) WorkflowOf(ctx context.Context, run agk.RunID) (string, error) {
	var workflow string
	err := n.tx.QueryRow(ctx,
		`select workflow from runs where namespace = $1 and id = $2::text`,
		n.namespace, string(run)).Scan(&workflow)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return "", fmt.Errorf("db: run %s could not be read: %w", run, err)
	}
	return workflow, nil
}

// RunDetail reads one run whole.
func (n *NS) RunDetail(ctx context.Context, run agk.RunID) (RunDetail, error) {
	var d RunDetail
	var inputs, outputs []byte
	var state, trigger string
	var by *string
	var started, finished *time.Time

	err := n.tx.QueryRow(ctx, `
		select id, workflow, commit, state, trigger, triggered_by,
		       created_at, started_at, finished_at, inputs, outputs, replay_from_start_only
		from runs where namespace = $1 and id = $2`, n.namespace, string(run)).
		Scan(&d.Run, &d.Workflow, &d.Commit, &state, &trigger, &by,
			&d.CreatedAt, &started, &finished, &inputs, &outputs, &d.ReplayFromStartOnly)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunDetail{}, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return RunDetail{}, fmt.Errorf("db: run %s could not be read: %w", run, err)
	}
	if err := d.State.UnmarshalText([]byte(state)); err != nil {
		return RunDetail{}, err
	}
	if err := d.Trigger.UnmarshalText([]byte(trigger)); err != nil {
		return RunDetail{}, err
	}
	if by != nil {
		d.TriggeredBy = *by
	}
	if started != nil {
		d.StartedAt = *started
	}
	if finished != nil {
		d.FinishedAt = *finished
	}
	for _, c := range []struct {
		body []byte
		into *map[string]any
	}{{inputs, &d.Inputs}, {outputs, &d.Outputs}} {
		if len(c.body) == 0 {
			continue
		}
		if err := json.Unmarshal(c.body, c.into); err != nil {
			return RunDetail{}, fmt.Errorf("db: run %s could not be read: %w", run, err)
		}
	}

	if d.Steps, err = n.steps(ctx, run); err != nil {
		return RunDetail{}, err
	}
	if d.Tasks, err = n.tasks(ctx, run); err != nil {
		return RunDetail{}, err
	}
	return d, nil
}

func (n *NS) steps(ctx context.Context, run agk.RunID) ([]StepSummary, error) {
	rows, err := n.tx.Query(ctx, `
		select step, state, attempts, started_at, finished_at, ports, envelopes_purged_at
		from steps where namespace = $1 and run_id = $2 order by step`,
		n.namespace, string(run))
	if err != nil {
		return nil, fmt.Errorf("db: the steps of run %s could not be read: %w", run, err)
	}
	defer rows.Close()

	out := []StepSummary{}
	for rows.Next() {
		var s StepSummary
		var verdict string
		var started, finished, purged *time.Time
		var raw []byte
		if err := rows.Scan(&s.Step, &verdict, &s.Attempts, &started, &finished, &raw, &purged); err != nil {
			return nil, err
		}
		if err := s.Verdict.UnmarshalText([]byte(verdict)); err != nil {
			return nil, err
		}
		if started != nil {
			s.StartedAt = *started
		}
		if finished != nil {
			s.FinishedAt = *finished
		}
		if len(raw) > 0 {
			held := map[string]port{}
			if err := json.Unmarshal(raw, &held); err != nil {
				return nil, fmt.Errorf("db: the ports of step %s could not be read: %w", s.Step, err)
			}
			if len(held) > 0 {
				s.Ports = map[agk.Port]Envelope{}
			}
			for name, p := range held {
				e := Envelope{Digest: trimAlgorithm(p.Digest), Size: p.Size, Items: p.Items}
				if purged != nil {
					e.PurgedAt = *purged
				}
				s.Ports[agk.Port(name)] = e
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (n *NS) tasks(ctx context.Context, run agk.RunID) ([]TaskSummary, error) {
	rows, err := n.tx.Query(ctx, `
		select idempotency_key, step, state, attempt, shard_index, shard_of,
		       runner, exit_code, started_at, finished_at
		from tasks where namespace = $1 and run_id = $2
		order by step, attempt, shard_index nulls first, requeue`,
		n.namespace, string(run))
	if err != nil {
		return nil, fmt.Errorf("db: the tasks of run %s could not be read: %w", run, err)
	}
	defer rows.Close()

	out := []TaskSummary{}
	for rows.Next() {
		var t TaskSummary
		var state string
		var index, of *int
		var runner *string
		var started, finished *time.Time
		if err := rows.Scan(&t.Task, &t.Step, &state, &t.Attempt, &index, &of,
			&runner, &t.ExitCode, &started, &finished); err != nil {
			return nil, err
		}
		if err := t.State.UnmarshalText([]byte(state)); err != nil {
			return nil, err
		}
		if index != nil && of != nil {
			t.Shard = &agk.Shard{Index: *index, Of: *of}
		}
		if runner != nil {
			t.Runner = *runner
		}
		if started != nil {
			t.StartedAt = *started
		}
		if finished != nil {
			t.FinishedAt = *finished
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanRun(rows pgx.Rows) (RunSummary, error) {
	var r RunSummary
	var state, trigger string
	var by *string
	var started, finished *time.Time
	if err := rows.Scan(&r.Run, &r.Workflow, &r.Commit, &state, &trigger, &by,
		&r.CreatedAt, &started, &finished); err != nil {
		return RunSummary{}, err
	}
	if err := r.State.UnmarshalText([]byte(state)); err != nil {
		return RunSummary{}, err
	}
	if err := r.Trigger.UnmarshalText([]byte(trigger)); err != nil {
		return RunSummary{}, err
	}
	if by != nil {
		r.TriggeredBy = *by
	}
	if started != nil {
		r.StartedAt = *started
	}
	if finished != nil {
		r.FinishedAt = *finished
	}
	return r, nil
}
