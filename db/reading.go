package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

	// Trigger is served as trigger_kind, the name an expression reads it by.
	Trigger     agk.TriggerKind `json:"trigger_kind"`
	TriggeredBy string          `json:"triggered_by,omitempty"`

	// From is the run and the step whose call started this run, for trigger_kind workflow and
	// no other.
	From *Caller `json:"from,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// ListedRun is a run as a listing answers it: its record, and its steps in the order they started,
// each with its verdict and when it started and ended, which a listing draws as a strip whose
// segments are as wide as the steps took, so that where a run failed and how long each part of it
// lasted are read before its colours are.
type ListedRun struct {
	RunSummary
	Steps []StepStrip `json:"steps"`
}

// StepStrip is one step of a listed run, as its strip draws it.
type StepStrip struct {
	Step       agk.Step    `json:"step"`
	Verdict    agk.Verdict `json:"verdict"`
	StartedAt  time.Time   `json:"started_at,omitzero"`
	FinishedAt time.Time   `json:"finished_at,omitzero"`
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

	// Image, InputPorts and OutputPorts are what the version the run pinned declares of the step:
	// the image it runs, by digest, and the ports it reads and publishes. The API reads them from
	// that version's graph, which the database holds as the document it was built from rather than
	// as a second copy that could disagree with it.
	Image       string     `json:"image,omitempty"`
	InputPorts  []agk.Port `json:"input_ports,omitempty"`
	OutputPorts []agk.Port `json:"output_ports,omitempty"`
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

	// MemoisedFrom is the run whose task published what a cache hit republished for this one:
	// "a hit republishes the same envelopes without starting a container", so no runner and no
	// exit code are named, and this says where the outputs were made.
	MemoisedFrom agk.RunID `json:"memoised_from,omitempty"`

	// Called is the run a call started, where the task is a workflow: step's, which no runner
	// held: its ending is that run's.
	Called agk.RunID `json:"called,omitempty"`

	// Inputs are the envelopes the task was handed on its input ports, by digest as a step's
	// ports are, from the grant it was dispatched with. A task never dispatched has none.
	Inputs map[agk.Port]Envelope `json:"inputs,omitempty"`

	// Params are the parameters it was dispatched with, resolved, from the same grant. An
	// expression may carry envelope contents into them, so the API answers them under
	// run:read_data alone, as it does a run's inputs.
	Params map[string]any `json:"params,omitempty"`
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

	// ReplayOf is the run this one replays, and ReplayFrom the step it replays from, left out
	// for a replay from the start: the steps above it were reused rather than run, and carry a
	// verdict and no task.
	ReplayOf   agk.RunID `json:"replay_of,omitempty"`
	ReplayFrom agk.Step  `json:"replay_from,omitempty"`

	// EnvelopesPurged says the run's envelopes have gone with its retention, which no replay
	// from a step can reuse. Not answered, since the steps' ports say so each.
	EnvelopesPurged bool `json:"-"`

	// Includes are the workflow includes of the version the run pinned, each with the commit its
	// ref resolved to when the version was pushed: "the run records the commit it resolved to".
	// Absent where it includes no other repository.
	Includes []RunInclude `json:"includes,omitempty"`

	// Reason is why the run ended as it did, where nothing in its workflow is what ended it, and
	// empty on every other run: from v0.3.0, a run its principal no longer held workflow:run for
	// when it was created, which "ends cancelled before any task, with a reason naming the grant
	// that lapsed".
	Reason string `json:"reason,omitempty"`

	Steps []StepSummary `json:"steps"`
	Tasks []TaskSummary `json:"tasks"`

	// Artifacts are the files the run's steps published, live or retired: "the run detail keeps
	// showing the artifact's name, size and digest with its collection recorded", so that one
	// fetched past its retention is told from one that never existed.
	Artifacts []ArtifactSummary `json:"artifacts"`
}

// ArtifactSummary is one file a step published on a port, as its reference holds it: what the
// envelope's file names, and where it stands in its retention, which the bytes behind it may
// have outlived or not.
type ArtifactSummary struct {
	URI       agk.URI  `json:"uri"`
	Step      agk.Step `json:"step"`
	Port      agk.Port `json:"port"`
	Name      string   `json:"name"`
	MediaType string   `json:"media_type"`
	Size      int64    `json:"size"`
	// SHA256 is the sixty-four hexadecimal characters the envelope's file carries.
	SHA256 string `json:"sha256"`

	// Status is live while the artifact may be fetched, expired once its duration ran out, and
	// collected once its fetches were spent; RetiredAt is when it stopped being live.
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	RetiredAt time.Time `json:"retired_at,omitzero"`

	// FetchesLeft is what remains of a fetch budget, which only a workflow output may declare,
	// and nil where there is none.
	FetchesLeft *int `json:"fetches_left,omitempty"`
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
	var from Caller

	err := n.tx.QueryRow(ctx, `
		select namespace, id, workflow, commit, state, trigger, triggered_by,
		       created_at, started_at, finished_at, inputs, outputs, replay_from_start_only,
		       coalesce(reason, ''), coalesce(replay_of, ''), coalesce(replay_from, ''),
		       envelopes_purged_at is not null, coalesce(caller_run, ''), coalesce(caller_step, '')
		from runs where namespace = $1 and id = $2`, n.namespace, string(run)).
		Scan(&d.Namespace, &d.Run, &d.Workflow, &d.Commit, &state, &trigger, &by,
			&d.CreatedAt, &started, &finished, &inputs, &outputs, &d.ReplayFromStartOnly,
			&d.Reason, &d.ReplayOf, &d.ReplayFrom, &d.EnvelopesPurged, &from.Run, &from.Step)
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
	if from.Run != "" {
		d.From = &from
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
		if err := asWritten(c.body, c.into); err != nil {
			return RunDetail{}, fmt.Errorf("db: run %s could not be read: %w", run, err)
		}
	}

	if d.Includes, err = n.includes(ctx, d.Workflow, d.Commit); err != nil {
		return RunDetail{}, err
	}
	if d.Steps, err = n.steps(ctx, run); err != nil {
		return RunDetail{}, err
	}
	if d.Tasks, err = n.tasks(ctx, run); err != nil {
		return RunDetail{}, err
	}
	if d.Artifacts, err = n.artifacts(ctx, run); err != nil {
		return RunDetail{}, err
	}
	return d, nil
}

// artifacts reads the references of a run's files, by step, port and name, never empty-handed:
// a run that published none answers an empty list.
func (n *NS) artifacts(ctx context.Context, run agk.RunID) ([]ArtifactSummary, error) {
	rows, err := n.tx.Query(ctx, `
		select step, port, name, media_type, size_bytes, digest, status, expires_at, retired_at, fetches_left
		from artifacts where namespace = $1 and run_id = $2
		order by step, port, name`, n.namespace, string(run))
	if err != nil {
		return nil, fmt.Errorf("db: the artifacts of run %s could not be read: %w", run, err)
	}
	defer rows.Close()
	out := []ArtifactSummary{}
	for rows.Next() {
		var a ArtifactSummary
		var step, port, digest string
		var retired *time.Time
		if err := rows.Scan(&step, &port, &a.Name, &a.MediaType, &a.Size, &digest, &a.Status, &a.ExpiresAt, &retired, &a.FetchesLeft); err != nil {
			return nil, fmt.Errorf("db: an artifact of run %s could not be read: %w", run, err)
		}
		a.Step, a.Port = agk.Step(step), agk.Port(port)
		a.URI = agk.URI{Run: run, Step: a.Step, Port: a.Port, Name: a.Name}
		a.SHA256 = strings.TrimPrefix(digest, "sha256:")
		a.ExpiresAt = a.ExpiresAt.UTC()
		if retired != nil {
			a.RetiredAt = retired.UTC()
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RunInclude is one workflow include of the version a run pinned: the repository and the ref the
// file wrote, and the commit the ref resolved to at the push.
type RunInclude struct {
	Workflow string `json:"workflow"`
	Ref      string `json:"ref"`
	Commit   string `json:"commit"`
}

// includes reads the workflow includes a version kept, in the order of what they name, without
// the files each read: the version holds them by the reference the file wrote,
// <namespace>/<name>@<ref>, and a namespace or a name holds no @.
func (n *NS) includes(ctx context.Context, workflow, commit string) ([]RunInclude, error) {
	rows, err := n.tx.Query(ctx, `
		select l.key, l.value->>'commit'
		from workflow_versions v, jsonb_each(coalesce(v.graph->'libraries', '{}')) l
		where v.namespace = $1 and v.workflow = $2 and v.commit = $3
		order by l.key`, n.namespace, workflow, commit)
	if err != nil {
		return nil, fmt.Errorf("db: the includes of %s@%s could not be read: %w", workflow, commit, err)
	}
	var out []RunInclude
	for rows.Next() {
		var key, resolved string
		if err := rows.Scan(&key, &resolved); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: the includes of %s@%s could not be read: %w", workflow, commit, err)
		}
		name, ref, _ := strings.Cut(key, "@")
		out = append(out, RunInclude{Workflow: name, Ref: ref, Commit: resolved})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: the includes of %s@%s could not be read: %w", workflow, commit, err)
	}
	return out, nil
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
	// The inputs and the parameters of the grant issued last, since every grant of one row names
	// what the one dispatch it was prepared for was handed.
	rows, err := n.tx.Query(ctx, `
		select t.idempotency_key, t.step, t.state, t.attempt, t.shard_index, t.shard_of,
		       t.runner, t.exit_code, t.started_at, t.finished_at, t.memoised_from, coalesce(t.called_run, ''),
		       g.scope->'inputs', g.scope->'params',
		       s.envelopes_purged_at
		from tasks t join steps s on s.namespace = t.namespace and s.run_id = t.run_id and s.step = t.step
		left join lateral (select scope from task_grants
		                   where namespace = t.namespace and task_id = t.id
		                   order by created_at desc limit 1) g on true
		where t.namespace = $1 and t.run_id = $2
		order by t.step, t.attempt, t.shard_index nulls first, t.requeue`,
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
		var runner, memoised *string
		var started, finished, purged *time.Time
		var handed, params []byte
		if err := rows.Scan(&t.Task, &t.Step, &state, &t.Attempt, &index, &of,
			&runner, &t.ExitCode, &started, &finished, &memoised, &t.Called, &handed, &params, &purged); err != nil {
			return nil, err
		}
		if t.Inputs, err = inputsHanded(handed, purged); err != nil {
			return nil, fmt.Errorf("db: the inputs of task %s could not be read: %w", t.Task, err)
		}
		if len(params) > 0 && string(params) != "null" {
			if err := asWritten(params, &t.Params); err != nil {
				return nil, fmt.Errorf("db: the parameters of task %s could not be read: %w", t.Task, err)
			}
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
		if memoised != nil {
			t.MemoisedFrom = agk.RunID(*memoised)
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
	var from Caller
	if err := rows.Scan(&r.Namespace, &r.Run, &r.Workflow, &r.Commit, &state, &trigger, &by,
		&r.CreatedAt, &started, &finished, &from.Run, &from.Step); err != nil {
		return RunSummary{}, err
	}
	if from.Run != "" {
		r.From = &from
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
func (w *Wide) Runs(ctx context.Context, among []Workflow, q RunQuery) ([]ListedRun, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	if len(among) == 0 {
		return []ListedRun{}, nil
	}
	namespaces := make([]string, len(among))
	names := make([]string, len(among))
	for i, wf := range among {
		namespaces[i], names[i] = wf.Namespace, wf.Name
	}
	rows, err := w.tx.Query(ctx, `
		select namespace, id, workflow, commit, state, trigger, triggered_by,
		       created_at, started_at, finished_at, coalesce(caller_run, ''), coalesce(caller_step, '')
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
	listed, err := summaries(rows)
	if err != nil {
		return nil, err
	}
	return w.strips(ctx, listed)
}

