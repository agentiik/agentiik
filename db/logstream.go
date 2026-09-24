package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Following the log of a step, which is what GET /api/v1/runs/{id}/steps/{step}/logs serves:
// "SSE log stream, history then live".
//
// A step's log is the logs of its dispatches, one per task row, since a retry is a new attempt and
// a requeue after loss a new dispatch of the same attempt, and each ships a log of its own. What is
// read here is where each stands and what it holds, a chunk at a time, and what tells a reader that
// a chunk landed, which is a notification rather than a question asked on a clock.

// LogChannel is the channel a shipment says on that a task's log moved on.
//
// The payload is the idempotency key the log is of, which carries the run and the step a reader
// follows and nothing else: "a notification carries only an identifier". It is sent in the
// transaction that records the chunk, and "PostgreSQL delivers such an event only once the
// transaction commits", so a reader woken by it finds the chunk there. As on the run channel, it
// is a latency optimisation and not the guarantee: a reader that was not listening when it went
// out finds the chunk when WatchLogs next sweeps.
const LogChannel = "agentiik_task_log"

// ErrNoStep is a run with no step of that name.
var ErrNoStep = errors.New("db: the run has no step of that name")

// StepLog is where the log of one step stands: whether anything more can be dispatched for it, and
// every dispatch it has had, in the order they were made.
type StepLog struct {
	Run     agk.RunState
	Verdict agk.Verdict

	// Expired is a run whose retention has run out, whose logs the purge takes or has taken.
	Expired bool

	Dispatches []Dispatch
}

// Over is a step nothing more is dispatched for: it reached its verdict, or its run ended, which
// ends every step of it.
func (s StepLog) Over() bool { return s.Verdict.Terminal() || s.Run.Terminal() }

// Dispatch is one task row of a step and where its log stands.
type Dispatch struct {
	// Row is the task_id, and Task the key it is a dispatch of, which carries the attempt and the
	// shard. Requeue counts the times the attempt was handed out again after a loss, 0 for the
	// first, so that two dispatches of one key are told apart by a number and not only by an
	// identifier.
	Row     string
	Task    agk.TaskID
	Attempt int
	Shard   *agk.Shard
	Requeue int

	State agk.TaskState

	// Bound is a dispatch a redemption bound to a runner, which is the only kind that ships a log.
	Bound bool

	// FinishedAt is when the dispatch ended, and zero while it has not or where nothing said.
	FinishedAt time.Time

	// Lines, Truncated and FinalSeq are where its log stands, all zero while nothing has shipped.
	Lines     int
	Truncated bool
	FinalSeq  int
}

// StepLog reads where the log of one step stands, and at most limit of its dispatches in the order
// they were made, from the one since names on where it names one, since a reader has let go of
// those before it. The limit is what a reader held on the first shard of a fan-out of ten thousand
// reads each time, rather than the ten thousand.
//
// The order is that of their identifiers: a task_id is minted when its row is, and a row is made
// when the evaluator hands the attempt out, so a dispatch made later sorts later and a reader that
// has gone past one never finds another made before it. Sorted byte by byte, as a ULID sorts,
// whatever collation the database was created with.
func (n *NS) StepLog(ctx context.Context, run agk.RunID, step agk.Step, since string, limit int) (StepLog, error) {
	var s StepLog
	var runState, verdict string
	err := n.tx.QueryRow(ctx, `
		select r.state, s.state, coalesce(r.expires_at <= now(), false)
		from runs r join steps s on s.namespace = r.namespace and s.run_id = r.id
		where r.namespace = $1 and r.id = $2 and s.step = $3`, n.namespace, string(run), string(step)).
		Scan(&runState, &verdict, &s.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := n.tx.QueryRow(ctx, `select exists (select 1 from runs where namespace = $1 and id = $2)`,
			n.namespace, string(run)).Scan(&exists); err != nil {
			return StepLog{}, fmt.Errorf("db: run %s could not be read: %w", run, err)
		}
		if !exists {
			return StepLog{}, fmt.Errorf("%w: %s", ErrNoRun, run)
		}
		return StepLog{}, fmt.Errorf("%w: %s", ErrNoStep, step)
	}
	if err != nil {
		return StepLog{}, fmt.Errorf("db: step %s of run %s could not be read: %w", step, run, err)
	}
	if err := s.Run.UnmarshalText([]byte(runState)); err != nil {
		return StepLog{}, err
	}
	if err := s.Verdict.UnmarshalText([]byte(verdict)); err != nil {
		return StepLog{}, err
	}

	rows, err := n.tx.Query(ctx, `
		select t.id, t.idempotency_key, t.attempt, t.shard_index, t.shard_of, t.requeue, t.state,
		       t.runner is not null, t.finished_at,
		       coalesce(l.lines, 0), coalesce(l.truncated, false), coalesce(l.final_seq, 0)
		from tasks t
		left join task_logs l on l.namespace = t.namespace and l.task_id = t.id
		where t.namespace = $1 and t.run_id = $2 and t.step = $3
		  and ($4 = '' or t.id collate "C" >= $4 collate "C")
		order by t.id collate "C"
		limit $5`, n.namespace, string(run), string(step), since, limit)
	if err != nil {
		return StepLog{}, fmt.Errorf("db: the tasks of step %s of run %s could not be read: %w", step, run, err)
	}
	defer rows.Close()
	for rows.Next() {
		var d Dispatch
		var state string
		var index, of *int
		var finished *time.Time
		if err := rows.Scan(&d.Row, &d.Task, &d.Attempt, &index, &of, &d.Requeue, &state,
			&d.Bound, &finished, &d.Lines, &d.Truncated, &d.FinalSeq); err != nil {
			return StepLog{}, fmt.Errorf("db: the tasks of step %s of run %s could not be read: %w", step, run, err)
		}
		if err := d.State.UnmarshalText([]byte(state)); err != nil {
			return StepLog{}, err
		}
		if index != nil && of != nil {
			d.Shard = &agk.Shard{Index: *index, Of: *of}
		}
		if finished != nil {
			d.FinishedAt = *finished
		}
		s.Dispatches = append(s.Dispatches, d)
	}
	if err := rows.Err(); err != nil {
		return StepLog{}, fmt.Errorf("db: the tasks of step %s of run %s could not be read: %w", step, run, err)
	}
	return s, nil
}

