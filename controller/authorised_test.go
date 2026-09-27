package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/jackc/pgx/v5"
)

// "Authorisation is re-evaluated when a run is created, because a trigger armed months ago can fire
// long after the grant that armed it. From v0.3.0, a run whose principal no longer holds
// workflow:run at that moment ends cancelled before any task, with a reason naming the grant that
// lapsed."

// runBy creates decidedRun as trigger started it, attributed to by, as the API or a trigger writes
// it: by is left empty for a run nobody asked for, which is its namespace's built-in identity's.
func runBy(t *testing.T, pool *db.Pool, run agk.RunID, trigger agk.TriggerKind, by string) {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		return ns.CreateRun(ctx, db.NewRun{
			ID: run, Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Trigger: trigger, TriggeredBy: by,
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}]}`),
			Steps:  []agk.Step{"normalize", "archive"},
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// grantOn answers the identifier of the one grant who holds on finance.
func grantOn(t *testing.T, conn *pgx.Conn, who string) string {
	t.Helper()
	var id string
	if err := conn.QueryRow(t.Context(),
		`select id from grants where namespace = 'finance' and principal = $1`, who).Scan(&id); err != nil {
		t.Fatalf("the grant %s holds on finance: %s", who, err)
	}
	return id
}

// revoke revokes a grant on finance as DELETE /api/v1/{ns}/grants/{id} does, and records it in the
// audit log as the API does, with the grant as it was, by bob; and answers when it was recorded.
func revoke(t *testing.T, pool *db.Pool, conn *pgx.Conn, id string) time.Time {
	t.Helper()
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		g, err := ns.RevokeAccess(ctx, "", id)
		if err != nil {
			return err
		}
		return ns.Audit(ctx, audit.Record{
			Actor: "bob", Action: audit.GrantDelete, Target: id, Result: audit.Done,
			Detail: map[string]any{"principal": g.Principal, "scope": g.Scope.String(), "role": string(g.Role)},
		})
	}); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	if err := conn.QueryRow(t.Context(),
		`select at from audit_log where action = 'grant.delete' and target = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// afterTheRun is a moment after decidedRun was created, by the database's clock, which wrote when
// it was: a grant of a run somebody asked for is named for ending only after the run was created.
// It moves the controller's clock past it, so that a grant ending then has ended when the controller
// asks, and answers it as the reason writes it.
func afterTheRun(t *testing.T, later time.Duration) string {
	t.Helper()
	ends := time.Now().UTC().Add(time.Hour + later).Truncate(time.Second)
	if clock.now().Before(ends.Add(time.Hour)) {
		clock.set(ends.Add(time.Hour))
	}
	return ends.Format(time.RFC3339Nano)
}

// exec runs each statement as the superuser, for the state a case sets up past the policies.
func exec(t *testing.T, conn *pgx.Conn, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
}

// refused holds decidedRun to having been refused at creation for want: cancelled before it
// started, with nothing handed out, the reason on the run as GET /api/v1/runs/{id} reads it, and the
// refusal in the audit log as the installation's act, naming the principal and the reason.
func refused(t *testing.T, core *Core, q *fakeQueue, pool *db.Pool, conn *pgx.Conn, by, want string) {
	t.Helper()
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a run its principal may not start handed out %+v", got)
	}
	var d db.RunDetail
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		d, err = ns.RunDetail(ctx, decidedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d.State != agk.Cancelled || !d.StartedAt.IsZero() {
		t.Errorf("the run is %s, started at %v, and it is cancelled before it starts", d.State, d.StartedAt)
	}
	if d.Reason != want {
		t.Errorf("the run's reason is\n%q\nwant\n%q", d.Reason, want)
	}
	for _, s := range d.Steps {
		if s.Verdict != agk.VerdictPending {
			t.Errorf("step %s reads %s in a run that never started", s.Step, s.Verdict)
		}
	}

	var actor, namespace, target, result, detail string
	if err := conn.QueryRow(t.Context(),
		`select actor, namespace, target, result, detail from audit_log where action = 'run.cancel'`).
		Scan(&actor, &namespace, &target, &result, &detail); err != nil {
		t.Fatalf("the refusal in the audit log: %s", err)
	}
	var recorded map[string]string
	if err := json.Unmarshal([]byte(detail), &recorded); err != nil {
		t.Fatal(err)
	}
	if actor != "installation" || namespace != "finance" || target != string(decidedRun) || result != audit.Done ||
		recorded["reason"] != want || recorded["principal"] != by || recorded["workflow"] != "monthly-invoicing" {
		t.Errorf("the audit log records %s %s %s %s %s", actor, namespace, target, result, detail)
	}
}

