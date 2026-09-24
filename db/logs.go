package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/token"
	"github.com/jackc/pgx/v5"
)

// A task's log, as the index to it.
//
// "Envelopes and logs are not stored in the database: it keeps only their digests and URIs." The
// lines of a log are in the object store, one object per chunk the runner shipped, and this is the
// index that says which chunk is where and where the log stands, which is what a shipment is
// answered from and what a reader follows to put the log back in order. Package db does not reach
// the object store, so it never holds a line: the API writes the chunk and records it here.

// ErrLogExpired is a shipment for a run whose retention has run out, whose logs the purge takes or
// has taken, so that a chunk arriving then would name an object nothing will ever delete.
var ErrLogExpired = errors.New("db: the logs of that run are past their retention")

// TaskLog is where one dispatch's log stands.
type TaskLog struct {
	Namespace string

	// Row is the dispatch, and Task the key it is a dispatch of, which is what the log's URI
	// names: a requeue after loss keeps its key and has a log of its own under its own row.
	Row  string
	Task agk.TaskID

	// NextSeq is the chunk the API expects next, and ShippedLines how far into the log the
	// runner has shipped by its own count, which is where that chunk has to begin.
	NextSeq      int
	ShippedLines int

	// Lines, Bytes and Truncated are what the API holds.
	Lines     int
	Bytes     int64
	Truncated bool

	// FinalSeq is the chunk that closed the log, and zero while nothing has.
	FinalSeq int
}

// URI is where the log is addressed from.
func (l TaskLog) URI() (agk.LogURI, error) { return agk.NewLogURI(l.Task) }

// LogChunk is one chunk of a log as the index holds it: where it sat in what the runner shipped,
// what it carried, and what of it was written where.
type LogChunk struct {
	Seq           int
	FirstLine     int
	Shipped       int
	ShippedDigest string

	// Lines and Bytes are what was written, and Key the object holding them, empty where the
	// cap kept none of the chunk's lines.
	Lines int
	Bytes int64
	Key   string
}

// ShippingLog finds the log a shipment writes to, and holds it until the transaction ends.
//
// The grant says which dispatch, and it is found by its hash and the row its text names, as a
// redemption finds it. The body's key has to be the one the dispatch is of: "a shipment whose body
// and grant name different tasks is refused, rather than written under whichever of the two was
// read first", and it is refused as a grant that opens nothing, as a redemption is.
//
// The grant's expiry is not held to, unlike at a redemption. It is the task's deadline, and the
// last chunk of a task stopped at its deadline arrives after it: refused then, the log of every
// task that ran out of time would lose the lines that say why. What a grant past its expiry could
// open is what the runner the task is bound to could open anyway, since the binding is what is
// checked, and a grant lifted from a queue opens nothing without that runner's credential.
//
// The task is the runner's when a redemption bound it to it, whatever state the task is in since:
// a cancelled task's container is still stopping and its last lines still coming, and a task
// declared lost may have been only cut off, which the host then finishes. A runner draining, or
// revoked and in its grace, is finishing what it holds, which is what a drain and a grace are for,
// and shipping its log is part of finishing. A task nobody bound, or somebody else, is ErrTaskHeld.
func (w *Wide) ShippingLog(ctx context.Context, clear string, task agk.TaskID, runner string) (TaskLog, error) {
	id, ok := token.TaskOf(clear)
	if !ok {
		return TaskLog{}, ErrNoGrant
	}
	if runner == "" {
		return TaskLog{}, errors.New("db: a shipment by no runner, and a log is shipped by the machine its task is bound to")
	}

	var l TaskLog
	var holder *string
	var expired bool
	err := w.tx.QueryRow(ctx, `
		select g.namespace, t.idempotency_key, t.runner,
		       coalesce(r.expires_at <= now(), false)
		from task_grants g
		join tasks t on t.namespace = g.namespace and t.id = g.task_id
		join runs r on r.namespace = t.namespace and r.id = t.run_id
		where g.task_id = $1 and g.hash = $2`, id, token.Hash(clear)).
		Scan(&l.Namespace, &l.Task, &holder, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskLog{}, ErrNoGrant
	}
	if err != nil {
		return TaskLog{}, fmt.Errorf("db: the grant could not be read: %w", err)
	}
	l.Row = id

	switch {
	case task != l.Task:
		return TaskLog{}, ErrNoGrant
	case holder == nil || *holder != runner:
		return TaskLog{}, ErrTaskHeld
	case expired:
		return TaskLog{}, ErrLogExpired
	}

	// Made on the first chunk and held from there: two shipments of one log at once are taken
	// one after the other, since what the second may write depends on what the first did.
	if _, err := w.tx.Exec(ctx,
		`insert into task_logs (namespace, task_id) values ($1, $2) on conflict do nothing`,
		l.Namespace, l.Row); err != nil {
		return TaskLog{}, fmt.Errorf("db: the log of task %s could not be opened: %w", l.Row, err)
	}
	var final *int
	if err := w.tx.QueryRow(ctx, `
		select next_seq, shipped_lines, lines, bytes, truncated, final_seq
		from task_logs where namespace = $1 and task_id = $2
		for update`, l.Namespace, l.Row).
		Scan(&l.NextSeq, &l.ShippedLines, &l.Lines, &l.Bytes, &l.Truncated, &final); err != nil {
		return TaskLog{}, fmt.Errorf("db: the log of task %s could not be read: %w", l.Row, err)
	}
	if final != nil {
		l.FinalSeq = *final
	}
	return l, nil
}