// strips reads the steps of the runs a page lists, in one question for the page: each run's steps
// in the order they started, those not started after them by name, since a listing reads no graph
// to order them by.
func (w *Wide) strips(ctx context.Context, listed []RunSummary) ([]ListedRun, error) {
	out := make([]ListedRun, len(listed))
	at := make(map[string]int, len(listed))
	namespaces := make([]string, len(listed))
	runs := make([]string, len(listed))
	for i, r := range listed {
		out[i] = ListedRun{RunSummary: r, Steps: []StepStrip{}}
		at[r.Namespace+"/"+string(r.Run)] = i
		namespaces[i], runs[i] = r.Namespace, string(r.Run)
	}
	if len(listed) == 0 {
		return out, nil
	}
	rows, err := w.tx.Query(ctx, `
		select namespace, run_id::text, step, state, started_at, finished_at
		from steps
		where (namespace, run_id::text) in (select * from unnest($1::text[], $2::text[]))
		order by namespace, run_id, started_at nulls last, step`, namespaces, runs)
	if err != nil {
		return nil, fmt.Errorf("db: the steps of the runs listed could not be read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var namespace, run, verdict string
		var s StepStrip
		var started, finished *time.Time
		if err := rows.Scan(&namespace, &run, &s.Step, &verdict, &started, &finished); err != nil {
			return nil, fmt.Errorf("db: a step of the runs listed could not be read: %w", err)
		}
		if err := s.Verdict.UnmarshalText([]byte(verdict)); err != nil {
			return nil, err
		}
		if started != nil {
			s.StartedAt = started.UTC()
		}
		if finished != nil {
			s.FinishedAt = finished.UTC()
		}
		i, ok := at[namespace+"/"+run]
		if !ok {
			continue
		}
		out[i].Steps = append(out[i].Steps, s)
	}
	return out, rows.Err()
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

// inputsHanded reads the inputs a grant's scope names, as the envelopes a run detail shows.
func inputsHanded(raw []byte, purged *time.Time) (map[agk.Port]Envelope, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var inputs []GrantInput
	if err := json.Unmarshal(raw, &inputs); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make(map[agk.Port]Envelope, len(inputs))
	for _, in := range inputs {
		e := Envelope{Digest: in.Digest, Size: in.Size, Items: in.Items}
		if purged != nil {
			e.PurgedAt = *purged
		}
		out[in.Port] = e
	}
	return out, nil
}

// ErrNoEnvelope is no envelope where one was asked for: a port the step has not published, or
// one no dispatch of the step was handed, or no dispatch of that attempt and shard.
var ErrNoEnvelope = errors.New("db: no envelope there")

// StepOutput reads the envelope a step published on one port.
func (n *NS) StepOutput(ctx context.Context, run agk.RunID, step agk.Step, port agk.Port) (Envelope, error) {
	if err := n.stepExists(ctx, run, step); err != nil {
		return Envelope{}, err
	}
	ports, err := n.PublishedPorts(ctx, run, step)
	if err != nil {
		return Envelope{}, err
	}
	e, published := ports[port]
	if !published {
		return Envelope{}, fmt.Errorf("%w: %s of step %s of run %s is not published", ErrNoEnvelope, port, step, run)
	}
	return e, nil
}

// StepInput reads the envelope one dispatch of a step was handed on one input port: the one of
// the attempt given, or of the last attempt dispatched where attempt is zero, and of the shard
// whose index is given, or of the step's one task where the step was not fanned out and shard
// is zero. Of a key requeued after a loss, the last dispatch, which was handed what the first
// was.
func (n *NS) StepInput(ctx context.Context, run agk.RunID, step agk.Step, attempt, shard int, port agk.Port) (Envelope, error) {
	if err := n.stepExists(ctx, run, step); err != nil {
		return Envelope{}, err
	}
	var raw []byte
	var purged *time.Time
	err := n.tx.QueryRow(ctx, `
		select g.scope->'inputs', s.envelopes_purged_at
		from tasks t
		join task_grants g on g.namespace = t.namespace and g.task_id = t.id
		join steps s on s.namespace = t.namespace and s.run_id = t.run_id and s.step = t.step
		where t.namespace = $1 and t.run_id = $2 and t.step = $3
		  and ($4 = 0 or t.attempt = $4)
		  and ($5 = 0 and t.shard_index is null or t.shard_index = $5)
		order by t.attempt desc, t.requeue desc, g.created_at desc
		limit 1`, n.namespace, string(run), string(step), attempt, shard).Scan(&raw, &purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return Envelope{}, fmt.Errorf("%w: no dispatch of step %s of run %s at attempt %d and shard %d", ErrNoEnvelope, step, run, attempt, shard)
	}
	if err != nil {
		return Envelope{}, fmt.Errorf("db: the inputs of step %s of run %s could not be read: %w", step, run, err)
	}
	inputs, err := inputsHanded(raw, purged)
	if err != nil {
		return Envelope{}, fmt.Errorf("db: the inputs of step %s of run %s could not be read: %w", step, run, err)
	}
	e, handed := inputs[port]
	if !handed {
		return Envelope{}, fmt.Errorf("%w: step %s of run %s was handed nothing on %s", ErrNoEnvelope, step, run, port)
	}
	return e, nil
}

// stepExists tells a run that is not there from a step it does not have.
func (n *NS) stepExists(ctx context.Context, run agk.RunID, step agk.Step) error {
	var runs, steps bool
	err := n.tx.QueryRow(ctx, `
		select exists (select 1 from runs where namespace = $1 and id = $2),
		       exists (select 1 from steps where namespace = $1 and run_id = $2 and step = $3)`,
		n.namespace, string(run), string(step)).Scan(&runs, &steps)
	switch {
	case err != nil:
		return fmt.Errorf("db: run %s could not be read: %w", run, err)
	case !runs:
		return fmt.Errorf("%w: %s", ErrNoRun, run)
	case !steps:
		return fmt.Errorf("%w: %s", ErrNoStep, step)
	}
	return nil
}
