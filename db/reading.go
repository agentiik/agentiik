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

// RunQuery is how a listing is narrowed, besides the workflows it is of, which are what the
// authorizer allowed and are given beside it.
//
// Since and Until bound when a run was created, both included, and the zero time bounds nothing.
// Included at both ends, so that a client paging back through time by passing the last run it was
// given as Until is given that run again rather than skipping the ones created in the same instant.
type RunQuery struct {
	State string
	Limit int

	Since time.Time
	Until time.Time
}

// check refuses a query that names no state there is, and bounds its limit.
func (q *RunQuery) check() error {
	if q.Limit < 1 || q.Limit > 500 {
		q.Limit = 50
	}
	if q.State != "" {
		var state agk.RunState
		if err := state.UnmarshalText([]byte(q.State)); err != nil {
			return fmt.Errorf("db: %q is not a run state: %w", q.State, err)
		}
	}
	return nil
}

// bound is a time as the query binds it, where the zero time is null and bounds nothing.
func bound(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// RunSummary is one row of a listing.
type RunSummary struct {
	// Namespace is where the run is, which a listing across namespaces has to say and a run
	// read by its identifier alone is the only place to learn.
	Namespace string `json:"namespace"`

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

// summaries reads the rows of a listing.
func summaries(rows pgx.Rows) ([]RunSummary, error) {
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

// RunDetail reads one run whole.
func (n *NS) RunDetail(ctx context.Context, run agk.RunID) (RunDetail, error) {
	var d RunDetail
	var inputs, outputs []byte
	var state, trigger string
	var by *string
	var started, finished *time.Time

	err := n.tx.QueryRow(ctx, `
		select namespace, id, workflow, commit, state, trigger, triggered_by,
		       created_at, started_at, finished_at, inputs, outputs, replay_from_start_only
		from runs where namespace = $1 and id = $2`, n.namespace, string(run)).
		Scan(&d.Namespace, &d.Run, &d.Workflow, &d.Commit, &state, &trigger, &by,
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
	if err := rows.Scan(&r.Namespace, &r.Run, &r.Workflow, &r.Commit, &state, &trigger, &by,
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

// Workflow is one workflow, named as the installation names it: by its namespace and its name.
type Workflow struct {
	Namespace string
	Name      string
}

// Workflows are every workflow there is, narrowed to one namespace or one name where either is
// given, in order.
//
// It is the first half of a listing across namespaces: what the authorizer is asked about, one
// workflow at a time, since a permission is held on a whole namespace or on a single workflow and
// both are answered by asking about the workflow.
func (w *Wide) Workflows(ctx context.Context, namespace, name string) ([]Workflow, error) {
	rows, err := w.tx.Query(ctx, `
		select namespace, name from workflows
		where ($1 = '' or namespace = $1)
		  and ($2 = '' or name = $2)
		order by namespace, name`, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("db: the workflows could not be read: %w", err)
	}
	defer rows.Close()
	out := []Workflow{}
	for rows.Next() {
		var wf Workflow
		if err := rows.Scan(&wf.Namespace, &wf.Name); err != nil {
			return nil, err
		}
		out = append(out, wf)
	}
	return out, rows.Err()
}

// Runs lists the runs of the workflows given and of no others, newest first.
//
// "with the failing and waiting runs surfaced first" is the console's ordering and not this one:
// what is ordered here is time, because a listing that reordered itself by state would be a
// listing whose second page overlapped its first. The surfacing is the client's to do over what
// it was given.
//
// The second half of a listing, across namespaces or within one, given the workflows the
// authorizer allowed. None given reads nothing, rather than everything: the filter is the
// authorisation decision, and a missing one fails closed. There is no listing of one namespace's
// runs that skips the question, because "a deny wins at any scope" and a deny on one workflow is
// only in the answer where that workflow is in the question.
func (w *Wide) Runs(ctx context.Context, among []Workflow, q RunQuery) ([]RunSummary, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	if len(among) == 0 {
		return []RunSummary{}, nil
	}
	namespaces := make([]string, len(among))
	names := make([]string, len(among))
	for i, wf := range among {
		namespaces[i], names[i] = wf.Namespace, wf.Name
	}
	rows, err := w.tx.Query(ctx, `
		select namespace, id, workflow, commit, state, trigger, triggered_by,
		       created_at, started_at, finished_at
		from runs
		where (namespace, workflow::text) in (select * from unnest($1::text[], $2::text[]))
		  and ($3 = '' or state = $3)
		  and ($4::timestamptz is null or created_at >= $4)
		  and ($5::timestamptz is null or created_at <= $5)
		order by created_at desc, id desc
		limit $6`, namespaces, names, q.State, bound(q.Since), bound(q.Until), q.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: the runs could not be read: %w", err)
	}
	return summaries(rows)
}

// ErrNoOutput is no output of that name recorded on the run: the workflow declares none, or the
// run has not ended, or it ended without every output it declares, since a run's outputs are
// written when it ends and only when all of them are there.
var ErrNoOutput = errors.New("db: the run records no output of that name")

// Output is one workflow output of a run: the step port it is a view of, and the envelope that
// port published.
type Output struct {
	Step     agk.Step
	Port     agk.Port
	Envelope Envelope
}

// Output reads one of a run's workflow outputs.
//
// runs.outputs names the step and the port each output is a view of, and steps.ports holds the
// digest that port published, which is where the envelope is: the database keeps its digest and
// never its bytes.
func (n *NS) Output(ctx context.Context, run agk.RunID, name string) (Output, error) {
	var raw []byte
	err := n.tx.QueryRow(ctx,
		`select outputs -> $3::text from runs where namespace = $1 and id = $2`,
		n.namespace, string(run), name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Output{}, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return Output{}, fmt.Errorf("db: the outputs of run %s could not be read: %w", run, err)
	}
	if len(raw) == 0 {
		return Output{}, fmt.Errorf("%w: %s of run %s", ErrNoOutput, name, run)
	}
	var of struct {
		Step agk.Step `json:"step"`
		Port agk.Port `json:"port"`
	}
	if err := json.Unmarshal(raw, &of); err != nil || of.Step == "" || of.Port == "" {
		return Output{}, fmt.Errorf("db: the output %s of run %s is recorded as %s, which names no step port", name, run, raw)
	}
	ports, err := n.PublishedPorts(ctx, run, of.Step)
	if err != nil {
		return Output{}, err
	}
	e, published := ports[of.Port]
	if !published {
		return Output{}, fmt.Errorf("db: the output %s of run %s is a view of %s/%s, which published nothing", name, run, of.Step, of.Port)
	}
	return Output{Step: of.Step, Port: of.Port, Envelope: e}, nil
}
