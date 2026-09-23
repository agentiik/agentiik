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

	// Envelopes are every envelope this decision's document references, published or per
	// shard. They are counted rather than merely written down, so that the collector can
	// see the objects the controller put in the store.
	Envelopes []EnvelopeRef

	// Artifacts are the files those envelopes reference, each with the retention the
	// workflow declared for the port that published it. Writing one twice is what a pass
	// that decided something else does, and is free: a reference that says the same thing
	// is the same reference.
	Artifacts []Reference
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
// ID is the idempotency key, and it is never written: the database computes the same string for
// itself from the four columns that make it, and a caller writing the key would be a second place
// it could be got wrong. The ULID the row is keyed by is minted when the row is first written.
//
// Requeue is which dispatch of the key the row records, as graph.ShardState counts them. It is
// what makes a requeue after loss a row of its own under the same key rather than the lost row
// written over.
type TaskRow struct {
	ID    agk.TaskID
	Step  agk.Step
	State agk.TaskState

	Attempt int
	Shard   agk.Shard
	Requeue int

	Runner   string
	ExitCode *int

	Log      agk.LogURI
	LogLines int
	LogCut   bool

	// Usage is what the attempt cost, as the runner measured it. Nothing here reads it:
	// it is carried so that a person, a console and a bill can.
	Usage map[string]any

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

	// Read before the row is overwritten, because the diff the counts move by is against
	// what this run referenced a moment ago and the update below is what replaces it. The
	// state it was in comes from the same look, since a notification is about a run that has
	// just become terminal and a row already updated cannot say whether it just did.
	was, err := w.envelopesOf(ctx, d.Namespace, d.Run)
	if err != nil {
		return err
	}
	before, startedBy, err := w.stateOf(ctx, d.Namespace, d.Run)
	if err != nil {
		return err
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

	unheard := false
	for _, t := range d.Tasks {
		heard, err := w.writeTask(ctx, d.Namespace, d.Run, t)
		if err != nil {
			return err
		}
		unheard = unheard || !heard
	}
	if unheard {
		// A dispatch the heartbeat declared lost after this decision was read, and which the
		// decision still thought in flight. The loss stands, and the run is left with nothing
		// on the clock, which is what the sweep comes round for, so that the next pass hears
		// of it rather than the first one after whatever this decision was waiting on.
		if _, err := w.tx.Exec(ctx,
			`update runs set wake_at = null where namespace = $1 and id = $2`,
			d.Namespace, string(d.Run)); err != nil {
			return fmt.Errorf("db: run %s could not be woken for the loss it has not heard of: %w", d.Run, err)
		}
	}

	if err := w.setEnvelopes(ctx, d.Namespace, d.Run, was, d.Envelopes); err != nil {
		return err
	}
	for _, a := range d.Artifacts {
		if _, err := writeArtifact(ctx, w.tx, d.Namespace, a); err != nil {
			return fmt.Errorf("db: the artifact %s of run %s: %w", a.URI, d.Run, err)
		}
	}
	return w.emit(ctx, d.Namespace, d.Run, before, d.State, startedBy)
}

// stateOf is what a run was before this decision, and who started it.
func (w *Wide) stateOf(ctx context.Context, namespace string, run agk.RunID) (agk.RunState, string, error) {
	var state string
	var by *string
	if err := w.tx.QueryRow(ctx,
		`select state, triggered_by from runs where namespace = $1 and id = $2`,
		namespace, string(run)).Scan(&state, &by); err != nil {
		return 0, "", fmt.Errorf("db: run %s could not be read: %w", run, err)
	}
	var s agk.RunState
	if err := s.UnmarshalText([]byte(state)); err != nil {
		return 0, "", fmt.Errorf("db: run %s is in state %q: %w", run, state, err)
	}
	if by == nil {
		return s, "", nil
	}
	return s, *by, nil
}

// writeTask records one dispatch of one task, keyed by the identifier it is known by everywhere
// else and by which dispatch of it this is.
//
// The row's primary key is a ULID, and the identifier a runner compares is the generated
// idempotency key. The two are matched here, which is the one place they meet: a dispatch is
// inserted once, by its key and its requeue, and updated by them for ever after. A requeue after
// loss is a requeue the row has not seen, so it is inserted, and that is the whole of how it takes
// a new task_id while it keeps its key.
//
// It answers whether the row says what the decision said. It does not where the heartbeat moved
// the dispatch to lost after the decision was read: a loss is declared here and heard by the
// evaluator on its next pass, so a decision that still has the dispatch in flight is behind rather
// than right, and writing it over the loss would erase the one record that the runner went quiet.
// An ending is different. It came back from the runner, so the dispatch was not lost after all,
// and it is written.
func (w *Wide) writeTask(ctx context.Context, namespace string, run agk.RunID, t TaskRow) (bool, error) {
	if err := t.ID.Validate(); err != nil {
		return false, fmt.Errorf("db: a task of run %s: %w", run, err)
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

	usage, err := json.Marshal(orEmpty(t.Usage))
	if err != nil {
		return false, fmt.Errorf("db: the usage of task %s could not be written: %w", t.ID, err)
	}

	// The finish is kept where the decision has none, because a dispatch finishes once: a
	// further attempt and a requeue are rows of their own, so nothing written to this row
	// later can mean it has not finished after all.
	var held string
	err = w.tx.QueryRow(ctx,
		`insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of, requeue, state,
		                    runner, exit_code, log_uri, log_lines, log_truncated,
		                    dispatched_at, started_at, finished_at, deadline, published_at, usage)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		 on conflict (namespace, idempotency_key, requeue) do update
		 set state = case when tasks.state = 'lost'
		                   and excluded.state in ('pending', 'dispatched', 'running', 'publishing')
		                  then tasks.state else excluded.state end,
		     runner = coalesce(excluded.runner, tasks.runner),
		     exit_code = excluded.exit_code,
		     log_uri = coalesce(excluded.log_uri, tasks.log_uri),
		     log_lines = coalesce(excluded.log_lines, tasks.log_lines),
		     log_truncated = excluded.log_truncated,
		     dispatched_at = coalesce(tasks.dispatched_at, excluded.dispatched_at),
		     started_at = coalesce(tasks.started_at, excluded.started_at),
		     finished_at = coalesce(excluded.finished_at, tasks.finished_at),
		     deadline = coalesce(excluded.deadline, tasks.deadline),
		     published_at = coalesce(tasks.published_at, excluded.published_at),
		     usage = case when excluded.usage = '{}'::jsonb then tasks.usage else excluded.usage end
		 returning state`,
		namespace, ulid.New(), string(run), string(t.Step), t.Attempt, shardIndex, shardOf, t.Requeue,
		t.State.String(), nilIfEmpty(t.Runner), t.ExitCode, log,
		nilIfZeroInt(t.LogLines), t.LogCut,
		nilIfZero(t.DispatchedAt), nilIfZero(t.StartedAt), nilIfZero(t.FinishedAt),
		nilIfZero(t.Deadline), nilIfZero(t.PublishedAt), usage).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("db: task %s could not be written: %w", t.ID, err)
	}
	return held == t.State.String(), nil
}

// TaskRow is the identifier a task's own row is keyed by: the row of the latest dispatch of the
// key, which is the one a decision has just planned.
//
// Not the idempotency key. The key says which unit of work this is and is derived from what makes
// it that; the row says which record, and it is what a grant names inside its own text and what a
// log is addressed by. The two are separate on purpose: one is a statement about the work and the
// other is a handle on a row, and a key requeued after a loss has a row for every dispatch.
func (w *Wide) TaskRow(ctx context.Context, namespace string, key agk.TaskID) (string, error) {
	var id string
	err := w.tx.QueryRow(ctx,
		`select id from tasks where namespace = $1 and idempotency_key = $2
		 order by requeue desc limit 1`,
		namespace, string(key)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("db: task %s has no row, and a task is recorded before it is handed out", key)
	}
	if err != nil {
		return "", fmt.Errorf("db: task %s could not be read: %w", key, err)
	}
	return id, nil
}

// ErrNoDispatch is a task_id that names no dispatch of the key it came with.
var ErrNoDispatch = errors.New("db: that task_id is no dispatch of that task")

// RequeueOf answers which dispatch of its key a row records, as graph.ShardState counts them.
//
// A result names its unit of work by the key and its dispatch by the task_id, and the evaluator
// counts dispatches rather than holding rows: this is the one number between them, read the
// other way from TaskRow. A row that is not a dispatch of the key is answered ErrNoDispatch.
func (w *Wide) RequeueOf(ctx context.Context, namespace string, key agk.TaskID, row string) (int, error) {
	// Compared as text, for the reason Lose gives: a runner wrote it.
	var requeue int
	err := w.tx.QueryRow(ctx,
		`select requeue from tasks where namespace = $1 and id = $2::text and idempotency_key = $3`,
		namespace, row, string(key)).Scan(&requeue)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s is no dispatch of %s", ErrNoDispatch, row, key)
	}
	if err != nil {
		return 0, fmt.Errorf("db: dispatch %s of task %s could not be read: %w", row, key, err)
	}
	return requeue, nil
}

