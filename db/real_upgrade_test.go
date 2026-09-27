package db

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/jackc/pgx/v5"
)

// An upgrade is applied by init at docker compose up, on the state the release before left: "a
// compose.yaml of release N+1 starts on the state release N left", with no step by hand. So what
// is tested here is Provision, which is init's migrate, run on a database v0.2.5 migrated and
// filled, and then the application reading and writing what it held as v0.2.5 did.

// v025 is the last migration v0.2.5 carried.
const v025 = "0031_audit_verified.sql"

// migratedAt answers the superuser's address on a database of this test's own migrated as far as
// last and no further, and the name of the application's role, which nothing has created yet.
func migratedAt(t *testing.T, last string) (super, role string) {
	t.Helper()
	super, role = blank(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := MigrateThrough(t.Context(), conn, last); err != nil {
		t.Fatalf("the database could not be migrated as far as %s: %s", last, err)
	}
	return super, role
}

func TestAnInstallationOfV025UpgradesWithEverythingItHeld(t *testing.T) {
	super, role := migratedAt(t, v025)
	ctx := t.Context()

	const run = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// What a v0.2.5 installation holds after a few days: the namespace init created and one the
	// operator added with quotas of its own, a workflow pushed and run by the operator token, a
	// pool and a secret the operator made, and their audit entries, every one naming operator.
	for _, stmt := range []string{
		`insert into namespaces (name) values ('default')`,
		`insert into namespaces (name, max_retention_days, max_concurrent_tasks) values ('finance', 30, 7)`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance', 'monthly-invoicing', 'a3f9c1e', '{}', 'operator', now())`,
		`insert into runs (namespace, id, workflow, commit, state, trigger, triggered_by)
		   values ('finance', '` + run + `', 'monthly-invoicing', 'a3f9c1e', 'succeeded', 'manual', 'operator')`,
		`insert into steps (namespace, run_id, step, state) values ('finance', '` + run + `', 'invoice', 'succeeded')`,
		`insert into runner_pools (name, labels, accepted_namespaces, created_by)
		   values ('dmz', '{zone=dmz}', '{finance}', 'operator')`,
		`insert into secret_declarations (namespace, name, provider, declared_by)
		   values ('finance', 'billing', 'builtin', 'operator')`,
		`insert into audit_log (actor, action, namespace, target, result, detail)
		   values ('operator', 'namespace.create', null, 'finance', 'done', '{}'),
		          ('operator', 'run.trigger', 'finance', '` + run + `', 'done', '{}')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("filling the database as v0.2.5 would have: %s", err)
		}
	}

	ran, err := Provision(ctx, conn, role, "test")
	if err != nil {
		t.Fatalf("the upgrade was refused on a database v0.2.5 filled: %s", err)
	}
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var after []string
	for _, m := range all {
		if m.Name > v025 {
			after = append(after, m.Name)
		}
	}
	if !slices.Equal(ran, after) {
		t.Fatalf("the upgrade applied %v, and what v0.2.5 lacked is %v", ran, after)
	}

	pool, err := Open(ctx, withCredentials(super, role, "test"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// The namespaces are as they were, shared and owned by nobody yet, with no bound the upgrade
	// added: a workflow that ran under v0.2.5 is refused nothing.
	type namespaceRow struct {
		kind                  string
		owner                 *string
		retention, concurrent int
		perHour               *int
		artifactBytes         *int64
		duration              *string
		pools                 []string
	}
	read := func(name string) namespaceRow {
		var n namespaceRow
		err := pool.Installation(ctx, NamespaceAdministration, func(ctx context.Context, w *Wide) error {
			return w.tx.QueryRow(ctx,
				`select kind, owner, max_retention_days, max_concurrent_tasks, max_runs_per_hour,
				        max_artifact_bytes, max_run_duration, allowed_runner_pools
				   from namespaces where name = $1`, name).Scan(&n.kind, &n.owner, &n.retention,
				&n.concurrent, &n.perHour, &n.artifactBytes, &n.duration, &n.pools)
		})
		if err != nil {
			t.Fatalf("namespace %s could not be read after the upgrade: %s", name, err)
		}
		return n
	}
	for name, want := range map[string][2]int{"default": {90, 20}, "finance": {30, 7}} {
		n := read(name)
		switch {
		case n.kind != "shared" || n.owner != nil:
			t.Errorf("namespace %s is %s and owned by %v after the upgrade, and v0.2.5's namespaces are shared ones nobody owns yet", name, n.kind, n.owner)
		case n.retention != want[0] || n.concurrent != want[1]:
			t.Errorf("namespace %s keeps %d days and %d tasks after the upgrade, and held %d and %d", name, n.retention, n.concurrent, want[0], want[1])
		case n.perHour != nil || n.artifactBytes != nil || n.duration != nil || n.pools != nil:
			t.Errorf("namespace %s was given bounds by the upgrade: %v runs an hour, %v bytes, %v per run, pools %v", name, n.perHour, n.artifactBytes, n.duration, n.pools)
		}
	}

	// The run, its step and who triggered it, as the API reads them.
	err = pool.In(ctx, "finance", func(ctx context.Context, n *NS) error {
		d, err := n.RunDetail(ctx, run)
		if err != nil {
			return err
		}
		if d.State != agk.Succeeded || d.TriggeredBy != "operator" || len(d.Steps) != 1 || d.Steps[0].Step != "invoice" {
			t.Errorf("the run reads as %+v after the upgrade", d)
		}
		dec, err := n.Declaration(ctx, "billing")
		if err != nil {
			return err
		}
		if dec.DeclaredBy != "operator" {
			t.Errorf("the secret reads as declared by %q after the upgrade", dec.DeclaredBy)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the run could not be read after the upgrade: %s", err)
	}

	// The pools, the one the operator made and the one every installation has.
	err = pool.Installation(ctx, RunnerInventory, func(ctx context.Context, w *Wide) error {
		dmz, err := w.RunnerPoolNamed(ctx, "dmz")
		if err != nil {
			return err
		}
		if !slices.Equal(dmz.Labels, []string{"zone=dmz"}) || !dmz.Accepts("finance") || dmz.Accepts("default") || dmz.CreatedBy != "operator" {
			t.Errorf("the pool dmz reads as %+v after the upgrade", dmz)
		}
		_, err = w.RunnerPoolNamed(ctx, "default")
		return err
	})
	if err != nil {
		t.Fatalf("the pools could not be read after the upgrade: %s", err)
	}

	// And everything v0.2.5 did goes on: the operator starts a run, init creates a namespace, and
	// both are audited on the chain the upgrade found, which still verifies.
	started := agk.NewRunID()
	err = pool.In(ctx, "finance", func(ctx context.Context, n *NS) error {
		if err := n.CreateRun(ctx, NewRun{ID: started, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: agk.TriggerManual, TriggeredBy: "operator", Steps: []agk.Step{"invoice"}}); err != nil {
			return err
		}
		return n.Audit(ctx, audit.Record{Actor: "operator", Action: audit.RunTrigger, Target: string(started), Result: audit.Done})
	})
	if err != nil {
		t.Fatalf("the operator could not start a run after the upgrade: %s", err)
	}
	err = pool.Installation(ctx, NamespaceAdministration, func(ctx context.Context, w *Wide) error {
		if _, err := w.CreateNamespace(ctx, "team-ops"); err != nil {
			return err
		}
		return w.Audit(ctx, audit.Record{Actor: "operator", Action: audit.NamespaceCreate, Target: "team-ops", Result: audit.Done})
	})
	if err != nil {
		t.Fatalf("a namespace could not be created after the upgrade: %s", err)
	}
	if n := read("team-ops"); n.kind != "shared" || n.owner != nil || n.retention != 90 || n.concurrent != 20 {
		t.Errorf("a namespace created as v0.2.5's init creates one reads as %+v", n)
	}
	err = pool.Installation(ctx, AuditLog, func(ctx context.Context, w *Wide) error {
		return w.VerifyAuditLog(ctx)
	})
	if err != nil {
		t.Errorf("the audit log's chain does not verify across the upgrade: %s", err)
	}

	// What v0.3.0 starts from: the policy at its documented defaults and a bootstrap token that
	// init has not written yet and that has not ended.
	var hash []byte
	var enrolled *time.Time
	var password, passkey string
	var minPasskeys int
	err = pool.Installation(ctx, Identity, func(ctx context.Context, w *Wide) error {
		if err := w.tx.QueryRow(ctx, `select token_hash, enrolled_at from bootstrap`).Scan(&hash, &enrolled); err != nil {
			return err
		}
		return w.tx.QueryRow(ctx,
			`select password, passkey, min_passkeys from auth_policy where namespace is null`).Scan(&password, &passkey, &minPasskeys)
	})
	switch {
	case err != nil:
		t.Fatalf("the bootstrap state and the policy could not be read after the upgrade: %s", err)
	case hash != nil || enrolled != nil:
		t.Errorf("the bootstrap state after the upgrade holds %x, ended at %v", hash, enrolled)
	case password != "allowed" || passkey != "required" || minPasskeys != 2:
		t.Errorf("the installation's policy starts as password %s, passkey %s, %d passkeys", password, passkey, minPasskeys)
	}
}
