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
	// and happens before a run exists. A JSON object, written down as it arrived: the API
	// counts its values and decodes none of them, since nothing between the request and this
	// row reads them and decoding is what a document costs. Empty is none.
	Inputs json.RawMessage

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
	inputs := r.Inputs
	if len(inputs) == 0 {
		inputs = json.RawMessage(`{}`)
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

// RequestCancel records that a run is to be cancelled, and answers the state it is in.
//
// It asks and decides nothing. Ending a run and stopping what it holds is one decision, and it is
// the controller's, which reads the request on its next pass: the API writes down the moment it
// was asked and notifies, as it does for a run it starts. A run that has already ended is left as
// it is and answered in the state it ended in, so that the API wakes nobody for it; what the API
// says back is its own to decide, and it says nothing of the state. Asking again keeps the first
// moment.
//
// An identifier outside the alphabet runs are minted in is no run, as it is for Locate.
func (n *NS) RequestCancel(ctx context.Context, run agk.RunID, at time.Time) (agk.RunState, error) {
	if !minted(run) {
		return 0, fmt.Errorf("%w: %q", ErrNoRun, run)
	}
	var state string
	err := n.tx.QueryRow(ctx,
		`update runs set cancel_requested_at = coalesce(cancel_requested_at, $3)
		 where namespace = $1 and id = $2 and state in ('queued', 'running', 'waiting')
		 returning state`,
		n.namespace, string(run), at).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		// Over, or never there. A run that has ended stays ended, so the state read here
		// is the one the update was refused for.
		err = n.tx.QueryRow(ctx,
			`select state from runs where namespace = $1 and id = $2`,
			n.namespace, string(run)).Scan(&state)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return 0, fmt.Errorf("db: run %s could not be asked to cancel: %w", run, err)
	}
	var s agk.RunState
	if err := s.UnmarshalText([]byte(state)); err != nil {
		return 0, fmt.Errorf("db: run %s is in state %q: %w", run, state, err)
	}
	return s, nil
}

// Locate says which namespace and workflow a run is of, from its identifier alone.
//
// It is what a route about one run is authorised against: the path of such a route names the run
// and nothing it is of, and a permission such as workflow:run can be held on a single workflow.
// The identifier comes from a path, so one outside the alphabet runs are minted in is no run,
// answered without asking PostgreSQL: the column's domain refuses it with an error rather than
// finding nothing, and so does the protocol for U+0000 or bytes that are not UTF-8, which would
// have been a 500 for anybody holding a credential.
func (w *Wide) Locate(ctx context.Context, run agk.RunID) (namespace, workflow string, err error) {
	if !minted(run) {
		return "", "", fmt.Errorf("%w: %q", ErrNoRun, run)
	}
	err = w.tx.QueryRow(ctx,
		`select namespace, workflow from runs where id = $1`, string(run)).Scan(&namespace, &workflow)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	if err != nil {
		return "", "", fmt.Errorf("db: run %s could not be found: %w", run, err)
	}
	return namespace, workflow, nil
}

