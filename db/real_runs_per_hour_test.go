package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// max_runs_per_hour, against a real PostgreSQL: "the runs created in the last 60 minutes, a sliding
// count", counted wherever a run is created and whatever started it.

// runOf is a new run of finance's workflow, started the way kind starts one.
func runOf(kind agk.TriggerKind) NewRun {
	return NewRun{
		ID: agk.NewRunID(), Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Trigger: kind, TriggeredBy: "alice", Steps: []agk.Step{"normalize"},
	}
}

// createIn creates one run in a namespace, in a transaction of its own.
func createIn(t *testing.T, pool *Pool, namespace string, r NewRun) error {
	t.Helper()
	return pool.In(t.Context(), namespace, func(ctx context.Context, ns *NS) error {
		return ns.CreateRun(ctx, r)
	})
}

// ofTeamOps is a run of team-ops' workflow, which sets no quota.
func ofTeamOps() NewRun {
	return NewRun{
		ID: agk.NewRunID(), Workflow: "nightly", Commit: "b1c2d3e",
		Trigger: agk.TriggerSchedule, TriggeredBy: "cron", Steps: []agk.Step{"normalize"},
	}
}

// within says whether a duration is want, give or take the few seconds a test takes.
func within(got, want time.Duration) bool {
	return got > want-5*time.Second && got <= want
}