// ShippedChunk is one chunk of a log the index already holds, and false where it holds none.
func (w *Wide) ShippedChunk(ctx context.Context, l TaskLog, seq int) (LogChunk, bool, error) {
	c, err := scanChunk(w.tx.QueryRow(ctx,
		`select `+chunkColumns+` from task_log_chunks where namespace = $1 and task_id = $2 and seq = $3`,
		l.Namespace, l.Row, seq))
	if errors.Is(err, pgx.ErrNoRows) {
		return LogChunk{}, false, nil
	}
	if err != nil {
		return LogChunk{}, false, fmt.Errorf("db: chunk %d of the log of task %s could not be read: %w", seq, l.Row, err)
	}
	return c, true, nil
}

// ShipChunk records what a shipment did to a log: the chunk, where it was taken, and the log as it
// now stands, which is was with the chunk counted in.
//
// A chunk is recorded unless the log was already cut short before it arrived, since from then on
// nothing is written and a runner that went on shipping would otherwise grow the index a row per
// request. The task's row is given the log's URI the first time, which is what the purge finds a
// log by: a task whose result never came back still has its log swept with its run.
func (w *Wide) ShipChunk(ctx context.Context, was, now TaskLog, c *LogChunk) error {
	if c != nil {
		var key *string
		if c.Key != "" {
			key = &c.Key
		}
		if _, err := w.tx.Exec(ctx, `
			insert into task_log_chunks (namespace, task_id, seq, first_line, shipped, shipped_digest, lines, bytes, object_key)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			was.Namespace, was.Row, c.Seq, c.FirstLine, c.Shipped, c.ShippedDigest, c.Lines, c.Bytes, key); err != nil {
			return fmt.Errorf("db: chunk %d of the log of task %s could not be recorded: %w", c.Seq, was.Row, err)
		}
	}
	var final *int
	if now.FinalSeq != 0 {
		final = &now.FinalSeq
	}
	if _, err := w.tx.Exec(ctx, `
		update task_logs
		set next_seq = $3, shipped_lines = $4, lines = $5, bytes = $6, truncated = $7, final_seq = $8, updated_at = now()
		where namespace = $1 and task_id = $2`,
		was.Namespace, was.Row, now.NextSeq, now.ShippedLines, now.Lines, now.Bytes, now.Truncated, final); err != nil {
		return fmt.Errorf("db: the log of task %s could not be moved on: %w", was.Row, err)
	}
	uri, err := was.URI()
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	if _, err := w.tx.Exec(ctx,
		`update tasks set log_uri = $3 where namespace = $1 and id = $2 and log_uri is null`,
		was.Namespace, was.Row, uri.String()); err != nil {
		return fmt.Errorf("db: task %s could not be given the URI of its log: %w", was.Row, err)
	}
	return nil
}

// TaskLog reads one dispatch's log as a reader follows it: where it stands, and the chunks that
// hold lines, in order. A dispatch that has shipped nothing is ErrNoLog.
//
// It is what GET /api/v1/runs/{id}/steps/{step}/logs reads the history from, in the namespace its
// run was authorised in, before it follows what is shipped next.
func (n *NS) TaskLog(ctx context.Context, row string) (TaskLog, []LogChunk, error) {
	l := TaskLog{Namespace: n.namespace, Row: row}
	var final *int
	err := n.tx.QueryRow(ctx, `
		select t.idempotency_key, l.next_seq, l.shipped_lines, l.lines, l.bytes, l.truncated, l.final_seq
		from task_logs l join tasks t on t.namespace = l.namespace and t.id = l.task_id
		where l.namespace = $1 and l.task_id = $2`, n.namespace, row).
		Scan(&l.Task, &l.NextSeq, &l.ShippedLines, &l.Lines, &l.Bytes, &l.Truncated, &final)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskLog{}, nil, ErrNoLog
	}
	if err != nil {
		return TaskLog{}, nil, fmt.Errorf("db: the log of task %s could not be read: %w", row, err)
	}
	if final != nil {
		l.FinalSeq = *final
	}
	rows, err := n.tx.Query(ctx,
		`select `+chunkColumns+` from task_log_chunks
		 where namespace = $1 and task_id = $2 and object_key is not null
		 order by seq`, n.namespace, row)
	if err != nil {
		return TaskLog{}, nil, fmt.Errorf("db: the chunks of the log of task %s could not be read: %w", row, err)
	}
	chunks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LogChunk, error) { return scanChunk(r) })
	if err != nil {
		return TaskLog{}, nil, fmt.Errorf("db: the chunks of the log of task %s could not be read: %w", row, err)
	}
	return l, chunks, nil
}

// ErrNoLog is a dispatch nothing has shipped a log for.
var ErrNoLog = errors.New("db: nothing has shipped a log for that task")

const chunkColumns = `seq, first_line, shipped, shipped_digest, lines, bytes, coalesce(object_key, '')`

func scanChunk(row pgx.Row) (LogChunk, error) {
	var c LogChunk
	err := row.Scan(&c.Seq, &c.FirstLine, &c.Shipped, &c.ShippedDigest, &c.Lines, &c.Bytes, &c.Key)
	return c, err
}