// minted says whether an identifier is in the alphabet the ulid domain holds, Crockford base32 of
// any length, which is every run identifier the engine has minted and the documentation printed.
func minted(run agk.RunID) bool {
	if run == "" {
		return false
	}
	for i := 0; i < len(run); i++ {
		c := run[i]
		if !('0' <= c && c <= '9' || 'A' <= c && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U') {
			return false
		}
	}
	return true
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

	// CancelRequestedAt is when somebody asked through the API for this run to be cancelled, and
	// zero while nobody has. The request is the API's to write and the cancellation is the
	// controller's to carry out, so a run holding one is a run the next pass ends.
	CancelRequestedAt time.Time
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
	var wake, cancel *time.Time
	err := w.tx.QueryRow(ctx,
		`select namespace, id, workflow, commit, state, evaluation, seq, inputs, trigger, wake_at,
		        cancel_requested_at
		 from runs where id = $1`, string(run)).
		Scan(&e.Namespace, &e.Run, &e.Workflow, &e.Commit, &state, &e.Document, &e.Seq,
			&inputs, &trigger, &wake, &cancel)
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
	if cancel != nil {
		e.CancelRequestedAt = *cancel
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
//
// Nor does a decision move a dispatch back along the way to its ending. Running and publishing
// are written by Progress, from the runner holding the dispatch, and the evaluator never hears of
// them: its state says dispatched until the ending, so every pass in between would write
// dispatched over running. A dispatch only moves forwards, since a further attempt and a requeue
// are rows of their own, so the later of the two states in flight is kept. That is the row saying
// more than the decision rather than something the decision has not heard, and it is answered as
// heard.
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
		                  then tasks.state
		                  when tasks.state in ('running', 'publishing')
		                   and excluded.state in ('pending', 'dispatched')
		                  then tasks.state
		                  when tasks.state = 'publishing' and excluded.state = 'running'
		                  then tasks.state
		                  else excluded.state end,
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
	return held != agk.TaskLost.String() || t.State == agk.TaskLost, nil
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

// HeldBy answers which runner one dispatch of a task was bound to when its grant was redeemed, and
// the empty string where nobody has redeemed it.
//
// It is the binding Redeem made, read on the way out: a result is taken from the runner its task
// was bound to and from no other. The dispatch is named twice, by its row and by its key, and both
// are compared with what is recorded, for the reason Redeem compares them on the way in: a row of
// one task and the key of another is an answer somebody assembled out of two, and neither half is
// evidence about the other. The row is compared as text, because a runner wrote it and the
// column's domain would refuse a value that is not a ULID with an error rather than find nothing.
func (w *Wide) HeldBy(ctx context.Context, namespace string, key agk.TaskID, row string) (string, error) {
	var runner *string
	err := w.tx.QueryRow(ctx,
		`select runner from tasks where namespace = $1 and id = $2::text and idempotency_key = $3`,
		namespace, row, string(key)).Scan(&runner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s is no dispatch of %s", ErrNoDispatch, row, key)
	}
	if err != nil {
		return "", fmt.Errorf("db: dispatch %s of task %s could not be read: %w", row, key, err)
	}
	if runner == nil {
		return "", nil
	}
	return *runner, nil
}

// BindUnredeemed binds one dispatch nobody has redeemed to the runner whose ending of it is being
// written, and answers who holds it once that is done.
//
// Two endings come for a dispatch no runner is bound to. A grant that would not redeem ends a
// dispatch that never reached a container and that no redemption bound: a runner redeems before it
// pulls, so a refused pull is reported by the runner the redemption bound, but a refused redemption
// binds nobody. And a requeue that comes back to the host which already ended its key is never
// redeemed at all: the host answers it from its record, through the runner that redeemed an earlier
// dispatch of the key. The runner reporting either is bound to the dispatch here, as a redemption
// would have bound it, so that no other runner can report a second ending for it and no redemption
// can follow. A dispatch somebody already holds keeps its holder, and the answer says who that is;
// the row is locked by the update, so a redemption racing it binds first or finds it bound.
//
// It belongs in the transaction that writes the ending, and never in one of its own. Lost takes a
// bound dispatch in flight for one a runner redeemed, so a binding committed without its ending
// would be swept lost, counted from the dispatch, as if a container had run and its host gone
// quiet.
func (w *Wide) BindUnredeemed(ctx context.Context, namespace string, key agk.TaskID, row, runner string) (string, error) {
	if runner == "" {
		return "", fmt.Errorf("db: dispatch %s of task %s bound to no runner", row, key)
	}
	var holder string
	err := w.tx.QueryRow(ctx,
		`update tasks set runner = coalesce(runner, $4)
		 where namespace = $1 and id = $2::text and idempotency_key = $3
		 returning runner`,
		namespace, row, string(key), runner).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s is no dispatch of %s", ErrNoDispatch, row, key)
	}
	if err != nil {
		return "", fmt.Errorf("db: dispatch %s of task %s could not be bound: %w", row, key, err)
	}
	return holder, nil
}

// RedeemedBefore answers whether a runner redeemed a dispatch of a key earlier than the one a row
// records.
//
// It is what a requeue is answered from when it comes back to the host that already ended its key.
// That host refuses to run the key again and reports the ending it recorded under the requeue's
// task_id, and nobody redeems the requeue, so its ending cannot be held to a redemption of its own.
// It is held to the redemption of the dispatch the host ended, which is a dispatch of the same key
// handed out before it: the runner reporting it was given that work, so it reaches no further
// than "the tasks in its hands". A binding made by an ending that never reached a container is no
// redemption, and does not count; nor does a later dispatch, which cannot be what the requeue came
// back to.
func (w *Wide) RedeemedBefore(ctx context.Context, namespace string, key agk.TaskID, row, runner string) (bool, error) {
	if runner == "" {
		return false, nil
	}
	// The row is compared as text, for the reason HeldBy gives.
	var redeemed bool
	err := w.tx.QueryRow(ctx, `
		select exists (
		  select 1 from tasks this
		  join tasks earlier
		    on earlier.namespace = this.namespace
		   and earlier.idempotency_key = this.idempotency_key
		   and earlier.requeue < this.requeue
		  join task_grants g
		    on g.namespace = earlier.namespace and g.task_id = earlier.id
		  where this.namespace = $1 and this.id = $2::text and this.idempotency_key = $3
		    and earlier.runner = $4 and g.redeemed_at is not null)`,
		namespace, row, string(key), runner).Scan(&redeemed)
	if err != nil {
		return false, fmt.Errorf("db: the dispatches of task %s before %s could not be read: %w", key, row, err)
	}
	return redeemed, nil
}

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
// It is how the controller hears what its sweep declared. "Liveness therefore lives in the
// database beside the task state", so a loss is written here first, by Lost or by Lose, and
// the evaluator is told on the next pass rather than by whoever noticed: a requeue is a decision,
// and deciding is the controller's. A dispatch already requeued past is not named, since the loss
// has been heard; one that was not requeued, because its step is not idempotent, its policy does
// not name lost or its key was already handed out again as often as max_requeues allows, is named
// on every pass and changes nothing on any but the first.
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

// ErrNotHeld is a loss or a progress message naming a dispatch that was never bound to the runner
// it names.
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
// It reaches only a dispatch bound to that runner, for the reason a heartbeat keeps alive only a
// runner's own tasks: a runner able to declare somebody else's task lost could send work round
// the fleet that is running perfectly well. A dispatch of the key bound to another runner, or to
// none, or no dispatch of the key at all, is answered ErrNotHeld. And it moves only one still in
// flight, which is one that runner redeemed: a runner bound by reporting that a task never reached
// a container was bound with that ending, so the dispatch is over and nothing moves.
//
// That runner is the one the result names, and it is the one that published it: a heartbeat is a
// request the API authenticates, and a result arrives on a subject only its runner's credential
// may publish on, which package bus/control holds the result's runner field to. So this keeps off
// another's task both a runner that is wrong about what it holds and one that lies about its name.
func (w *Wide) Lose(ctx context.Context, namespace string, key agk.TaskID, row, runner string, at time.Time) (bool, error) {
	switch {
	case runner == "":
		return false, fmt.Errorf("%w: a loss of %s declared by no runner", ErrNotHeld, key)
	case row == "":
		return false, fmt.Errorf("%w: a loss of %s that names no dispatch of it", ErrNotHeld, key)
	}
	// The row is compared as text, because a runner wrote it and the column's domain would
	// refuse a value that is not a ULID with an error rather than find nothing.
	//
	// The run's row is locked before the task's, which is the order a decision takes them in. A
	// loss reported while its run is being decided then waits for the decision, where holding the
	// task the decision is about to write would be a deadlock, and PostgreSQL would end one of
	// the two. A dispatch that is not there locks nothing, and is refused below.
	if _, err := w.tx.Exec(ctx,
		`select 1 from runs
		 where namespace = $1
		   and id = (select run_id from tasks where namespace = $1 and id = $2::text)
		 for update`,
		namespace, row); err != nil {
		return false, fmt.Errorf("db: the run of task %s could not be locked: %w", key, err)
	}
	var run string
	err := w.tx.QueryRow(ctx,
		`update tasks set state = 'lost', finished_at = $5
		 where namespace = $1 and id = $2::text and idempotency_key = $3 and runner = $4
		   and state in ('dispatched', 'running', 'publishing')
		 returning run_id`,
		namespace, row, string(key), runner, at).Scan(&run)
	switch {
	case err == nil:
		// And the run is left for the next sweep, as Lost leaves it, so that the loss is
		// heard even where whoever wrote it goes no further.
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

// Progress moves the one dispatch a progress message names to running or publishing, where it is
// bound to the runner the message names, and answers whether anything moved.
//
// "running: The container is running. publishing: The container is finished; its outputs are being
// collected and uploaded." Only the runner holding a dispatch can see either, so it says so on its
// results subject, and this is where it is written: on the row alone, for a person reading the run.
// The evaluator's state is not told, since nothing it decides depends on either, and the row is
// what the run detail reads.
//
// Only forwards, and never over an ending. Running moves a dispatched row, publishing a dispatched
// or running one, and a row already there or past it is left as it is, so a message delivered twice,
// out of order or after the result changes nothing, whichever commits first: the row is locked by
// the update, and one that waited on a decision writing the ending finds the ending. Nor on the row
// of a run that has ended, which has nothing to learn, as a result for one has not.
//
// It reaches only a dispatch bound to that runner, for the reason Lose gives: a runner able to move
// somebody else's task could show work running that nobody runs. A dispatch bound to another
// runner, or to none, or no dispatch of the key at all, is answered ErrNotHeld. The dispatch is
// named by its task_id and its key together, for the reason HeldBy gives, so a host still running a
// dispatch that was declared lost moves nothing on the requeue somebody else may hold.
//
// A dispatch bound to that runner and still pending is answered ErrNotYetDispatched, and moves
// nothing. The controller publishes a task before it records the dispatch, so a runner quick enough
// redeems, starts and reports a task the rows still hold as planned, and even more so where a pass
// publishing a few hundred shards records them all at the end, or dies before it does and leaves
// them to the next sweep. Written over pending, running would stand on a row Unpublished and
// Actionable no longer find, and a message whose pass died would never be sent again; taken as no
// news, it would be acknowledged and never said again, and the task would read dispatched until its
// ending. So it is left for a later delivery, by when the dispatch is recorded.
func (w *Wide) Progress(ctx context.Context, namespace string, key agk.TaskID, row, runner string, to agk.TaskState) (bool, error) {
	var from []string
	switch to {
	case agk.TaskRunning:
		from = []string{agk.TaskDispatched.String()}
	case agk.TaskPublishing:
		from = []string{agk.TaskDispatched.String(), agk.TaskRunning.String()}
	default:
		return false, fmt.Errorf("db: task %s cannot move to %s on its runner's word, which says running or publishing and leaves the ending to a result", key, to)
	}
	switch {
	case runner == "":
		return false, fmt.Errorf("%w: progress of %s from no runner", ErrNotHeld, key)
	case row == "":
		return false, fmt.Errorf("%w: progress of %s that names no dispatch of it", ErrNotHeld, key)
	}
	// The run's row is locked before the task's, which is the order a decision takes them in,
	// and for share, which a decision writing the run waits on and waits for. Without it a
	// progress message waiting on the task's row while a decision ended the run would be written
	// once that decision committed: the task's row is read again as it stands then, but the run
	// in the condition below as it stood when the statement began, still going. With it, the
	// statement begins once the decision has committed, and reads the run as that left it.
	if _, err := w.tx.Exec(ctx,
		`select 1 from runs
		 where namespace = $1
		   and id = (select run_id from tasks where namespace = $1 and id = $2::text)
		 for share`,
		namespace, row); err != nil {
		return false, fmt.Errorf("db: the run of task %s could not be locked: %w", key, err)
	}
	// The row is compared as text, for the reason Lose gives.
	tag, err := w.tx.Exec(ctx,
		`update tasks set state = $5
		 where namespace = $1 and id = $2::text and idempotency_key = $3 and runner = $4
		   and state = any($6)
		   and exists (select 1 from runs
		               where runs.namespace = tasks.namespace and runs.id = tasks.run_id
		                 and runs.state in ('queued', 'running', 'waiting'))`,
		namespace, row, string(key), runner, to.String(), from)
	if err != nil {
		return false, fmt.Errorf("db: the progress of task %s could not be written: %w", key, err)
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	var state string
	var going bool
	err = w.tx.QueryRow(ctx,
		`select tasks.state::text, runs.state in ('queued', 'running', 'waiting')
		 from tasks join runs on runs.namespace = tasks.namespace and runs.id = tasks.run_id
		 where tasks.namespace = $1 and tasks.id = $2::text and tasks.idempotency_key = $3 and tasks.runner = $4`,
		namespace, row, string(key), runner).Scan(&state, &going)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("%w: %s does not hold dispatch %s of %s", ErrNotHeld, runner, row, key)
	case err != nil:
		return false, fmt.Errorf("db: task %s could not be read: %w", key, err)
	case state == agk.TaskPending.String() && going:
		return false, fmt.Errorf("%w: dispatch %s of %s is still pending", ErrNotYetDispatched, row, key)
	}
	return false, nil
}

// ErrNotYetDispatched is progress on a dispatch its runner holds and the controller has not yet
// recorded as dispatched, which a later delivery will find recorded.
var ErrNotYetDispatched = errors.New("db: that dispatch is not recorded as dispatched yet")

// CancelTasks moves to cancelled every task of a run that is not over, and answers the keys of
// those a runner had redeemed.
//
// "cancelled: Stopped because the run was cancelled by a principal, by a concurrency group or by
// a merge: first." It belongs in the transaction that writes the cancellation, because the
// evaluator ends a run and leaves its tasks where they were: it names the ones a runner holds, to
// be stopped, and the endings those runners send back reach a run with nothing left to learn.
// Left in flight, a task whose message was still on the queue would redeem its grant and start a
// container for a run that had ended, and every one of them would count against
// max_concurrent_tasks for good. A dispatch the heartbeat declared lost keeps its loss, which is
// the one record that its runner went quiet.
//
// The keys it answers are to be stopped whatever the evaluator's document says of them. A pass
// that published a task and died before recording the dispatch leaves the task pending in the
// document, where the evaluator names nothing to stop, and a runner that took the message
// meanwhile has redeemed its grant and started the container. The row knows, because the
// redemption bound it.
func (w *Wide) CancelTasks(ctx context.Context, namespace string, run agk.RunID, at time.Time) ([]agk.TaskID, error) {
	return w.endTasks(ctx, namespace, run, agk.TaskCancelled, at)
}

// TimeOutTasks moves to timed_out every task of a run that is not over, for a run whose deadline
// has passed, and answers the keys of those a runner had redeemed.
//
// "Once it passes, the tasks still running are stopped and the run ends there." It is
// CancelTasks for the other way a run ends under its tasks, and for the same reasons: a message
// still on the queue would otherwise redeem its grant for a run that has ended, the run's tasks
// would count against max_concurrent_tasks for good, and the row is what names a runner's
// dispatch the document does not. It is also what the heartbeat answers cancel from, so that a
// runner that missed the deadline's stop on agentiik.stops hears it at its next heartbeat rather
// than running the container to its own deadline.
func (w *Wide) TimeOutTasks(ctx context.Context, namespace string, run agk.RunID, at time.Time) ([]agk.TaskID, error) {
	return w.endTasks(ctx, namespace, run, agk.TaskTimedOut, at)
}

// StopCode writes onto one dispatch that a run's ending stopped the exit code its container
// stopped with, as its runner reported it, and answers whether a row took it.
//
// CancelTasks and TimeOutTasks end a run's tasks in the pass that ends the run, before any
// container has exited, so the rows they end carry no code; the runner's report comes later, to a
// run with nothing left to decide. "A timed_out or cancelled task carries an exit code wherever a
// container ran" all the same, and this is where it lands. Only on a row that is stopped and has no
// code yet, so an ending is written once; only on the dispatch named by its row and its key, as
// HeldBy compares them; and only from the runner the dispatch is bound to. When it started is kept
// where the row has none, and when it finished is the moment the run ended it, which stays.
func (w *Wide) StopCode(ctx context.Context, namespace string, key agk.TaskID, row, runner string, code int, started time.Time) (bool, error) {
	tag, err := w.tx.Exec(ctx,
		`update tasks set exit_code = $5, started_at = coalesce(started_at, $6)
		 where namespace = $1 and id = $2::text and idempotency_key = $3 and runner = $4
		   and state in ('cancelled', 'timed_out') and exit_code is null`,
		namespace, row, string(key), runner, code, nilIfZero(started))
	if err != nil {
		return false, fmt.Errorf("db: the exit code of dispatch %s of task %s could not be written: %w", row, key, err)
	}
	return tag.RowsAffected() == 1, nil
}

// endTasks writes the ending given over every task of a run that is not over, and answers the
// keys of those a runner had redeemed.
func (w *Wide) endTasks(ctx context.Context, namespace string, run agk.RunID, ending agk.TaskState, at time.Time) ([]agk.TaskID, error) {
	rows, err := w.tx.Query(ctx,
		`with ended as (
		   update tasks set state = $4, finished_at = coalesce(finished_at, $3)
		   where namespace = $1 and run_id = $2 and state in ('pending', 'dispatched', 'running', 'publishing')
		   returning idempotency_key, runner)
		 select idempotency_key from ended where runner is not null order by idempotency_key`,
		namespace, string(run), at, ending.String())
	if err != nil {
		return nil, fmt.Errorf("db: the tasks of run %s could not be ended %s: %w", run, ending, err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[agk.TaskID])
	if err != nil {
		return nil, fmt.Errorf("db: the tasks of run %s could not be ended %s: %w", run, ending, err)
	}
	return held, nil
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
// is four things: a run that has never been decided, a run whose clock has come round, a run
// holding a task whose message never went, and a run somebody has asked to cancel, whose clock
// says nothing about when that was.
func (w *Wide) Actionable(ctx context.Context, now time.Time, batch int) ([]agk.RunID, error) {
	batch, err := batchOf(batch)
	if err != nil {
		return nil, err
	}
	rows, err := w.tx.Query(ctx, `
		select id from runs
		where state in ('queued', 'running', 'waiting')
		  and (wake_at is null or wake_at <= $1
		       or cancel_requested_at is not null
		       or exists (select 1 from tasks t
		                  where t.namespace = runs.namespace and t.run_id = runs.id
		                    and t.published_at is null
		                    and t.state in ('pending', 'dispatched')))
		order by coalesce(least(wake_at, cancel_requested_at), created_at)
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
