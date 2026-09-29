package db

import (
	"context"
	"errors"
	"testing"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// A cache entry is found under its key in its own namespace and nowhere else, is a hit only while
// everything it names is there, and goes with its run's envelopes: "a cache entry pointing at an
// expired artifact is not a hit", and "it never crosses a namespace boundary".
func TestACacheEntryIsAHitOnlyWhileWhatItNamesIsThere(t *testing.T) {
	pool, super := opened(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	envelope := "sha256:" + digestOf("e")
	exec(`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs) values ('finance', $1, 64, 'application/json', 1)`, envelope)
	exec(`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs) values ('finance', $1, 4096, 'application/pdf', 1)`, "sha256:"+digestOf("f"))
	exec(`insert into artifacts (namespace, run_id, step, port, name, digest, size_bytes, media_type, expires_at)
	      values ('finance', $1, 'render', 'ok', 'invoice.pdf', $2, 4096, 'application/pdf', now() + interval '1 day')`, financeRun, "sha256:"+digestOf("f"))

	const key = "finance/sha256/" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	m := Memo{Key: key, Run: financeRun, Step: "render", Ports: []MemoPort{{Port: "ok", Digest: envelope, Items: 1, Size: 64}}}
	file := agk.URI{Run: financeRun, Step: "render", Port: "ok", Name: "invoice.pdf"}
	alive := func() bool {
		t.Helper()
		var ok bool
		if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			var err error
			ok, err = w.MemoAlive(ctx, "finance", m, []agk.URI{file})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		if err := w.Memoise(ctx, "finance", m); err != nil {
			return err
		}
		got, err := w.Memo(ctx, "finance", key)
		if err != nil || got.Run != financeRun || got.Step != "render" || len(got.Ports) != 1 || got.Ports[0] != m.Ports[0] {
			t.Errorf("the entry reads back as %+v: %v", got, err)
		}
		if _, err := w.Memo(ctx, "team-ops", key); !errors.Is(err, ErrNoMemo) {
			t.Errorf("another namespace finds the entry: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !alive() {
		t.Fatal("an entry whose envelope and file are there is no hit")
	}

	for name, c := range map[string]struct{ break_, mend string }{
		"an envelope being collected": {`update artifact_objects set collecting_at = now() where digest = '` + envelope + `'`, `update artifact_objects set collecting_at = null where digest = '` + envelope + `'`},
		"an envelope nothing counts":  {`update artifact_objects set refs = 0 where digest = '` + envelope + `'`, `update artifact_objects set refs = 1 where digest = '` + envelope + `'`},
		"a file expired":              {`update artifacts set expires_at = now() - interval '1 second'`, `update artifacts set expires_at = now() + interval '1 day'`},
		"a file with a fetch budget":  {`update artifacts set fetches_left = 1`, `update artifacts set fetches_left = null`},
		"a file collected":            {`update artifacts set status = 'collected', retired_at = now()`, `update artifacts set status = 'live', retired_at = null`},
		"its run's envelopes purged":  {`update runs set envelopes_purged_at = now() where id = '` + financeRun + `'`, `update runs set envelopes_purged_at = null where id = '` + financeRun + `'`},
	} {
		exec(c.break_)
		if alive() {
			t.Errorf("%s is still a hit", name)
		}
		exec(c.mend)
	}
	if !alive() {
		t.Fatal("the entry mended is no hit")
	}

	// The purge of the run's envelopes takes its entries with it.
	exec(`update runs set started_at = now(), finished_at = now(), expires_at = now() - interval '1 minute' where id = $1`, financeRun)
	if _, err := pool.PurgeEnvelopes(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := conn.QueryRow(ctx, `select count(*) from step_cache`).Scan(&left); err != nil || left != 0 {
		t.Errorf("the purge left %d entries: %v", left, err)
	}
}
