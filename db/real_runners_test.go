package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// A machine joins, says it is there, and stops saying it.

func joining(t *testing.T) (*Pool, string) {
	t.Helper()
	super, app := database(t)
	seed(t, super)
	steps(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pooled(t, pool)
	return pool, super
}

// pooled creates what an administrator creates before any of this exists: "An administrator
// creates a runner pool with its labels, its accepted namespaces and its resource ceilings, then
// issues a join token."
func pooled(t *testing.T, p *Pool) {
	t.Helper()
	err := p.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		for _, made := range []RunnerPool{
			{Name: "dmz", Labels: []string{"zone=dmz", "arch=amd64"}, CreatedBy: "admin"},
			{Name: "default", Labels: []string{"arch=amd64"}, CreatedBy: "admin"},
		} {
			if err := w.CreateRunnerPool(ctx, made); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// "The API verifies the token, checks that the claimed labels are a subset of what the token
// permits, refuses anything else, creates the runner record and returns a runner identifier and a
// long-lived credential."
func TestAMachineJoinsWithATokenAndGetsACredential(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()

	var issued JoinToken
	var joined Joined
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		issued, err = w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz", "arch=amd64"}, "admin", now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err = w.Join(ctx, Joining{
			Token: issued.Clear, Labels: []string{"zone=dmz"},
			CPU: 8, MemoryBytes: 1 << 34, DiskBytes: 1 << 38,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, 30*24*time.Hour, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if joined.Runner == "" || joined.Credential == "" || joined.Pool != "dmz" {
		t.Fatalf("joining answered %+v", joined)
	}
	if !joined.RotateBy.After(now) {
		t.Errorf("the credential rotates at %s", joined.RotateBy)
	}

	// The credential opens the runner it was issued to, and nothing else does.
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		r, err := w.Authenticate(ctx, joined.Credential)
		if err != nil {
			return err
		}
		if r.ID != joined.Runner || r.Pool != "dmz" {
			t.Errorf("the credential opened %+v", r)
		}
		if len(r.Labels) != 1 || r.Labels[0] != "zone=dmz" {
			t.Errorf("the runner claims %v", r.Labels)
		}
		if _, err := w.Authenticate(ctx, joined.Credential+"x"); !errors.Is(err, ErrNoRunner) {
			t.Errorf("a credential that is nearly right answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// "one machine, one token, and a reimaged host joins again." A token is spent by being used.
func TestAJoinTokenIsSpentOnce(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()

	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "default", []string{"arch=amd64"}, "admin", now.Add(time.Hour))
		if err != nil {
			return err
		}
		machine := Joining{
			Token: issued.Clear, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}
		if _, err := w.Join(ctx, machine, time.Hour, now); err != nil {
			return err
		}
		if _, err := w.Join(ctx, machine, time.Hour, now); !errors.Is(err, ErrNoJoinToken) {
			t.Errorf("a token spent twice answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// "Labels are not self-asserted. A runner can only ever claim labels its join token allowed, so a
// machine cannot add zone=lan to itself and start receiving the steps that were kept off the
// internet."
func TestAMachineCannotClaimALabelItsTokenDoesNotPermit(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()

	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz"}, "admin", now.Add(time.Hour))
		if err != nil {
			return err
		}
		_, err = w.Join(ctx, Joining{
			Token: issued.Clear, Labels: []string{"zone=dmz", "zone=lan"},
			CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, now)
		if !errors.Is(err, ErrNoJoinToken) {
			t.Errorf("a machine claiming a label its token does not permit answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// An expired token is refused like one that never existed.
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "dmz", nil, "admin", now.Add(time.Minute))
		if err != nil {
			return err
		}
		_, err = w.Join(ctx, Joining{
			Token: issued.Clear, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, now.Add(2*time.Minute))
		if !errors.Is(err, ErrNoJoinToken) {
			t.Errorf("an expired token answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// "Three missed intervals move a task to lost", and a lost task is not a failed one: one is
// charged to the infrastructure and the other to the brick.
func TestATaskWhoseRunnerStoppedReportingIsLost(t *testing.T) {
	pool, super := joining(t)
	now := time.Now().UTC()

	var runner string
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, Joining{
			Token: issued.Clear, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, now)
		runner = joined.Runner
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two tasks in this runner's hands, dispatched a moment ago.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	held := agk.NewTaskID(financeRun, "render", 2, agk.Shard{Index: 1, Of: 2})
	other := agk.NewTaskID(financeRun, "render", 2, agk.Shard{Index: 2, Of: 2})
	for i, shard := range []int{1, 2} {
		if _, err := conn.Exec(t.Context(), `
			insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of,
			                   state, runner, dispatched_at)
			values ('finance', $1, $2, 'render', 2, $3, 2, 'running', $4, now())`,
			"01M2H"+string(rune('A'+i))+"AAAAAAAAAAAAAAAAAAAAA", string(financeRun), shard, runner); err != nil {
			t.Fatal(err)
		}
	}

	// The runner says it is there and holding one of them.
	err = pool.Installation(t.Context(), Heartbeat, func(ctx context.Context, w *Wide) error {
		r, err := w.Beat(ctx, runner, []agk.TaskID{held}, now)
		if err != nil {
			return err
		}
		if r.State != "ready" {
			t.Errorf("a runner that just joined is %q", r.State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing is lost yet.
	if lost, err := pool.Lost(t.Context(), 30*time.Second, 0); err != nil || lost != 0 {
		t.Fatalf("a runner that just reported lost %d tasks, %v", lost, err)
	}

	// Three intervals pass with nothing said. The one it never claimed goes first, because
	// a task nobody has reported since it was dispatched counts from the dispatch.
	if _, err := conn.Exec(t.Context(),
		`update tasks set dispatched_at = now() - interval '5 minutes',
		                  last_heartbeat_at = case when last_heartbeat_at is null then null
		                                           else now() - interval '5 minutes' end
		 where step = 'render'`); err != nil {
		t.Fatal(err)
	}
	lost, err := pool.Lost(t.Context(), 30*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lost != 2 {
		t.Fatalf("%d tasks were lost and two were held", lost)
	}

	var states []string
	rows, err := conn.Query(t.Context(),
		`select state from tasks where run_id = $1 and step = 'render' order by shard_index`, string(financeRun))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	rows.Close()
	for _, s := range states {
		if s != "lost" {
			t.Errorf("a task of a runner that stopped reporting is %q", s)
		}
	}
	_ = other

	// And the run was woken, because a lost task is something the controller has to decide
	// about and nothing else would have told it.
	var wake *time.Time
	if err := conn.QueryRow(t.Context(), `select wake_at from runs where id = $1`, string(financeRun)).Scan(&wake); err != nil {
		t.Fatal(err)
	}
	if wake == nil {
		t.Error("the run holding the lost tasks was not woken")
	}
}

// A heartbeat naming somebody else's task keeps nothing alive, which is the one thing a liveness
// report must not be able to do.
func TestAHeartbeatCannotKeepSomebodyElseTaskAlive(t *testing.T) {
	pool, super := joining(t)
	now := time.Now().UTC()

	var mine, theirs string
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		for i, into := range []*string{&mine, &theirs} {
			issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now.Add(time.Hour))
			if err != nil {
				return err
			}
			joined, err := w.Join(ctx, Joining{
				Token: issued.Clear, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
				Architecture: "amd64", AgentVersion: "0.2.0",
			}, time.Hour, now.Add(time.Duration(i)*time.Second))
			if err != nil {
				return err
			}
			*into = joined.Runner
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	key := agk.NewTaskID(financeRun, "render", 7, agk.Shard{})
	if _, err := conn.Exec(t.Context(), `
		insert into tasks (namespace, id, run_id, step, attempt, state, runner, dispatched_at)
		values ('finance', '01M2HZAAAAAAAAAAAAAAAAAAAA', $1, 'render', 7, 'running', $2,
		        now() - interval '5 minutes')`, string(financeRun), theirs); err != nil {
		t.Fatal(err)
	}

	// My heartbeat names their task.
	err = pool.Installation(t.Context(), Heartbeat, func(ctx context.Context, w *Wide) error {
		_, err := w.Beat(ctx, mine, []agk.TaskID{key}, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	lost, err := pool.Lost(t.Context(), 30*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lost != 1 {
		t.Errorf("a task kept alive by somebody else's heartbeat was lost %d times", lost)
	}
}

// A revoked credential stops being accepted, which is what "revoking it from the console stops the
// runner at its next heartbeat" comes down to.
func TestARevokedCredentialOpensNothing(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()

	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, Joining{
			Token: issued.Clear, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, now)
		if err != nil {
			return err
		}

		if err := w.Drain(ctx, joined.Runner, "the host is being retired"); err != nil {
			return err
		}
		r, err := w.Authenticate(ctx, joined.Credential)
		if err != nil {
			return err
		}
		if r.State != "draining" || r.DrainReason == "" {
			t.Errorf("a drained runner reads %+v", r)
		}

		if err := w.Revoke(ctx, joined.Runner, "the credential leaked"); err != nil {
			return err
		}
		if _, err := w.Authenticate(ctx, joined.Credential); !errors.Is(err, ErrNoRunner) {
			t.Errorf("a revoked credential answered %v", err)
		}
		if _, err := w.Beat(ctx, joined.Runner, nil, now); !errors.Is(err, ErrNoRunner) {
			t.Errorf("a revoked runner's heartbeat answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