// LogChunks reads the chunks of one dispatch's log that hold lines, after seq after and at most
// limit of them, in order: what a reader who has had everything up to after reads next.
func (n *NS) LogChunks(ctx context.Context, row string, after, limit int) ([]LogChunk, error) {
	rows, err := n.tx.Query(ctx,
		`select `+chunkColumns+` from task_log_chunks
		 where namespace = $1 and task_id = $2 and seq > $3 and object_key is not null
		 order by seq limit $4`, n.namespace, row, after, limit)
	if err != nil {
		return nil, fmt.Errorf("db: the chunks of the log of task %s could not be read: %w", row, err)
	}
	chunks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LogChunk, error) { return scanChunk(r) })
	if err != nil {
		return nil, fmt.Errorf("db: the chunks of the log of task %s could not be read: %w", row, err)
	}
	return chunks, nil
}

// unlistenWithin bounds the unlisten on the way out of WatchLogs, on a connection that may no longer
// answer, for the reason rollbackWithin bounds a rollback.
const unlistenWithin = 5 * time.Second

// WatchLogs calls on with the key of every log a shipment moved on, and with the empty key on every
// sweep. It blocks until ctx is done, or the connection it listens on fails, and answers why.
//
// It takes a session of its own, since LISTEN is a property of a connection. The first thing it
// does once listening is sweep: whatever was shipped before it listened is a notification it did
// not hear, and the sweep is what makes that cost latency rather than lines. Then it sweeps every
// sweep, which is also what tells a reader of what no shipment announces, a task that ended or a
// step that reached its verdict. Every sweep and not every sweep without a notification: on an
// installation where some task ships something every few seconds, a quiet spell would never come,
// and a reader whose step ended would never hear it.
func (p *Pool) WatchLogs(ctx context.Context, sweep time.Duration, on func(agk.TaskID)) error {
	if on == nil || sweep <= 0 {
		return errors.New("db: WatchLogs needs something to call and a sweep to call it on")
	}
	return p.Session(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		if _, err := conn.Exec(ctx, `listen `+LogChannel); err != nil {
			return fmt.Errorf("db: the log channel could not be listened to: %w", err)
		}
		defer func() {
			unlisten, stop := context.WithTimeout(context.WithoutCancel(ctx), unlistenWithin)
			defer stop()
			conn.Exec(unlisten, `unlisten `+LogChannel)
		}()
		on("")
		next := time.Now().Add(sweep)
		for {
			waiting, stop := context.WithDeadline(ctx, next)
			note, err := conn.Conn().WaitForNotification(waiting)
			stop()
			switch {
			case err == nil:
				on(agk.TaskID(note.Payload))
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, context.DeadlineExceeded):
			default:
				return fmt.Errorf("db: the connection listening for logs failed: %w", err)
			}
			if !time.Now().Before(next) {
				on("")
				next = time.Now().Add(sweep)
			}
		}
	})
}