// Every way a principal comes to no longer hold workflow:run between the moment its run was created
// and the moment the controller would let it in, and the reason each is named by.
func TestARunWhosePrincipalNoLongerHoldsWorkflowRunIsCancelledNamingWhatLapsed(t *testing.T) {
	for _, c := range []struct {
		what string
		// lapse sets it up past the request, and answers the reason the run ends with and
		// the principal it is attributed to.
		lapse func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (by, reason string)
	}{
		{"alice's grant revoked since the request", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			id := grantOn(t, conn, "alice")
			at := revoke(t, pool, conn, id)
			return "alice", fmt.Sprintf("alice no longer holds workflow:run on finance/monthly-invoicing: grant %s (operator on the namespace finance) was revoked at %s", id, at.UTC().Format(time.RFC3339Nano))
		}},
		{"alice's grant expired since the request", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			id := grantOn(t, conn, "alice")
			ends := afterTheRun(t, 0)
			exec(t, conn, `update grants set expires_at = '`+ends+`' where id = '`+id+`'`)
			return "alice", "alice no longer holds workflow:run on finance/monthly-invoicing: grant " + id + " (operator on the namespace finance) expired at " + ends
		}},
		{"the grant of alice's group expired, the last of two that gave it", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			first, last := afterTheRun(t, 0), afterTheRun(t, 30*time.Minute)
			exec(t, conn,
				`insert into principals (id, kind) values ('group:team-finance', 'group')`,
				`insert into groups (name) values ('team-finance')`,
				`insert into group_members (group_name, login) values ('team-finance', 'alice')`,
				`insert into grants (id, namespace, workflow, principal, role, granted_by, expires_at)
				   values ('01M2Z8V1P9C4XQ7K2N4D6F8G0A', 'finance', 'monthly-invoicing', 'group:team-finance', 'editor', 'bob', '`+last+`')`,
				`update grants set expires_at = '`+first+`' where principal = 'alice'`,
			)
			return "alice", "alice no longer holds workflow:run on finance/monthly-invoicing: grant 01M2Z8V1P9C4XQ7K2N4D6F8G0A (editor to group:team-finance on the workflow finance/monthly-invoicing) expired at " + last
		}},
		{"alice let in by a group she has left since, her own grant revoked before she asked", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			revoke(t, pool, conn, grantOn(t, conn, "alice"))
			exec(t, conn,
				`insert into principals (id, kind) values ('group:team-finance', 'group')`,
				`insert into groups (name) values ('team-finance')`,
				`insert into group_members (group_name, login) values ('team-finance', 'alice')`,
				`insert into grants (id, namespace, principal, role, granted_by) values ('01M2Z8V1P9C4XQ7K2N4D6F8G0E', 'finance', 'group:team-finance', 'operator', 'bob')`,
			)
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			exec(t, conn, `delete from group_members where login = 'alice'`)
			return "alice", "alice does not hold workflow:run on finance/monthly-invoicing: no grant gives it there"
		}},
		{"a deny of workflow:run written for alice since the request", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			exec(t, conn, `insert into grants (id, namespace, workflow, principal, deny, granted_by)
			   values ('01M2Z8V1P9C4XQ7K2N4D6F8G0B', 'finance', 'monthly-invoicing', 'alice', 'workflow:run', 'bob')`)
			return "alice", "alice no longer holds workflow:run on finance/monthly-invoicing: grant 01M2Z8V1P9C4XQ7K2N4D6F8G0B denies alice workflow:run on the workflow finance/monthly-invoicing"
		}},
		{"alice suspended since the request", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			exec(t, conn, `update users set suspended = true where login = 'alice'`)
			return "alice", "alice no longer holds workflow:run on finance/monthly-invoicing: alice is suspended"
		}},
		{"alice removed since the request", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			exec(t, conn, `delete from principals where id = 'alice'`)
			return "alice", "alice no longer holds workflow:run on finance/monthly-invoicing: the user alice was removed"
		}},
		{"a service account removed since its token started the run", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			mayRun(t, conn, "finance/nightly-sync", "finance")
			runBy(t, pool, decidedRun, agk.TriggerManual, "finance/nightly-sync")
			if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
				_, err := w.RemoveServiceAccount(ctx, "finance", "nightly-sync")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return "finance/nightly-sync", "finance/nightly-sync no longer holds workflow:run on finance/monthly-invoicing: the service account finance/nightly-sync was removed"
		}},
		{"a schedule's run, the built-in identity holding no grant", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			exec(t, conn, `delete from grants where principal = 'finance/agentiik'`)
			runBy(t, pool, decidedRun, agk.TriggerSchedule, "")
			return "finance/agentiik", "finance/agentiik does not hold workflow:run on finance/monthly-invoicing: no grant gives it there, and a namespace's built-in identity holds none until an owner gives it one"
		}},
		{"a schedule's run, the built-in identity's grant revoked after the schedule was armed and before it fired", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			id := grantOn(t, conn, "finance/agentiik")
			at := revoke(t, pool, conn, id)
			runBy(t, pool, decidedRun, agk.TriggerSchedule, "")
			return "finance/agentiik", fmt.Sprintf("finance/agentiik no longer holds workflow:run on finance/monthly-invoicing: grant %s (operator on the namespace finance) was revoked at %s", id, at.UTC().Format(time.RFC3339Nano))
		}},
		{"a grant revoked after it expired, which ended by its expiry", func(t *testing.T, pool *db.Pool, conn *pgx.Conn) (string, string) {
			runBy(t, pool, decidedRun, agk.TriggerManual, "alice")
			id := grantOn(t, conn, "alice")
			// By the database's clock, which dates the revocation: after the run was created and
			// before the grant was revoked.
			var ended time.Time
			if err := conn.QueryRow(t.Context(), `select clock_timestamp()`).Scan(&ended); err != nil {
				t.Fatal(err)
			}
			ended = ended.UTC()
			if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
				g, err := ns.RevokeAccess(ctx, "", id)
				if err != nil {
					return err
				}
				return ns.Audit(ctx, audit.Record{
					Actor: "bob", Action: audit.GrantDelete, Target: id, Result: audit.Done,
					Detail: map[string]any{"principal": g.Principal, "scope": g.Scope.String(), "role": string(g.Role),
						"expires_at": ended.Format(time.RFC3339Nano)},
				})
			}); err != nil {
				t.Fatal(err)
			}
			return "alice", "alice no longer holds workflow:run on finance/monthly-invoicing: grant " + id + " (operator on the namespace finance) expired at " + ended.Format(time.RFC3339Nano)
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			core, q, pool, super := deciding(t)
			conn := dbtest.Superuser(t, super)
			by, want := c.lapse(t, pool, conn)
			refused(t, core, q, pool, conn, by, want)
		})
	}
}