// Loss is one dispatch the heartbeat declared lost: the key, which dispatch of it, and when.
type Loss struct {
	Task    agk.TaskID
	Requeue int
	At      time.Time
}

// Losses names the dispatches of one run that are lost and that nothing has requeued.
//
// It is how the controller hears what the heartbeat declared. "Liveness therefore lives in the
// database beside the task state", so a loss is written here first, by Pool.Lost or by Lose, and
// the evaluator is told on the next pass rather than by whoever noticed: a requeue is a decision,
// and deciding is the controller's. A dispatch already requeued past is not named, since the loss
// has been heard; one that was not requeued, because its step is not idempotent or its policy
// does not name lost, is named on every pass and changes nothing on any but the first.
func (w *Wide) Losses(ctx context.Context, namespace string, run agk.RunID) ([]Loss, error) {
	rows, err := w.tx.Query(ctx, `
		select t.idempotency_key, t.requeue, t.finished_at from tasks t
		where t.namespace = $1 and t.run_id = $2 and t.state = 'lost'
		  and not exists (select 1 from tasks later
		                  where later.namespace = t.namespace
		                    and later.idempotency_key = t.idempotency_key
		                    and later.requeue > t.requeue)
		order by t.idempotency_key`,
		namespace, string(run))
	if err != nil {
		return nil, fmt.Errorf("db: the lost tasks of run %s could not be read: %w", run, err)
	}
	defer rows.Close()
	var out []Loss
	for rows.Next() {
		var l Loss
		var at *time.Time
		if err := rows.Scan(&l.Task, &l.Requeue, &at); err != nil {
			return nil, err
		}
		if at != nil {
			l.At = *at
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ErrNotHeld is a loss naming a dispatch that was never bound to the runner it names.
var ErrNotHeld = errors.New("db: that dispatch was never bound to that runner")

// Lose moves to lost the one dispatch a loss names, where it is bound to the runner the loss
// names and still in flight, and answers whether anything moved.
//
// "lost is declared by the controller rather than reported here, and travels on a result only
// where a runner recovers one it had already lost." Such a result is written where the heartbeat
// writes its own losses, so that the evaluator hears of both the same way.
//
// The dispatch is named by its task_id rather than found by its key. A requeue keeps the key, and
// one runner may hold two dispatches of a key over time: the one it lost, and the requeue it took
// afterwards. Found by the key and the runner, a loss it reported about the first, late or
// delivered again, would land on the second and requeue a task that is running perfectly well.
// Named, it finds the first already lost and moves nothing, which is also what makes a loss
// delivered twice requeue once.
//
// It reaches only a dispatch bound to that runner at redemption, for the reason a heartbeat keeps
// alive only a runner's own tasks: a runner able to declare somebody else's task lost could send
// work round the fleet that is running perfectly well. A dispatch of the key bound to another
// runner, or to none, or no dispatch of the key at all, is answered ErrNotHeld.
//
// That runner is the one the result names, which is its own word and not something checked. A
// heartbeat is a request the API authenticates; a result is a message on a subject every runner
// may publish to, and nothing on it says who published it. So this keeps a runner that is wrong
// about what it holds off another's task, and does not keep off one that lies about its name.
// That waits for results to carry a publisher the controller can check, which the refusal of a
// result from a runner other than the one bound at redemption waits for too.
func (w *Wide) Lose(ctx context.Context, namespace string, key agk.TaskID, row, runner string, at time.Time) (bool, error) {
	switch {
	case runner == "":
		return false, fmt.Errorf("%w: a loss of %s declared by no runner", ErrNotHeld, key)
	case row == "":
		return false, fmt.Errorf("%w: a loss of %s that names no dispatch of it", ErrNotHeld, key)
	}
	// The row is compared as text, because a runner wrote it and the column's domain would
	// refuse a value that is not a ULID with an error rather than find nothing.
	var run string
	err := w.tx.QueryRow(ctx,
		`update tasks set state = 'lost', finished_at = $5
		 where namespace = $1 and id = $2::text and idempotency_key = $3 and runner = $4
		   and state in ('dispatched', 'running', 'publishing')
		 returning run_id`,
		namespace, row, string(key), runner, at).Scan(&run)
	switch {
	case err == nil:
		// And the run is left for the next sweep, as Pool.Lost leaves it, so that the loss
		// is heard even where whoever wrote it goes no further.
		if _, err := w.tx.Exec(ctx,
			`update runs set wake_at = null
			 where namespace = $1 and id = $2 and state in ('queued', 'running', 'waiting')`,
			namespace, run); err != nil {
			return false, fmt.Errorf("db: the run of task %s could not be woken for its loss: %w", key, err)
		}
		return true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("db: task %s could not be declared lost: %w", key, err)
	}
	var held bool
	if err := w.tx.QueryRow(ctx,
		`select exists (select 1 from tasks
		                where namespace = $1 and id = $2::text and idempotency_key = $3 and runner = $4)`,
		namespace, row, string(key), runner).Scan(&held); err != nil {
		return false, fmt.Errorf("db: task %s could not be read: %w", key, err)
	}
	if !held {
		return false, fmt.Errorf("%w: %s never held dispatch %s of %s", ErrNotHeld, runner, row, key)
	}
	return false, nil
}

// Published stamps the tasks whose messages have gone.
//
// Called after the bus accepted them and never before, which is what makes the stamp mean what
// it says. A task with no stamp is one the sweep publishes again, and publishing one dispatch
// twice is free: the stream deduplicates it on its task_id, and a runner refuses a key it holds
// or has completed. The stream does not deduplicate on the key, because a requeue after loss
// keeps the key and takes a new task_id, and it is meant to go out.
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

// EnvelopeRef is one envelope a run's decision references, as the database holds it.
//
// Every envelope the document names is counted here, not only the ones a step published. A
// shard's envelope is in the document until the step publishes, so a run resumed inside that
// window needs its bytes, and an object with no row at all is an object the collector cannot
// see and therefore never deletes. Counting all of them is what makes the store's contents
// accounted for rather than merely mostly accounted for.
type EnvelopeRef struct {
	Step agk.Step

	// Shard is the position in the step's slice, and PublishedByTheStep for the envelope
	// the step published rather than one a shard produced.
	Shard int

	Port   agk.Port
	Digest string
	Size   int64
	Items  int
}

// PublishedByTheStep is the Shard of an envelope the step published.
//
// "A port produces exactly one envelope, once, when the emitting step ends. A step split into
// shards has its shard envelopes concatenated port by port before publication." The publication
// is what steps.ports records and what a downstream step reads; a shard's own envelope is
// working material.
const PublishedByTheStep = -1

// setEnvelopes records what this decision's document references, and moves the counts.
//
// The diff is against what the run referenced before, so a pass that changed nothing changes no
// count, a port republished after a retry lowers what it replaced, and a run that ends holds
// exactly the envelopes its document names. The count is what makes sharing safe: two steps
// publishing identical bytes publish one object, and a purge that deleted by digest alone would
// delete what another step still names.
func (w *Wide) setEnvelopes(ctx context.Context, namespace string, run agk.RunID, was, next []EnvelopeRef) error {
	// Counted by digest and by how many times the run names it, because one run can name one
	// object from two places and dropping one of them must not drop the object.
	before, after := map[string]int{}, map[string]EnvelopeRef{}
	for _, e := range was {
		before[e.Digest]++
	}
	now := map[string]int{}
	for _, e := range next {
		if !hexDigest.MatchString(e.Digest) {
			return fmt.Errorf("db: the envelope of %s on %s names %q, and a digest is sixty-four lowercase hexadecimal characters", e.Step, e.Port, e.Digest)
		}
		now[e.Digest]++
		after[e.Digest] = e
	}

	for digest, count := range now {
		for range count - before[digest] {
			if _, _, err := raise(ctx, w.tx, namespace, "sha256:"+digest, after[digest].Size, envelopeMediaType); err != nil {
				return err
			}
		}
	}
	for digest, count := range before {
		for range count - now[digest] {
			if err := lower(ctx, w.tx, namespace, "sha256:"+digest, ""); err != nil {
				return err
			}
		}
	}

	// And what each step published, which is the chapter's "envelope digests for each port"
	// and what the run detail shows.
	published := map[agk.Step]map[string]port{}
	for _, e := range next {
		if e.Shard != PublishedByTheStep {
			continue
		}
		if published[e.Step] == nil {
			published[e.Step] = map[string]port{}
		}
		published[e.Step][string(e.Port)] = port{Digest: "sha256:" + e.Digest, Size: e.Size, Items: e.Items}
	}
	for step, ports := range published {
		encoded, err := json.Marshal(ports)
		if err != nil {
			return fmt.Errorf("db: the published ports of step %s could not be written: %w", step, err)
		}
		if _, err := w.tx.Exec(ctx,
			`update steps set ports = $4 where namespace = $1 and run_id = $2 and step = $3`,
			namespace, string(run), string(step), encoded); err != nil {
			return fmt.Errorf("db: the published ports of step %s could not be written: %w", step, err)
		}
	}
	return nil
}

// envelopesOf reads what a run's stored decision references.
//
// The document is package controller's shape, and this reaches two fields of it. That coupling
// is deliberate and is the narrowest available: the alternative is a second copy of the list in
// a column of its own, which would be two answers to what a run references and one of them
// eventually wrong.
func (w *Wide) envelopesOf(ctx context.Context, namespace string, run agk.RunID) ([]EnvelopeRef, error) {
	rows, err := w.tx.Query(ctx, `
		select e->>'step', (e->>'shard')::int, e->>'port', e->>'digest', (e->>'size')::bigint
		from runs, lateral jsonb_array_elements(coalesce(evaluation->'envelopes', '[]'::jsonb)) as e
		where namespace = $1 and id = $2`, namespace, string(run))
	if err != nil {
		return nil, fmt.Errorf("db: the envelopes of run %s could not be read: %w", run, err)
	}
	defer rows.Close()
	var out []EnvelopeRef
	for rows.Next() {
		var e EnvelopeRef
		if err := rows.Scan(&e.Step, &e.Shard, &e.Port, &e.Digest, &e.Size); err != nil {
			return nil, fmt.Errorf("db: the envelopes of run %s could not be read: %w", run, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Admission: what a queued run is waiting on.
//
// "queued: Created, waiting on a concurrency lock or on namespace quota." Both answers are
// counts over rows the controller already writes, which is why neither is a lock table: a group
// holds at most one started run, so what holds it is that run, and a quota is how many tasks a
// namespace has in flight.

// Holding names the run that holds a concurrency group, and whether it is this one.
//
// A run holds its group once it has started. A queued run holds nothing, which is what lets
// several queue up behind one without any of them believing it is in charge. The oldest queued
// run is the one that goes next, because a group that let the newest through would starve the
// first arrival for as long as triggers kept firing.
func (w *Wide) Holding(ctx context.Context, namespace, group string) (agk.RunID, error) {
	if group == "" {
		return "", nil
	}
	var id agk.RunID
	err := w.tx.QueryRow(ctx, `
		select id from runs
		where namespace = $1 and concurrency_group = $2
		  and state in ('running', 'waiting')
		order by created_at
		limit 1`, namespace, group).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: the holder of concurrency group %q could not be read: %w", group, err)
	}
	return id, nil
}

// NextInLine says whether this run is the one a free group should let through.
//
// Creation order, which is the only order that does not starve somebody: a group admitting the
// newest arrival would leave the first one queued for as long as triggers kept firing.
func (w *Wide) NextInLine(ctx context.Context, namespace, group string, run agk.RunID) (bool, error) {
	if group == "" {
		return true, nil
	}
	var first agk.RunID
	err := w.tx.QueryRow(ctx, `
		select id from runs
		where namespace = $1 and concurrency_group = $2 and state = 'queued'
		order by created_at, id
		limit 1`, namespace, group).Scan(&first)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: the queue of concurrency group %q could not be read: %w", group, err)
	}
	return first == run, nil
}

// SetGroup records which concurrency group a run belongs to.
//
// Stamped by the controller on the first pass that looks at the run, because the group is read
// out of the graph and whoever created the run does not read graphs.
func (w *Wide) SetGroup(ctx context.Context, namespace string, run agk.RunID, group string) error {
	if _, err := w.tx.Exec(ctx,
		`update runs set concurrency_group = $3 where namespace = $1 and id = $2`,
		namespace, string(run), nilIfEmpty(group)); err != nil {
		return fmt.Errorf("db: the concurrency group of run %s could not be recorded: %w", run, err)
	}
	return nil
}

// Slots is how many more tasks this namespace may have in flight.
//
// "max_concurrent_tasks: Caps how much of the runner fleet one namespace can hold at once, so a
// fan-out of ten thousand items cannot starve everyone else." What counts against it is a task
// that holds a runner or is on its way to one, which is the four states between being decided
// and being over.
func (w *Wide) Slots(ctx context.Context, namespace string) (int, error) {
	var ceiling, held int
	if err := w.tx.QueryRow(ctx,
		`select max_concurrent_tasks from namespaces where name = $1`, namespace).Scan(&ceiling); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("db: namespace %q has no row, so the ceiling on what it may hold is unknown", namespace)
		}
		return 0, fmt.Errorf("db: the task ceiling of namespace %q could not be read: %w", namespace, err)
	}
	if err := w.tx.QueryRow(ctx,
		`select count(*) from tasks
		 where namespace = $1 and state in ('pending', 'dispatched', 'running', 'publishing')
		   and published_at is not null`, namespace).Scan(&held); err != nil {
		return 0, fmt.Errorf("db: what namespace %q holds could not be counted: %w", namespace, err)
	}
	if held >= ceiling {
		return 0, nil
	}
	return ceiling - held, nil
}
