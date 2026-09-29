package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A workflow deleted is absent from the answer on: its rows answer as none, its name is refused a
// new workflow, and it cannot be deleted twice. Its run still going is asked to cancel, and the data
// of its runs expires at once; the controller's purge then deletes the runs the purges are done
// with, the versions once no run is left, letting go of what their trees name, hands its packs to the
// collection, and deletes the workflow once nothing names it, which frees its name. The other
// namespace's workflow is left as it was.
func TestADeletedWorkflowIsAbsentAtOnceAndPurgedInTurn(t *testing.T) {
	pool, super := opened(t)
	conn := superuser(t, super)
	tree := digestOf("d")
	for _, stmt := range []string{
		`insert into artifact_objects (namespace, digest, size_bytes, media_type, refs) values ('finance', 'sha256:` + tree + `', 12, 'application/octet-stream', 1)`,
		`update workflow_versions set tree = '[{"path":"agentiik.yaml","sha256":"` + tree + `","size":12,"mode":"0644"}]' where namespace = 'finance'`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	in := func(fn func(ctx context.Context, n *NS) error) error { return pool.In(t.Context(), "finance", fn) }
	if err := in(func(ctx context.Context, n *NS) error {
		_, err := n.WriteArtifact(ctx, Reference{URI: uri(financeRun, "archive", "out", "invoices.zip"), Digest: digestOf("a"), Size: 4096, For: 90 * 24 * time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var cancelled []string
	if err := in(func(ctx context.Context, n *NS) error {
		runs, err := n.DeleteWorkflow(ctx, "monthly-invoicing", "carol", time.Now().UTC())
		for _, r := range runs {
			cancelled = append(cancelled, string(r))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 1 || cancelled[0] != financeRun {
		t.Errorf("the deletion asked to cancel %v", cancelled)
	}
	var asked, expired bool
	if err := conn.QueryRow(t.Context(), `select cancel_requested_at is not null from runs where id = $1`, financeRun).Scan(&asked); err != nil || !asked {
		t.Errorf("the run going was not asked to cancel: %v", err)
	}
	if err := conn.QueryRow(t.Context(), `select expires_at <= now() from artifacts where run_id = $1`, financeRun).Scan(&expired); err != nil || !expired {
		t.Errorf("the run's artifact does not expire at the deletion: %v", err)
	}

	err := in(func(ctx context.Context, n *NS) error {
		gone, err := n.Deleted(ctx, "monthly-invoicing")
		if err != nil || !gone {
			t.Errorf("the workflow deleted reads as deleted %t, %v", gone, err)
		}
		if _, err := n.WorkflowRecord(ctx, "monthly-invoicing"); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("the workflow deleted reads as %v", err)
		}
		if err := n.HoldRepository(ctx, "monthly-invoicing"); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("the repository of the workflow deleted is held: %v", err)
		}
		if _, err := n.Repository(ctx, "monthly-invoicing"); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("the repository of the workflow deleted reads as %v", err)
		}
		if err := n.SaveWorkflow(ctx, "monthly-invoicing", "main"); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("a tree push to the workflow deleted answered %v", err)
		}
		if _, err := n.CreateWorkflow(ctx, "monthly-invoicing", "main", false, "carol", time.Time{}); !errors.Is(err, ErrWorkflowPurging) {
			t.Errorf("a workflow created under the name answered %v", err)
		}
		if _, err := n.DeleteWorkflow(ctx, "monthly-invoicing", "carol", time.Now()); !errors.Is(err, ErrNoWorkflow) {
			t.Errorf("deleting it again answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The controller ends the run as cancelled, with the expiry its retention gives, and a push had
	// left a pack.
	for _, stmt := range []string{
		`update runs set state = 'cancelled', started_at = now(), finished_at = now(), expires_at = now() + interval '30 days' where id = '` + financeRun + `'`,
		`insert into git_packs (namespace, repository, name, size, objects, state) select namespace, repository, '` + c1 + `', 1, 1, 'live' from workflows where name = 'monthly-invoicing'`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	purged, _, err := pool.PurgeDeleted(t.Context(), 0)
	if err != nil || purged.Runs != 0 || purged.Versions != 0 || purged.Workflows != 0 || purged.Packs != 1 {
		t.Fatalf("the first purge removed %+v: %v", purged, err)
	}
	if err := conn.QueryRow(t.Context(), `select expires_at <= now() from runs where id = $1`, financeRun).Scan(&expired); err != nil || !expired {
		t.Errorf("the run ended since the deletion does not expire: %v", err)
	}
	var state string
	if err := conn.QueryRow(t.Context(), `select state from git_packs where name = $1`, c1).Scan(&state); err != nil || state != "superseded" {
		t.Errorf("the workflow's pack is %q: %v", state, err)
	}

	// The purges let go of the run's data, and the collection of the pack.
	if _, err := pool.ExpireArtifacts(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.PurgeEnvelopes(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `update tasks set log_uri = null where run_id = $1`, financeRun); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.LogsGone(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	purged, _, err = pool.PurgeDeleted(t.Context(), 0)
	if err != nil || purged.Runs != 1 || purged.Versions != 1 || purged.Workflows != 0 {
		t.Fatalf("the purge after the others removed %+v: %v", purged, err)
	}
	if got := refsOf(t, pool, "finance", tree); got != 0 {
		t.Errorf("the object the version's tree named is counted %d times", got)
	}
	if _, err := conn.Exec(t.Context(), `delete from git_packs where name = $1`, c1); err != nil {
		t.Fatal(err)
	}
	purged, _, err = pool.PurgeDeleted(t.Context(), 0)
	if err != nil || purged.Workflows != 1 {
		t.Fatalf("the purge once nothing names the workflow removed %+v: %v", purged, err)
	}
	if err := in(func(ctx context.Context, n *NS) error {
		_, err := n.CreateWorkflow(ctx, "monthly-invoicing", "main", false, "carol", time.Time{})
		return err
	}); err != nil {
		t.Errorf("the name purged is refused a new workflow: %v", err)
	}
	var others int
	if err := conn.QueryRow(t.Context(), `select count(*) from runs where namespace = 'team-ops'`).Scan(&others); err != nil || others != 1 {
		t.Errorf("the other namespace holds %d runs: %v", others, err)
	}
}