// The principals that still hold workflow:run are let in: alice by her own grant, by her group's
// alone, and a schedule's run by the grant an owner gave the built-in identity. And a grant ending
// later than now, or a deny that has ended, takes nothing away.
func TestARunWhosePrincipalStillHoldsWorkflowRunIsLetIn(t *testing.T) {
	for _, c := range []struct {
		what    string
		trigger agk.TriggerKind
		by      string
		setUp   []string
	}{
		{"alice by her own grant", agk.TriggerManual, "alice", nil},
		{"alice by her group's grant alone", agk.TriggerManual, "alice", []string{
			`delete from grants where principal = 'alice'`,
			`insert into principals (id, kind) values ('group:team-finance', 'group')`,
			`insert into groups (name) values ('team-finance')`,
			`insert into group_members (group_name, login) values ('team-finance', 'alice')`,
			`insert into grants (id, namespace, principal, role, granted_by) values ('01M2Z8V1P9C4XQ7K2N4D6F8G0C', 'finance', 'group:team-finance', 'operator', 'bob')`,
		}},
		{"alice by a grant ending later, beside a deny that has ended", agk.TriggerManual, "alice", []string{
			`update grants set expires_at = '2026-09-14T07:00:00Z' where principal = 'alice'`,
			`insert into grants (id, namespace, workflow, principal, deny, granted_by, expires_at)
			   values ('01M2Z8V1P9C4XQ7K2N4D6F8G0D', 'finance', 'monthly-invoicing', 'alice', 'workflow:run', 'bob', '2026-09-14T05:00:00Z')`,
		}},
		{"a schedule's run by the built-in identity's grant", agk.TriggerSchedule, "", nil},
	} {
		t.Run(c.what, func(t *testing.T) {
			core, q, pool, super := deciding(t)
			exec(t, dbtest.Superuser(t, super), c.setUp...)
			runBy(t, pool, decidedRun, c.trigger, c.by)
			if err := core.Decide(t.Context(), decidedRun); err != nil {
				t.Fatal(err)
			}
			if got := stateOf(t, core); got != agk.Running {
				t.Errorf("the run is %s", got)
			}
			if got := q.taken(); len(got) != 1 {
				t.Errorf("the run handed out %d tasks", len(got))
			}
		})
	}
}

