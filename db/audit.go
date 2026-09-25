package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
)

// The audit log, appended to in the transaction of the act it records and read across the
// installation by the export.
//
// An append is the last statement of its transaction, wherever it is called from. It takes the
// head of the chain under a row lock that lasts until the transaction ends, so that appends take
// turns; a transaction that went on to lock anything else after it would hold the head while it
// waited, and every act of the installation would wait behind it.

// Audit appends r to the audit log, in this namespace, in this handle's transaction: the entry
// commits with the act and is rolled back with it.
func (n *NS) Audit(ctx context.Context, r audit.Record) error {
	return appendAudit(ctx, n.tx, n.namespace, r)
}

// Audit appends r to the audit log as an act on the installation, in this handle's transaction.
func (w *Wide) Audit(ctx context.Context, r audit.Record) error {
	return appendAudit(ctx, w.tx, "", r)
}

func appendAudit(ctx context.Context, tx pgx.Tx, namespace string, r audit.Record) error {
	if err := r.Check(); err != nil {
		return err
	}
	detail, err := r.DetailText()
	if err != nil {
		return err
	}
	// The number, the moment and both hashes are the database's to give, and are left out.
	if _, err := tx.Exec(ctx,
		`insert into audit_log (actor, action, namespace, target, result, detail)
		 values ($1, $2, nullif($3, ''), $4, $5, $6)`,
		r.Actor, r.Action, namespace, r.Target, r.Result, detail); err != nil {
		return fmt.Errorf("db: the %s could not be recorded in the audit log, and the act is not done either: %w", r.Action, err)
	}
	return nil
}

// AuditEntries answers at most limit entries of the audit log after entry seq, in order.
func (w *Wide) AuditEntries(ctx context.Context, seq int64, limit int) ([]audit.Entry, error) {
	rows, err := w.tx.Query(ctx,
		`select seq, at, actor, action, coalesce(namespace, ''), target, result, detail, prev_hash, hash
		   from audit_log where seq > $1 order by seq limit $2`, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("db: the audit log could not be read: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (audit.Entry, error) {
		var e audit.Entry
		err := row.Scan(&e.Seq, &e.At, &e.Actor, &e.Action, &e.Namespace, &e.Target, &e.Result, &e.Detail, &e.PrevHash, &e.Hash)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("db: the audit log could not be read: %w", err)
	}
	return entries, nil
}

// AuditHead is how many entries the audit log has been given and the hash of the last.
func (w *Wide) AuditHead(ctx context.Context) (int64, []byte, error) {
	var seq int64
	var hash []byte
	if err := w.tx.QueryRow(ctx, `select seq, hash from audit_head`).Scan(&seq, &hash); err != nil {
		return 0, nil, fmt.Errorf("db: the head of the audit log could not be read: %w", err)
	}
	return seq, hash, nil
}

// auditBatch is how many entries VerifyAuditLog reads at a time.
const auditBatch = 1000

// VerifyAuditLog checks the whole audit log from its first entry, and answers the first
// *audit.Break.
//
// Beside each entry following the one before it, the last entry has to be the head of the chain,
// which an append moves and nothing else can: an entry removed from the end leaves a chain that
// holds up to where it stops, and a head that is further on.
func (w *Wide) VerifyAuditLog(ctx context.Context) error {
	var chain audit.Chain
	for {
		last, _ := chain.Last()
		entries, err := w.AuditEntries(ctx, last, auditBatch)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := chain.Next(e); err != nil {
				return err
			}
		}
		if len(entries) < auditBatch {
			break
		}
	}
	seq, hash, err := w.AuditHead(ctx)
	if err != nil {
		return err
	}
	last, lastHash := chain.Last()
	switch {
	case last != seq:
		return &audit.Break{Seq: last + 1, Why: fmt.Sprintf("the log has been given %d entries and holds %d, so the last were removed", seq, last)}
	case string(lastHash) != string(hash):
		return &audit.Break{Seq: last, Why: "the last entry is not the one the head of the chain follows"}
	}
	return nil
}

// AuditTrail is the audit log as the export reads it, across the installation.
type AuditTrail struct{ pool *Pool }

// AuditTrail is this pool's audit log, for the export.
func (p *Pool) AuditTrail() AuditTrail { return AuditTrail{pool: p} }

// After answers at most limit entries after entry seq, in order.
func (a AuditTrail) After(ctx context.Context, seq int64, limit int) ([]audit.Entry, error) {
	var entries []audit.Entry
	err := a.pool.Installation(ctx, AuditLog, func(ctx context.Context, w *Wide) error {
		var err error
		entries, err = w.AuditEntries(ctx, seq, limit)
		return err
	})
	return entries, err
}

// Exported is the last entry the sink accepted, and its hash: 0 and audit.Genesis before any.
func (a AuditTrail) Exported(ctx context.Context) (int64, []byte, error) {
	var seq int64
	var hash []byte
	err := a.pool.Installation(ctx, AuditLog, func(ctx context.Context, w *Wide) error {
		err := w.tx.QueryRow(ctx, `select through, hash from audit_export`).Scan(&seq, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("db: the audit log's export has no cursor, which its migration creates")
		}
		return err
	})
	if err != nil {
		return 0, nil, fmt.Errorf("db: how far the audit log was exported could not be read: %w", err)
	}
	return seq, hash, nil
}

// MarkExported records that the sink accepted every entry up to seq, whose hash is hash. It only
// moves forward: a controller that lost the lead in the middle of a batch and marks it after the
// next leader went further leaves the cursor where the further one put it.
func (a AuditTrail) MarkExported(ctx context.Context, seq int64, hash []byte) error {
	err := a.pool.Installation(ctx, AuditLog, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `update audit_export set through = $1, hash = $2 where through < $1`, seq, hash)
		return err
	})
	if err != nil {
		return fmt.Errorf("db: how far the audit log was exported could not be recorded: %w", err)
	}
	return nil
}
