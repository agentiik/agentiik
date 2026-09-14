package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// The run, its decision document, and the rows that project it.
//
// A run is created inside a namespace, by whoever authorised it, and decided from outside one,
// by the controller, which serves every namespace and is elected once. So the two halves are
// on the two doors: NS creates and reads, Wide decides.

// ErrStale is a decision written against a run that has moved on.
//
// "Seq counts the decisions taken against this state. It is what a caller persisting the state
// writes beside it to tell a stale copy from a current one, and what makes two writers of one
// run detectable rather than silent." A controller seeing this read the run, decided, and found
// the row changed underneath: the decision it took was taken against a state that no longer
// exists, and the answer is to read again and decide again rather than to force it.
var ErrStale = errors.New("db: the run has been decided since this copy of it was read")

// ErrNoRun is nothing of that identifier in that namespace.
var ErrNoRun = errors.New("db: no run of that identifier")

// NewRun is a run as it is created, before anything has decided anything about it.
type NewRun struct {
	ID       agk.RunID
	Workflow string
	Commit   string

	Trigger     agk.TriggerKind
	TriggeredBy string

	// Inputs are the workflow's declared inputs as they were bound, "already held to their
	// declared schemas with required and default applied", which is package schema's work
	// and happens before a run exists.
	Inputs map[string]any

	// Steps are every step of the graph, so that a step nobody has reached is a pending row
	// rather than a missing one. The evaluator seeds its own state the same way and for the
	// same reason: "the reduction to a run verdict can tell the two apart".
	Steps []agk.Step
}

// CreateRun writes a run and one row per step of its graph.
//
// The run is queued and not running, because "Created, waiting on a concurrency lock or on
// namespace quota" is what queued means and admission has not happened yet. It carries no
// evaluation document for the same reason: a document is what a decision leaves behind, and
// nothing has decided.
func (n *NS) CreateRun(ctx context.Context, r NewRun) error {
	if err := r.ID.Validate(); err != nil {
		return fmt.Errorf("db: the run: %w", err)
	}
	if r.Workflow == "" || r.Commit == "" {
		return fmt.Errorf("db: run %s names workflow %q at commit %q", r.ID, r.Workflow, r.Commit)
	}
	if len(r.Steps) == 0 {
		return fmt.Errorf("db: run %s has no steps, and a graph with nothing in it is refused at validation", r.ID)
	}
	inputs, err := json.Marshal(orEmpty(r.Inputs))
	if err != nil {
		return fmt.Errorf("db: the inputs of run %s could not be written: %w", r.ID, err)
	}

	if _, err := n.tx.Exec(ctx,
		`insert into runs (namespace, id, workflow, commit, state, trigger, triggered_by, inputs)
		 values ($1, $2, $3, $4, 'queued', $5, $6, $7)`,
		n.namespace, string(r.ID), r.Workflow, r.Commit,
		r.Trigger.String(), nilIfEmpty(r.TriggeredBy), inputs); err != nil {
		return fmt.Errorf("db: run %s could not be created: %w", r.ID, err)
	}

	names := make([]string, len(r.Steps))
	for i, s := range r.Steps {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("db: a step of run %s: %w", r.ID, err)
		}
		names[i] = string(s)
	}
	if _, err := n.tx.Exec(ctx,
		`insert into steps (namespace, run_id, step)
		 select $1, $2, name from unnest($3::text[]) as name`,
		n.namespace, string(r.ID), names); err != nil {
		return fmt.Errorf("db: the steps of run %s could not be created: %w", r.ID, err)
	}
	return nil
}

// Evaluation is a run as the controller picks it up.
type Evaluation struct {
	Namespace string
	Run       agk.RunID
	Workflow  string
	Commit    string
	State     agk.RunState

	// Document is the evaluator's state with the envelopes lifted out, and is empty for a
	// run nothing has decided yet.
	Document json.RawMessage

	// Seq is what the next decision has to be written against.
	Seq int

	Inputs  map[string]any
	Trigger agk.TriggerKind
	WakeAt  time.Time
}

// Run reads one run for deciding.
//
// It is on the installation door because the controller is the installation's: it is elected
// once and serves every namespace, and a notification carries "an identifier and nothing else",
// so which namespace a run belongs to is something this answers rather than something the
// caller knew.
func (w *Wide) Run(ctx context.Context, run agk.RunID) (Evaluation, error) {
	var e Evaluation
	// The two enumerations are scanned as the text they are stored as and converted here.
	// pgx would read a text column into an int by refusing it, and a state whose name the
	// database holds is exactly the case UnmarshalText exists for.
	var state, trigger string
	var inputs []byte
	var wake *time.Time
	err := w.tx.QueryRow(ctx,
		`select namespace, id, workflow, commit, state, evaluation, seq, inputs, trigger, wake_at
		 from runs where id = $1`, string(run)).
		Scan(&e.Namespace, &e.Run, &e.Workflow, &e.Commit, &state, &e.Document, &e.Seq,
			&inputs, &trigger, &wake)
	if errors.Is(err, pgx.ErrNoRows) {
		return Evaluation{}, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("db: run %s could not be read: %w", run, err)
	}
	if err := e.State.UnmarshalText([]byte(state)); err != nil {
		return Evaluation{}, fmt.Errorf("db: run %s is in state %q: %w", run, state, err)
	}
	if err := e.Trigger.UnmarshalText([]byte(trigger)); err != nil {
		return Evaluation{}, fmt.Errorf("db: run %s says it was started by %q: %w", run, trigger, err)
	}
	if len(inputs) > 0 {
		if err := json.Unmarshal(inputs, &e.Inputs); err != nil {
			return Evaluation{}, fmt.Errorf("db: the inputs of run %s could not be read: %w", run, err)
		}
	}
	if wake != nil {
		e.WakeAt = *wake
	}
	return e, nil
}