// A run v0.2.5 left queued, attributed to operator, is let in after the upgrade while the bootstrap
// token lasts, since the token is that operator's under its v0.3.0 name and "after the upgrade it
// works as before". Once the first administrator has enrolled and the token has ended, operator
// holds nothing, as the API answers it then: a run of its still waiting to be let in ends cancelled,
// naming the end of the bootstrap, and one already let in goes on, since what was decided at its
// creation stands.
func TestARunOfTheV02OperatorIsLetInWhileTheBootstrapLasts(t *testing.T) {
	core, q, pool, super := deciding(t)
	conn := dbtest.Superuser(t, super)
	const later agk.RunID = "01M2Z8V1P9C4XQ7K2N4D6F8H0F"
	// As v0.2.5 wrote them, before the upgrade gave the namespace a built-in identity or anybody a
	// grant.
	exec(t, conn, `delete from grants`)
	for _, run := range []agk.RunID{decidedRun, later} {
		runBy(t, pool, run, agk.TriggerManual, "operator")
	}

	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	handed := q.taken()
	if got := stateOf(t, core); got != agk.Running || len(handed) != 1 {
		t.Fatalf("while the bootstrap lasts operator's run is %s and handed out %d tasks", got, len(handed))
	}

	exec(t, conn, `update bootstrap set enrolled_at = '2026-09-14T05:59:00Z', token_hash = null`)
	if err := core.Decide(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	var d db.RunDetail
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		var err error
		d, err = ns.RunDetail(ctx, later)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := "operator no longer holds workflow:run on finance/monthly-invoicing: the bootstrap token ended at 2026-09-14T05:59:00Z, when the first administrator enrolled a passkey"
	if d.State != agk.Cancelled || d.Reason != want {
		t.Errorf("once the bootstrap has ended operator's waiting run is %s, for\n%q\nwant\n%q", d.State, d.Reason, want)
	}
	if got := q.taken(); len(got) != 0 {
		t.Errorf("a run refused handed out %+v", got)
	}

	// The run let in before goes on to its next step.
	core.answer(t, succeeded(t, handed[0], core.now()))
	if got := stateOf(t, core); got != agk.Running {
		t.Errorf("operator's run let in before the bootstrap ended is %s", got)
	}
	if got := q.taken(); len(got) != 1 || got[0].Step != "archive" {
		t.Errorf("operator's run let in before the bootstrap ended handed out %+v next", got)
	}
}

// "Authorisation is re-evaluated when a run is created": a run once let in is not asked again, so
// a grant revoked while it runs stops nothing, as revoking a grant "does not protect a secret a step
// already used" either. Stopping a run is cancelling it, which is somebody's act.
func TestARunLetInIsNotAskedAgain(t *testing.T) {
	core, q, pool, super := deciding(t)
	conn := dbtest.Superuser(t, super)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	first := q.taken()
	if len(first) != 1 {
		t.Fatalf("the run handed out %d tasks", len(first))
	}
	revoke(t, pool, conn, grantOn(t, conn, "alice"))

	core.answer(t, succeeded(t, first[0], core.now()))
	if got := q.taken(); len(got) != 1 || got[0].Step != "archive" {
		t.Errorf("once alice's grant was revoked her running run handed out %+v", got)
	}
	if got := stateOf(t, core); got != agk.Running {
		t.Errorf("once alice's grant was revoked her running run is %s", got)
	}
}

// A refusal that reaches a run let in since it was asked about, by a pass that read the run first,
// leaves it going: what was decided when it was created stands.
func TestARefusalReachingARunLetInSinceChangesNothing(t *testing.T) {
	core, q, pool, _ := deciding(t)
	createRun(t, pool)
	if err := core.Decide(t.Context(), decidedRun); err != nil {
		t.Fatal(err)
	}
	if got := q.taken(); len(got) != 1 {
		t.Fatalf("the run handed out %d tasks", len(got))
	}
	if err := core.refuse(t.Context(), decidedRun, "alice no longer holds workflow:run on finance/monthly-invoicing: alice is suspended"); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, core); got != agk.Running {
		t.Errorf("a refusal reaching a run let in since took it to %s", got)
	}
	if got := q.stops(); len(got) != 0 {
		t.Errorf("a refusal reaching a run let in since stopped %+v", got)
	}
}