// One counter across every trigger kind, over the last 60 minutes and not the clock hour: the run a
// namespace creates past its quota is refused, whatever started it, and told when one more fits;
// a run created more than an hour ago no longer counts, and a namespace that sets no quota is
// refused nothing. The oldest run is 59 minutes old, so that it lies in the clock hour before this
// one at every minute but the last, where a count per clock hour would not count it.
func TestARunPastTheRunsAnHourIsRefusedWhateverStartedIt(t *testing.T) {
	pool, super := created(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	// The run seed created, 59 minutes ago, and a quota of three.
	if _, err := conn.Exec(t.Context(), `
		update runs set created_at = now() - interval '59 minutes' where namespace = 'finance';
		update namespaces set max_runs_per_hour = 3 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []agk.TriggerKind{agk.TriggerWebhook, agk.TriggerSchedule} {
		if err := createIn(t, pool, "finance", runOf(kind)); err != nil {
			t.Fatalf("a %s run within the quota was refused: %s", kind, err)
		}
	}
	for _, kind := range []agk.TriggerKind{agk.TriggerManual, agk.TriggerEvent, agk.TriggerWorkflow} {
		err := createIn(t, pool, "finance", runOf(kind))
		var reached *RunsPerHourReached
		if !errors.As(err, &reached) {
			t.Fatalf("a fourth run, %s, within the hour answered %v", kind, err)
		}
		if reached.Namespace != "finance" || reached.Limit != 3 {
			t.Errorf("the refusal reads %+v", reached)
		}
		// One more fits once the run seed created leaves the window, a minute from now.
		if !within(reached.RetryAfter, time.Minute) {
			t.Errorf("one more run is said to fit in %s, and the oldest run counted leaves the window in 1m", reached.RetryAfter)
		}
	}
	var runs int
	if err := conn.QueryRow(t.Context(), `select count(*) from runs where namespace = 'finance'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 3 {
		t.Errorf("finance holds %d runs, and its quota is three", runs)
	}

	// team-ops sets no quota, and finance's is not its own.
	for range 5 {
		if err := createIn(t, pool, "team-ops", ofTeamOps()); err != nil {
			t.Fatalf("a namespace with no quota was refused a run: %s", err)
		}
	}

	// A sliding count: once the oldest run is more than an hour old, one more fits, and only one.
	if _, err := conn.Exec(t.Context(),
		`update runs set created_at = now() - interval '61 minutes' where namespace = 'finance' and trigger = 'manual'`); err != nil {
		t.Fatal(err)
	}
	if err := createIn(t, pool, "finance", runOf(agk.TriggerManual)); err != nil {
		t.Fatalf("a run once the oldest left the window was refused: %s", err)
	}
	if err := createIn(t, pool, "finance", runOf(agk.TriggerManual)); !errors.As(err, new(*RunsPerHourReached)) {
		t.Fatalf("a run past the quota again answered %v", err)
	}
}

// Retry-After is when one more run fits, which is the oldest run counted leaving the window. Where
// an administrator lowered the quota below what the last hour holds, that is the run the quota
// places from the newest, not the oldest of all, since the oldest leaving would still leave the
// namespace at its quota.
func TestOneMoreRunFitsWhenTheOldestRunCountedLeaves(t *testing.T) {
	pool, super := created(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	for range 2 {
		if err := createIn(t, pool, "finance", runOf(agk.TriggerManual)); err != nil {
			t.Fatal(err)
		}
	}
	// Three runs, 40, 20 and 5 minutes old, and then a quota of two.
	if _, err := conn.Exec(t.Context(), `
		with aged as (
		  select id, row_number() over (order by id) as n from runs where namespace = 'finance')
		update runs r set created_at = now() - case a.n when 1 then interval '40 minutes'
		                                               when 2 then interval '20 minutes'
		                                               else interval '5 minutes' end
		  from aged a where r.namespace = 'finance' and r.id = a.id;
		update namespaces set max_runs_per_hour = 2 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}

	err = createIn(t, pool, "finance", runOf(agk.TriggerManual))
	var reached *RunsPerHourReached
	if !errors.As(err, &reached) {
		t.Fatalf("a run past a lowered quota answered %v", err)
	}
	if !within(reached.RetryAfter, 40*time.Minute) {
		t.Errorf("one more run is said to fit in %s, and the run 20 minutes old leaves the window in 40m", reached.RetryAfter)
	}
	if got := reached.Seconds(); got < 2395 || got > 2400 {
		t.Errorf("Retry-After would read %d seconds", got)
	}
	// And the reason names the quota, not three runs as two.
	if why := reached.Reason(); !strings.HasPrefix(why, "namespace finance has created as many runs in the last 60 minutes as its max_runs_per_hour, 2, allows, and one more fits in ") {
		t.Errorf("the refusal says %q", why)
	}
}

// Two replicas of the API creating a run in one namespace at once count one after the other: the
// second waits until the first has committed, counts its run, and is refused the one run too
// many. A namespace that sets no quota takes no lock, as before v0.3.0, so its creations do not
// wait on each other.
func TestTwoCreationsAtOnceAreCountedOneAfterTheOther(t *testing.T) {
	pool, super := created(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	// seed's run counts, so two leave room for one more.
	if _, err := conn.Exec(t.Context(), `update namespaces set max_runs_per_hour = 2 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		namespace string
		run       func() NewRun
		waits     bool
	}{
		{"finance", func() NewRun { return runOf(agk.TriggerManual) }, true},
		{"team-ops", ofTeamOps, false},
	} {
		t.Run(c.namespace, func(t *testing.T) {
			created, release, first := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				first <- pool.In(context.Background(), c.namespace, func(ctx context.Context, ns *NS) error {
					if err := ns.CreateRun(ctx, c.run()); err != nil {
						return err
					}
					close(created)
					<-release
					return nil
				})
			}()
			select {
			case <-created:
			case err := <-first:
				t.Fatalf("the first creation answered %v", err)
			}

			// Long enough for the second to have counted had it not waited, and far longer where it
			// should not wait at all, so that a slow machine is not taken for a lock.
			patience := 500 * time.Millisecond
			if !c.waits {
				patience = 10 * time.Second
			}
			second := make(chan error, 1)
			go func() { second <- createIn(t, pool, c.namespace, c.run()) }()
			var err error
			select {
			case err = <-second:
				if c.waits {
					close(release)
					t.Fatalf("the second creation answered %v while the first had not committed", err)
				}
			case <-time.After(patience):
				if !c.waits {
					close(release)
					t.Fatal("a creation in a namespace with no quota waited on another")
				}
			}
			close(release)
			if err := <-first; err != nil {
				t.Fatalf("the first creation answered %v", err)
			}
			if c.waits {
				err = <-second
				if !errors.As(err, new(*RunsPerHourReached)) {
					t.Errorf("the second creation answered %v once the first had committed", err)
				}
			} else if err != nil {
				t.Errorf("the second creation in a namespace with no quota answered %v", err)
			}
		})
	}
}

// firing is a trigger that is not a request, a schedule say, as package db's caller would write
// one: it creates the run a firing asks for and records the firing, a skipped one with its reason
// where the namespace is past its quota.
type firing struct {
	created []agk.RunID
	skipped []string
}

func (f *firing) fire(ctx context.Context, pool *Pool) error {
	return pool.In(ctx, "finance", func(ctx context.Context, ns *NS) error {
		r := runOf(agk.TriggerSchedule)
		err := ns.CreateRun(ctx, r)
		var reached *RunsPerHourReached
		switch {
		case errors.As(err, &reached):
			// Recorded in the transaction the refusal was answered in, which is still good:
			// a firing's record is written with it.
			var recorded string
			if err := ns.tx.QueryRow(ctx, `select $1::text`, reached.Reason()).Scan(&recorded); err != nil {
				return err
			}
			f.skipped = append(f.skipped, recorded)
			return nil
		case err != nil:
			return err
		}
		f.created = append(f.created, r.ID)
		return nil
	})
}

// A scheduled firing past the quota starts no run and is recorded as a skipped firing, with the
// reason, in the transaction that asked: the refusal wrote nothing and failed no statement, so the
// firing's own record commits with it. The request's side of the same refusal is the API's 429,
// tested there.
func TestAFiringPastTheQuotaIsSkippedWithItsReason(t *testing.T) {
	pool, super := created(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `update namespaces set max_runs_per_hour = 2 where name = 'finance'`); err != nil {
		t.Fatal(err)
	}

	f := &firing{}
	for range 3 {
		if err := f.fire(t.Context(), pool); err != nil {
			t.Fatalf("a firing failed where it should have been skipped: %s", err)
		}
	}
	if len(f.created) != 1 || len(f.skipped) != 2 {
		t.Fatalf("three firings created %d runs and skipped %d", len(f.created), len(f.skipped))
	}
	// A skipped firing commits, so the refusal must have written nothing for it to commit.
	var scheduled int
	if err := conn.QueryRow(t.Context(),
		`select count(*) from runs where namespace = 'finance' and trigger = 'schedule'`).Scan(&scheduled); err != nil {
		t.Fatal(err)
	}
	if scheduled != 1 {
		t.Errorf("three firings, two of them skipped, left %d runs", scheduled)
	}
	for _, why := range f.skipped {
		if !strings.HasPrefix(why, "namespace finance has created as many runs in the last 60 minutes as its max_runs_per_hour, 2, allows, and one more fits in ") {
			t.Errorf("a skipped firing says %q", why)
		}
	}
}

// Retry-After is whole seconds rounded up, since a client asking again a fraction of a second early
// would find the oldest run still counted, and never zero, which would ask again at once.
func TestRetryAfterIsWholeSecondsRoundedUp(t *testing.T) {
	for _, c := range []struct {
		after time.Duration
		want  int
	}{
		{29*time.Second + time.Millisecond, 30},
		{30 * time.Second, 30},
		{time.Millisecond, 1},
		{0, 1},
	} {
		if got := (&RunsPerHourReached{RetryAfter: c.after}).Seconds(); got != c.want {
			t.Errorf("%s reads as %d seconds, want %d", c.after, got, c.want)
		}
	}
}
