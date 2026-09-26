package db

import (
	"bytes"
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

// auditBatch is how many entries a verification reads at a time.
const auditBatch = 1000

// VerifyAuditLog checks the whole audit log from its first entry, and answers the first
// *audit.Break.
//
// Beside each entry following the one before it, the last entry has to be the head of the chain,
// which an append moves and nothing else can: an entry removed from the end leaves a chain that
// holds up to where it stops, and a head that is further on.
func (w *Wide) VerifyAuditLog(ctx context.Context) error {
	seq, hash, err := w.AuditHead(ctx)
	if err != nil {
		return err
	}
	var chain audit.Chain
	return verifyChain(ctx, &chain, seq, hash, auditBatch, w.AuditEntries, nil)
}

// verifyChain carries chain on through entry head, whose hash is hash, reading batch entries at a
// time with read, and tells progress, where it is given, how far the chain is proved to hold after
// each batch.
//
// The head is read before the entries rather than after them. An append commits its entry and the
// head it moved together, so every entry up to a head already read is there to be read, and an act
// committing in the middle of the verification adds entries past it, which are left to the next
// one. Read after the entries, the head of an act that committed in between would be taken for
// entries removed from the end.
//
// An entry is proved only once something that follows it carries its hash: the next entry, or the
// head for the last. The last entry of a batch is not proved yet, since an entry changed and hashed
// again holds on its own and breaks only at what follows it, so progress is told the entry before
// it, and the head once the head has been checked.
func verifyChain(ctx context.Context, chain *audit.Chain, head int64, hash []byte, batch int,
	read func(ctx context.Context, seq int64, limit int) ([]audit.Entry, error),
	progress func(ctx context.Context, seq int64, hash []byte) error) error {
	proved, provedHash := chain.Last()
	told := proved
	tell := func(seq int64, hash []byte) error {
		if progress == nil || seq <= told {
			return nil
		}
		told = seq
		return progress(ctx, seq, hash)
	}
	for {
		last, _ := chain.Last()
		if last >= head {
			break
		}
		entries, err := read(ctx, last, int(min(int64(batch), head-last)))
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			before, beforeHash := chain.Last()
			if err := chain.Next(e); err != nil {
				// What held before the break is kept, so the next verification reads no
				// more than it has to to find the break again.
				if err := tell(proved, provedHash); err != nil {
					return err
				}
				return err
			}
			proved, provedHash = before, beforeHash
		}
		if err := tell(proved, provedHash); err != nil {
			return err
		}
	}
	last, lastHash := chain.Last()
	switch {
	case last < head:
		return &audit.Break{Seq: last + 1, Why: fmt.Sprintf("the log has been given %d entries and holds %d, so the last were removed", head, last)}
	case !bytes.Equal(lastHash, hash):
		return &audit.Break{Seq: last, Why: "the last entry is not the one the head of the chain follows"}
	}
	return tell(last, lastHash)
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

// AuditVerification is how far one verification of the chain in the database went: it carried on
// after entry From, 0 where it read the log from its first, and found the chain holding through
// entry Through.
type AuditVerification struct {
	From    int64
	Through int64
}

// Verify checks the chain in the database from the last entry a verification reached, up to the
// head as it stands when it begins, and answers the first *audit.Break. It records how far the
// chain holds after each batch of at most batch entries, auditBatch where batch is not positive,
// so that the next verification, by this controller or the one after a failover, carries on from
// there, and one cut short keeps what it read.
//
// The record is the application's to write, and moves only forward, so it is never taken on
// trust: the entry it names has to carry the hash recorded beside it, and fields that give that
// hash, before the chain is carried on from it. Where it does not, the whole log is verified
// again and the first break found is answered, leaving the record where it was; a chain that holds
// from its first entry all the same is an *AuditRecordDisagrees, and the record is moved forward
// to the head it verified.
//
// An entry before the record that is changed and keeps its hash is not read again, which is what
// bounds the cost on a long log, and is found by comparing with the copy outside the installation.
func (a AuditTrail) Verify(ctx context.Context, batch int) (AuditVerification, error) {
	if batch <= 0 {
		batch = auditBatch
	}
	var head, from int64
	var headHash, fromHash []byte
	var recorded []audit.Entry
	err := a.pool.Installation(ctx, AuditLog, func(ctx context.Context, w *Wide) error {
		// The record before the head: a verification records no further than the head it read,
		// which is never past a head read after it, so one that finishes in between cannot
		// leave a record that reads as past the end of the log.
		if err := w.tx.QueryRow(ctx, `select through, hash from audit_verified`).Scan(&from, &fromHash); err != nil {
			return fmt.Errorf("db: how far the audit log was verified could not be read: %w", err)
		}
		var err error
		if head, headHash, err = w.AuditHead(ctx); err != nil {
			return err
		}
		if from > 0 && from <= head {
			recorded, err = w.AuditEntries(ctx, from-1, 1)
		}
		return err
	})
	if err != nil {
		return AuditVerification{}, err
	}

	holds := from == 0 || (len(recorded) == 1 && recorded[0].Seq == from &&
		bytes.Equal(recorded[0].Hash, fromHash) && bytes.Equal(recorded[0].Hash, recorded[0].Sum()))
	if holds {
		chain := audit.From(from, fromHash)
		if from == 0 {
			chain = audit.From(0, audit.Genesis)
		}
		err := verifyChain(ctx, chain, head, headHash, batch, a.After, a.markVerified)
		through, _ := chain.Last()
		return AuditVerification{From: from, Through: through}, err
	}

	chain := audit.From(0, audit.Genesis)
	err = verifyChain(ctx, chain, head, headHash, batch, a.After, nil)
	through, lastHash := chain.Last()
	v := AuditVerification{Through: through}
	if err != nil {
		return v, err
	}
	// The chain holds from its first entry, and the record does not agree with it. Moved forward
	// to the head, so that the next verification carries on from there rather than reading the
	// whole log at every term with nothing able to take the record back.
	if err := a.markVerified(ctx, through, lastHash); err != nil {
		return v, err
	}
	disagrees := &AuditRecordDisagrees{Seq: from, Why: "it does not carry the hash it had when it was verified"}
	if from > head {
		disagrees = &AuditRecordDisagrees{Seq: from, Why: fmt.Sprintf("the log ends at entry %d", head)}
	}
	return v, disagrees
}

// AuditRecordDisagrees is a chain that holds from its first entry while the entry the last
// verification recorded reaching does not agree with the record: it has another hash, or the log
// no longer reaches it.
//
// Only the record shows it, and the record alone cannot tell which it is. Either the chain was
// written again from that entry or before it, head and all, which is what the record is there to
// catch, or the record was written by something other than a verification, which the
// application's role can do. Both want the log compared with its copy outside the installation.
type AuditRecordDisagrees struct {
	Seq int64
	Why string
}

func (d *AuditRecordDisagrees) Error() string {
	return fmt.Sprintf("db: the audit log's chain holds from its first entry, and entry %d, which the last verification reached, does not agree with its record: %s", d.Seq, d.Why)
}

// markVerified records that the chain holds through entry seq, whose hash is hash. Like the
// export's cursor it only moves forward, so a controller that lost the lead and records after the
// next one went further leaves the record where the further one put it.
func (a AuditTrail) markVerified(ctx context.Context, seq int64, hash []byte) error {
	err := a.pool.Installation(ctx, AuditLog, func(ctx context.Context, w *Wide) error {
		_, err := w.tx.Exec(ctx, `update audit_verified set through = $1, hash = $2 where through < $1`, seq, hash)
		return err
	})
	if err != nil {
		return fmt.Errorf("db: how far the audit log was verified could not be recorded: %w", err)
	}
	return nil
}