// A run is asked before its concurrency group is, so a run nobody may start never takes the group
// with cancel_in_progress: the run holding it goes on, and the refused one ends. And a run waiting
// on its group is asked again on every pass, so one whose principal lost the permission while it
// waited ends then, rather than when the group comes free.
func TestARunRefusedAtCreationCancelsNobodyElse(t *testing.T) {
	t.Run("with cancel_in_progress", func(t *testing.T) {
		core, q, pool, super := decidingOn(t, impatientWorkflow)
		createRun(t, pool)
		if err := core.Decide(t.Context(), decidedRun); err != nil {
			t.Fatal(err)
		}
		if got := q.taken(); len(got) != 1 {
			t.Fatalf("the first run handed out %d tasks", len(got))
		}
		exec(t, dbtest.Superuser(t, super), `delete from grants where principal = 'finance/agentiik'`)
		createSecond(t, pool)
		if err := core.Decide(t.Context(), second); err != nil {
			t.Fatal(err)
		}
		if got := runState(t, core, decidedRun); got != agk.Running {
			t.Errorf("a run its principal may not start took the group, and the run holding it is %s", got)
		}
		if got := q.stops(); len(got) != 0 {
			t.Errorf("a run its principal may not start stopped %+v", got)
		}
		if got := runState(t, core, second); got != agk.Cancelled {
			t.Errorf("the run its principal may not start is %s", got)
		}
	})

	t.Run("waiting on the group", func(t *testing.T) {
		core, q, pool, super := decidingOn(t, groupedWorkflow)
		conn := dbtest.Superuser(t, super)
		createRun(t, pool)
		createSecond(t, pool)
		if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
			t.Fatal(err)
		}
		if got := runState(t, core, second); got != agk.Queued {
			t.Fatalf("the run behind the group is %s", got)
		}
		q.taken()

		revoke(t, pool, conn, grantOn(t, conn, "finance/agentiik"))
		if err := core.Wake(t.Context(), Wake{Swept: true}); err != nil {
			t.Fatal(err)
		}
		if got := runState(t, core, second); got != agk.Cancelled {
			t.Errorf("a run whose principal lost workflow:run while it waited on its group is %s", got)
		}
		if got := runState(t, core, decidedRun); got != agk.Running {
			t.Errorf("the run holding the group is %s", got)
		}
		if got := q.taken(); len(got) != 0 {
			t.Errorf("the sweep handed out %+v", got)
		}
	})
}
