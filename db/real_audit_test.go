package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
)

// The audit log as PostgreSQL keeps it: chained by the database, appended to in the act's own
// transaction, and never changed or removed.

// auditLog is a database holding the namespace finance, and the application's pool on it.
func auditLog(t *testing.T) (*Pool, *pgx.Conn) {
	t.Helper()
	super, app := database(t)
	conn := connect(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, conn
}

// act appends one entry, in a namespace or on the installation where namespace is empty.
func act(ctx context.Context, pool *Pool, namespace string, r audit.Record) error {
	if namespace == "" {
		return pool.Installation(ctx, RunnerInventory, func(ctx context.Context, w *Wide) error { return w.Audit(ctx, r) })
	}
	return pool.In(ctx, namespace, func(ctx context.Context, ns *NS) error { return ns.Audit(ctx, r) })
}

func record(actor, action, target string) audit.Record {
	return audit.Record{Actor: actor, Action: action, Target: target, Result: audit.Done, Detail: map[string]any{"why": "a test"}}
}

// entries reads the whole log through the export's door.
func entries(t *testing.T, pool *Pool) []audit.Entry {
	t.Helper()
	got, err := pool.AuditTrail().After(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func verified(t *testing.T, pool *Pool) error {
	t.Helper()
	return pool.Installation(t.Context(), AuditLog, func(ctx context.Context, w *Wide) error { return w.VerifyAuditLog(ctx) })
}

// Three acts, in two namespaces and on the installation, make a chain of three that the Go spelling
// of the hash verifies: the database and package audit hash the same bytes.
func TestTheDatabaseChainsWhatPackageAuditVerifies(t *testing.T) {
	pool, _ := auditLog(t)
	for _, a := range []struct{ namespace, target string }{{"finance", "01JMZ8V1P9C4XQ7K2N4D6F8H0A"}, {"", "runner-a"}, {"team-ops", "billing"}} {
		if err := act(t.Context(), pool, a.namespace, record("operator", audit.RunCancel, a.target)); err != nil {
			t.Fatal(err)
		}
	}
	got := entries(t, pool)
	if len(got) != 3 {
		t.Fatalf("the log holds %d entries after three acts", len(got))
	}
	if err := audit.Verify(got); err != nil {
		t.Fatalf("the chain the database made does not verify: %s", err)
	}
	if err := verified(t, pool); err != nil {
		t.Fatalf("the log does not verify against its head: %s", err)
	}
	if got[0].Namespace != "finance" || got[1].Namespace != "" || got[2].Namespace != "team-ops" {
		t.Errorf("the entries are in namespaces %q, %q and %q", got[0].Namespace, got[1].Namespace, got[2].Namespace)
	}
	if !bytes.Equal(got[0].PrevHash, audit.Genesis) || got[2].Detail != `{"why":"a test"}` {
		t.Errorf("the first entry follows %x and the last carries %s", got[0].PrevHash, got[2].Detail)
	}
}

// Whatever a writer puts where the chain goes is replaced: no writer chooses its number, its moment
// or what it claims to follow.
func TestAWriterCannotChooseItsPlaceInTheChain(t *testing.T) {
	pool, _ := auditLog(t)
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.tx.Exec(ctx,
			`insert into audit_log (seq, at, actor, action, namespace, target, result, detail, prev_hash, hash)
			 values (99, '2001-01-01', 'mallory', 'run.cancel', 'finance', 'x', 'done', '{}', $1, $1)`,
			bytes.Repeat([]byte{7}, 32))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got := entries(t, pool)
	if len(got) != 1 || got[0].Seq != 1 || got[0].At.Year() == 2001 || !bytes.Equal(got[0].PrevHash, audit.Genesis) {
		t.Fatalf("a writer placed its own entry: %+v", got)
	}
	if err := verified(t, pool); err != nil {
		t.Fatal(err)
	}
}

// The application's role is granted no update and no delete on the log and no write on its head,
// and the table's own triggers refuse a change and a removal to the role that owns it.
func TestNoRoleChangesOrRemovesAnEntry(t *testing.T) {
	pool, super := auditLog(t)
	if err := act(t.Context(), pool, "finance", record("operator", audit.RunTrigger, "01JMZ8V1P9C4XQ7K2N4D6F8H0A")); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`update audit_log set actor = 'mallory'`,
		`delete from audit_log`,
		`update audit_head set seq = 0`,
	} {
		err := pool.Installation(t.Context(), AuditLog, func(ctx context.Context, w *Wide) error {
			_, err := w.tx.Exec(ctx, stmt)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("the application ran %q and was answered %v", stmt, err)
		}
	}
	for _, stmt := range []string{
		`update audit_log set actor = 'mallory'`,
		`delete from audit_log`,
		`truncate audit_log`,
		`delete from audit_head`,
		`update audit_head set seq = 0`,
		`update audit_head set seq = seq + 5`,
	} {
		if _, err := super.Exec(t.Context(), stmt); err == nil || !strings.Contains(err.Error(), "append") {
			t.Errorf("the owner ran %q and was answered %v", stmt, err)
		}
	}
	if err := verified(t, pool); err != nil {
		t.Fatal(err)
	}
}

// tamper runs stmt as a superuser with the triggers off, which is what it takes to change the log at
// all, and what the chain is there to show afterwards.
func tamper(t *testing.T, super *pgx.Conn, stmt string) {
	t.Helper()
	ctx := t.Context()
	tx, err := super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, s := range []string{`set local session_replication_role = replica`, stmt} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %s", s, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// An edit, a deletion in the middle and a deletion at the end each leave a break, found at the entry
// where it is.
func TestAnEditOrADeletionLeavesABreak(t *testing.T) {
	for _, c := range []struct {
		name, stmt string
		at         int64
	}{
		{"an actor edited", `update audit_log set actor = 'somebody else' where seq = 2`, 2},
		{"a detail edited", `update audit_log set detail = '{"why":"another"}' where seq = 3`, 3},
		{"an entry edited and hashed again", `update audit_log set actor = 'x', hash = audit_entry_hash(prev_hash, seq, at, 'x', action, namespace, target, result, detail) where seq = 2`, 3},
		{"an entry removed", `delete from audit_log where seq = 2`, 2},
		{"the last entry removed", `delete from audit_log where seq = 4`, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, super := auditLog(t)
			for i := range 4 {
				if err := act(t.Context(), pool, "finance", record("operator", audit.RunCancel, fmt.Sprintf("run-%d", i))); err != nil {
					t.Fatal(err)
				}
			}
			tamper(t, super, c.stmt)
			var broke *audit.Break
			if err := verified(t, pool); !errors.As(err, &broke) || broke.Seq != c.at {
				t.Fatalf("after %s the log verifies as %v, and it breaks at entry %d", c.name, err, c.at)
			}
		})
	}
}

// An act whose transaction rolls back leaves no entry and the head where it was, so the next act
// follows the last one that happened.
func TestAnActRolledBackIsNotRecorded(t *testing.T) {
	pool, _ := auditLog(t)
	refused := errors.New("refused")
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if err := ns.Audit(ctx, record("operator", audit.SecretWrite, "billing")); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if got := entries(t, pool); len(got) != 0 {
		t.Fatalf("an act rolled back left %d entries", len(got))
	}
	if err := act(t.Context(), pool, "finance", record("operator", audit.SecretWrite, "billing")); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, pool); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("the act after one rolled back is %+v", got)
	}
	if err := verified(t, pool); err != nil {
		t.Fatal(err)
	}
}

// Acts committing at once take turns at the head: every one of them is recorded, each after one
// other, and the chain has no fork and no gap.
func TestActsAtOnceNeverForkTheChain(t *testing.T) {
	pool, _ := auditLog(t)
	const acts = 40
	var wg sync.WaitGroup
	errs := make(chan error, acts)
	for i := range acts {
		wg.Go(func() {
			namespace := []string{"finance", "team-ops", ""}[i%3]
			errs <- act(t.Context(), pool, namespace, record(fmt.Sprintf("principal-%d", i), audit.RunTrigger, fmt.Sprintf("run-%d", i)))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("an act made at the same moment as others failed: %s", err)
		}
	}
	got := entries(t, pool)
	if len(got) != acts {
		t.Fatalf("%d acts made %d entries", acts, len(got))
	}
	if err := verified(t, pool); err != nil {
		t.Fatal(err)
	}
}

// The export's cursor only moves forward.
func TestTheExportCursorOnlyMovesForward(t *testing.T) {
	pool, _ := auditLog(t)
	trail := pool.AuditTrail()
	if seq, hash, err := trail.Exported(t.Context()); err != nil || seq != 0 || !bytes.Equal(hash, audit.Genesis) {
		t.Fatalf("a new log was exported through %d, %x: %v", seq, hash, err)
	}
	five, three := bytes.Repeat([]byte{5}, 32), bytes.Repeat([]byte{3}, 32)
	if err := trail.MarkExported(t.Context(), 5, five); err != nil {
		t.Fatal(err)
	}
	if err := trail.MarkExported(t.Context(), 3, three); err != nil {
		t.Fatal(err)
	}
	if seq, hash, err := trail.Exported(t.Context()); err != nil || seq != 5 || !bytes.Equal(hash, five) {
		t.Fatalf("the cursor went back to %d: %v", seq, err)
	}
	// Whoever writes it: the application's role may update the cursor, and still cannot take it
	// back or remove it.
	for _, stmt := range []string{`update audit_export set through = 1`, `delete from audit_export`} {
		err := pool.Installation(t.Context(), AuditLog, func(ctx context.Context, w *Wide) error {
			_, err := w.tx.Exec(ctx, stmt)
			return err
		})
		if err == nil {
			t.Errorf("the application ran %q", stmt)
		}
	}
	if seq, _, err := trail.Exported(t.Context()); err != nil || seq != 5 {
		t.Fatalf("the cursor is at %d: %v", seq, err)
	}
}

// acts appends n entries to the log, in the namespace finance.
func acts(t *testing.T, pool *Pool, n int) {
	t.Helper()
	for i := range n {
		if err := act(t.Context(), pool, "finance", record("operator", audit.RunCancel, fmt.Sprintf("run-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

// verifiedThrough is how far the record says the chain was verified.
func verifiedThrough(t *testing.T, super *pgx.Conn) int64 {
	t.Helper()
	var through int64
	if err := super.QueryRow(t.Context(), `select through from audit_verified`).Scan(&through); err != nil {
		t.Fatal(err)
	}
	return through
}

// A long log is verified once: the next verification carries on from the last entry the one before
// it reached, and reads nothing before it but that entry.
func TestAVerificationCarriesOnFromWhereTheLastStopped(t *testing.T) {
	pool, super := auditLog(t)
	trail := pool.AuditTrail()
	acts(t, pool, 25)
	if v, err := trail.Verify(t.Context(), 10); err != nil || v != (AuditVerification{From: 0, Through: 25}) {
		t.Fatalf("a new log of 25 entries verifies as %+v: %v", v, err)
	}
	if got := verifiedThrough(t, super); got != 25 {
		t.Fatalf("the record says the chain was verified through %d", got)
	}

	// An entry already verified, changed and keeping its hash, is not read again: that is what
	// bounds the cost, and the copy outside is what finds it.
	tamper(t, super, `update audit_log set actor = 'somebody else' where seq = 3`)
	acts(t, pool, 5)
	if v, err := trail.Verify(t.Context(), 10); err != nil || v != (AuditVerification{From: 25, Through: 30}) {
		t.Fatalf("five entries more verify as %+v: %v", v, err)
	}
	if v, err := trail.Verify(t.Context(), 10); err != nil || v != (AuditVerification{From: 30, Through: 30}) {
		t.Fatalf("nothing more verifies as %+v: %v", v, err)
	}
	if got := verifiedThrough(t, super); got != 30 {
		t.Fatalf("the record says the chain was verified through %d", got)
	}
}

// A break is answered at the entry where it is, and the record stays at the last batch that held,
// so that every verification after it finds the break again.
func TestABreakIsFoundAgainByEveryVerification(t *testing.T) {
	pool, super := auditLog(t)
	trail := pool.AuditTrail()
	acts(t, pool, 25)
	tamper(t, super, `update audit_log set detail = '{"why":"another"}' where seq = 15`)
	for _, from := range []int64{0, 10} {
		var broke *audit.Break
		v, err := trail.Verify(t.Context(), 10)
		if !errors.As(err, &broke) || broke.Seq != 15 || v.From != from || v.Through != 14 {
			t.Fatalf("a log changed at entry 15 verifies as %+v: %v", v, err)
		}
		if got := verifiedThrough(t, super); got != 10 {
			t.Fatalf("after a break at entry 15 the record says the chain was verified through %d", got)
		}
	}
}

// The record is the application's to write, so the entry it names is checked before anything is
// carried on from it, and where it does not hold the whole log is verified again and a break is
// answered.
func TestARecordTheChainDoesNotCheckIsNotTrusted(t *testing.T) {
	for _, c := range []struct {
		name string
		// app is run by the application's role and tampered by a superuser with the
		// triggers off, after the log of four entries was verified through its last.
		app, tampered []string
		at            int64
	}{
		{name: "a record moved forward onto a hash the entry does not carry", app: []string{`update audit_verified set through = 6, hash = decode(repeat('07', 32), 'hex')`}, at: 6},
		{name: "a record moved past the end of the log", app: []string{`update audit_verified set through = 99`}, at: 7},
		{name: "the entry recorded changed and keeping its hash", tampered: []string{`update audit_log set actor = 'somebody else' where seq = 4`}, at: 4},
		{name: "the entry recorded removed", tampered: []string{`delete from audit_log where seq = 4`}, at: 4},
		{
			name: "the chain written again from before the entry recorded, head and all",
			tampered: []string{
				`update audit_log set actor = 'somebody else', hash = audit_entry_hash(prev_hash, seq, at, 'somebody else', action, namespace, target, result, detail) where seq = 2`,
				`update audit_log l set prev_hash = p.hash from audit_log p where p.seq = 2 and l.seq = 3`,
				`update audit_log set hash = audit_entry_hash(prev_hash, seq, at, actor, action, namespace, target, result, detail) where seq = 3`,
				`update audit_log l set prev_hash = p.hash from audit_log p where p.seq = 3 and l.seq = 4`,
				`update audit_log set hash = audit_entry_hash(prev_hash, seq, at, actor, action, namespace, target, result, detail) where seq = 4`,
				`update audit_log l set prev_hash = p.hash from audit_log p where p.seq = 4 and l.seq = 5`,
				`update audit_log set hash = audit_entry_hash(prev_hash, seq, at, actor, action, namespace, target, result, detail) where seq = 5`,
				`update audit_log l set prev_hash = p.hash from audit_log p where p.seq = 5 and l.seq = 6`,
				`update audit_log set hash = audit_entry_hash(prev_hash, seq, at, actor, action, namespace, target, result, detail) where seq = 6`,
				`update audit_head set hash = (select hash from audit_log where seq = 6)`,
			},
			at: 4,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, super := auditLog(t)
			trail := pool.AuditTrail()
			acts(t, pool, 4)
			if _, err := trail.Verify(t.Context(), 0); err != nil {
				t.Fatal(err)
			}
			acts(t, pool, 2)
			for _, stmt := range c.app {
				if err := pool.Installation(t.Context(), AuditLog, func(ctx context.Context, w *Wide) error {
					_, err := w.tx.Exec(ctx, stmt)
					return err
				}); err != nil {
					t.Fatalf("the application could not run %q: %s", stmt, err)
				}
			}
			for _, stmt := range c.tampered {
				tamper(t, super, stmt)
			}
			record := verifiedThrough(t, super)
			var broke *audit.Break
			if v, err := trail.Verify(t.Context(), 0); !errors.As(err, &broke) || broke.Seq != c.at || v.From != 0 {
				t.Fatalf("after %s the log verifies as %+v: %v, and it breaks at entry %d", c.name, v, err, c.at)
			}
			if got := verifiedThrough(t, super); got != record {
				t.Fatalf("a verification that did not trust the record moved it from %d to %d", record, got)
			}
		})
	}
}

// A verification reads the head before the entries, so acts committing while it reads are left to
// the next one rather than taken for entries removed from the end.
func TestActsDuringAVerificationAreNotABreak(t *testing.T) {
	pool, _ := auditLog(t)
	acts(t, pool, 1)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ctx.Err() == nil; i++ {
			act(ctx, pool, "finance", record("operator", audit.RunTrigger, fmt.Sprintf("run-%d", i)))
		}
	})
	for range 200 {
		if err := verified(t, pool); err != nil {
			stop()
			wg.Wait()
			t.Fatalf("a log appended to while it was verified verifies as %v", err)
		}
	}
	stop()
	wg.Wait()
}

// The record of how far the chain was verified only moves forward, and is never removed, whoever
// writes it.
func TestTheVerificationRecordOnlyMovesForward(t *testing.T) {
	pool, super := auditLog(t)
	acts(t, pool, 3)
	if _, err := pool.AuditTrail().Verify(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`update audit_verified set through = 1`, `delete from audit_verified`} {
		err := pool.Installation(t.Context(), AuditLog, func(ctx context.Context, w *Wide) error {
			_, err := w.tx.Exec(ctx, stmt)
			return err
		})
		if err == nil {
			t.Errorf("the application ran %q", stmt)
		}
		if _, err := super.Exec(t.Context(), stmt); err == nil {
			t.Errorf("the owner ran %q", stmt)
		}
	}
	if _, err := super.Exec(t.Context(), `truncate audit_verified`); err == nil {
		t.Error("the owner emptied the record")
	}
	if got := verifiedThrough(t, super); got != 3 {
		t.Fatalf("the record is at %d", got)
	}
}