// Decision is one pass of the evaluator, written down.
//
// Everything in it lands in one transaction or none of it does. A document saved without its
// task rows would be a controller that had decided to start work nothing recorded, and rows
// written without their document would be work recorded against a state that never asked for
// it.
type Decision struct {
	Namespace string
	Run       agk.RunID

	// Was is the Seq the document was read at, and Seq is what it has become. The write is
	// refused if the row is no longer at Was.
	Was int
	Seq int

	Document json.RawMessage

	State      agk.RunState
	StartedAt  time.Time
	FinishedAt time.Time

	// WakeAt is Plan.Wake, and the zero time is "nothing waits on the clock".
	WakeAt time.Time

	// ExpiresAt is when this run's envelopes and logs may be purged, set when it finishes
	// from the retention the workflow declared, capped by the namespace.
	ExpiresAt time.Time

	// Outputs are the run's declared outputs as digests, written when it succeeds.
	Outputs map[string]any

	Steps []StepRow
	Tasks []TaskRow
}

// StepRow is a step as the projection holds it.
type StepRow struct {
	Step     agk.Step
	Verdict  agk.Verdict
	Attempts int

	StartedAt  time.Time
	FinishedAt time.Time
}

// TaskRow is one task as the projection holds it.
//
// ID is the ULID the row is keyed by and Key is the identifier on the wire, which the database
// computes for itself from the four columns that make it. A caller writing the key would be a
// second place it could be got wrong.
type TaskRow struct {
	ID    agk.TaskID
	Step  agk.Step
	State agk.TaskState

	Attempt int
	Shard   agk.Shard

	Runner   string
	ExitCode *int

	Log      agk.LogURI
	LogLines int
	LogCut   bool

	DispatchedAt time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	Deadline     time.Time
	PublishedAt  time.Time
}

// SaveDecision writes one pass, or refuses it because the run has moved.
func (w *Wide) SaveDecision(ctx context.Context, d Decision) error {
	if d.Namespace == "" {
		return errors.New("db: a decision with no namespace")
	}
	if err := d.Run.Validate(); err != nil {
		return fmt.Errorf("db: the decision's run: %w", err)
	}
	if d.Seq <= d.Was {
		return fmt.Errorf("db: a decision taking run %s from sequence %d to %d, and a decision is something that happened", d.Run, d.Was, d.Seq)
	}
	outputs, err := json.Marshal(orEmpty(d.Outputs))
	if err != nil {
		return fmt.Errorf("db: the outputs of run %s could not be written: %w", d.Run, err)
	}

	tag, err := w.tx.Exec(ctx,
		`update runs
		 set evaluation = $4, seq = $5, state = $6,
		     started_at = coalesce(started_at, $7), finished_at = $8,
		     wake_at = $9, expires_at = $10, outputs = $11
		 where namespace = $1 and id = $2 and seq = $3`,
		d.Namespace, string(d.Run), d.Was,
		d.Document, d.Seq, d.State.String(),
		nilIfZero(d.StartedAt), nilIfZero(d.FinishedAt),
		nilIfZero(d.WakeAt), nilIfZero(d.ExpiresAt), outputs)
	if err != nil {
		return fmt.Errorf("db: run %s could not be decided: %w", d.Run, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: run %s was read at sequence %d", ErrStale, d.Run, d.Was)
	}

	for _, s := range d.Steps {
		if _, err := w.tx.Exec(ctx,
			`update steps set state = $4, attempts = $5, started_at = coalesce(started_at, $6), finished_at = $7
			 where namespace = $1 and run_id = $2 and step = $3`,
			d.Namespace, string(d.Run), string(s.Step), s.Verdict.String(), s.Attempts,
			nilIfZero(s.StartedAt), nilIfZero(s.FinishedAt)); err != nil {
			return fmt.Errorf("db: step %s of run %s could not be written: %w", s.Step, d.Run, err)
		}
	}

	for _, t := range d.Tasks {
		if err := w.writeTask(ctx, d.Namespace, d.Run, t); err != nil {
			return err
		}
	}
	return nil
}

// writeTask records one task, keyed by the identifier it is known by everywhere else.
//
// The row's primary key is a ULID, and the identifier a runner compares is the generated
// idempotency key. The two are matched here, which is the one place they meet: a task is
// inserted once, by its key, and updated by its key for ever after.
func (w *Wide) writeTask(ctx context.Context, namespace string, run agk.RunID, t TaskRow) error {
	if err := t.ID.Validate(); err != nil {
		return fmt.Errorf("db: a task of run %s: %w", run, err)
	}
	var shardIndex, shardOf *int
	if !t.Shard.IsZero() {
		shardIndex, shardOf = &t.Shard.Index, &t.Shard.Of
	}
	var log *string
	if t.Log != (agk.LogURI{}) {
		s := t.Log.String()
		log = &s
	}

	_, err := w.tx.Exec(ctx,
		`insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of, state,
		                    runner, exit_code, log_uri, log_lines, log_truncated,
		                    dispatched_at, started_at, finished_at, deadline, published_at)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		 on conflict (namespace, idempotency_key) do update
		 set state = excluded.state,
		     runner = coalesce(excluded.runner, tasks.runner),
		     exit_code = excluded.exit_code,
		     log_uri = coalesce(excluded.log_uri, tasks.log_uri),
		     log_lines = coalesce(excluded.log_lines, tasks.log_lines),
		     log_truncated = excluded.log_truncated,
		     dispatched_at = coalesce(tasks.dispatched_at, excluded.dispatched_at),
		     started_at = coalesce(tasks.started_at, excluded.started_at),
		     finished_at = excluded.finished_at,
		     deadline = coalesce(excluded.deadline, tasks.deadline),
		     published_at = coalesce(tasks.published_at, excluded.published_at)`,
		namespace, ulid.New(), string(run), string(t.Step), t.Attempt, shardIndex, shardOf,
		t.State.String(), nilIfEmpty(t.Runner), t.ExitCode, log,
		nilIfZeroInt(t.LogLines), t.LogCut,
		nilIfZero(t.DispatchedAt), nilIfZero(t.StartedAt), nilIfZero(t.FinishedAt),
		nilIfZero(t.Deadline), nilIfZero(t.PublishedAt))
	if err != nil {
		return fmt.Errorf("db: task %s could not be written: %w", t.ID, err)
	}
	return nil
}

// Published stamps the tasks whose messages have gone.
//
// Called after the bus accepted them and never before, which is what makes the stamp mean what
// it says. A task with no stamp is one the sweep publishes again, and publishing twice is free
// because the key is the same.
func (w *Wide) Published(ctx context.Context, namespace string, keys []agk.TaskID, at time.Time) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	strs := make([]string, len(keys))
	for i, k := range keys {
		strs[i] = string(k)
	}
	tag, err := w.tx.Exec(ctx,
		`update tasks set published_at = $3
		 where namespace = $1 and idempotency_key = any($2) and published_at is null`,
		namespace, strs, at)
	if err != nil {
		return 0, fmt.Errorf("db: the published tasks could not be stamped: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Actionable names the runs a sweep should look at.
//
// "A controller that was restarting therefore misses notifications, so it also sweeps for
// actionable work on a fixed interval. The notification is a latency optimisation; the sweep is
// the correctness guarantee." So this has to find everything a notification would have, which
// is three things: a run that has never been decided, a run whose clock has come round, and a
// run holding a task whose message never went.
func (w *Wide) Actionable(ctx context.Context, now time.Time, batch int) ([]agk.RunID, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	rows, err := w.tx.Query(ctx, `
		select id from runs
		where state in ('queued', 'running', 'waiting')
		  and (wake_at is null or wake_at <= $1
		       or exists (select 1 from tasks t
		                  where t.namespace = runs.namespace and t.run_id = runs.id
		                    and t.published_at is null
		                    and t.state in ('pending', 'dispatched')))
		order by coalesce(wake_at, created_at)
		limit $2`, now, batch)
	if err != nil {
		return nil, fmt.Errorf("db: the actionable runs could not be read: %w", err)
	}
	defer rows.Close()
	var out []agk.RunID
	for rows.Next() {
		var id agk.RunID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Unpublished names the tasks of one run whose messages never went.
//
// Pending or dispatched, because a task is dispatched in the evaluator's state the moment it is
// planned: "recording the dispatch is what fixes the task's deadline, because the deadline runs
// from the moment the work became somebody's". A task that has reached running or beyond was
// plainly delivered, whatever the stamp says.
func (w *Wide) Unpublished(ctx context.Context, namespace string, run agk.RunID) ([]agk.TaskID, error) {
	rows, err := w.tx.Query(ctx,
		`select idempotency_key from tasks
		 where namespace = $1 and run_id = $2 and published_at is null
		   and state in ('pending', 'dispatched')`,
		namespace, string(run))
	if err != nil {
		return nil, fmt.Errorf("db: the unpublished tasks of run %s could not be read: %w", run, err)
	}
	defer rows.Close()
	var out []agk.TaskID
	for rows.Next() {
		var k agk.TaskID
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nilIfZeroInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
