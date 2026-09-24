package db

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// A machine joins, says it is there, and stops saying it.

// hostKey is the public half of the keypair a host generates at join, one host to a seed.
func hostKey(seed byte) ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
}

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
		issued, err = w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz", "arch=amd64"}, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err = w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(1), Labels: []string{"zone=dmz"},
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
		r, err := w.Authenticate(ctx, joined.Credential, now)
		if err != nil {
			return err
		}
		if r.ID != joined.Runner || r.Pool != "dmz" {
			t.Errorf("the credential opened %+v", r)
		}
		if len(r.Labels) != 1 || r.Labels[0] != "zone=dmz" {
			t.Errorf("the runner claims %v", r.Labels)
		}
		if _, err := w.Authenticate(ctx, joined.Credential+"x", now); !errors.Is(err, ErrNoRunner) {
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
		issued, err := w.IssueJoinToken(ctx, "default", []string{"arch=amd64"}, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		machine := Joining{
			Token: issued.Clear, PublicKey: hostKey(1), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
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
		issued, err := w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz"}, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		_, err = w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(1), Labels: []string{"zone=dmz", "zone=lan"},
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
		issued, err := w.IssueJoinToken(ctx, "dmz", nil, "admin", now, now.Add(time.Minute))
		if err != nil {
			return err
		}
		_, err = w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(1), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
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

// What a host says of itself at join is what the inventory reads back: the key it will sign a
// rotation with, the namespaces it narrows itself to, what it can prove of its containment and a
// vCPU count past 32 bits.
// The identifier it is answered with is lowercase, which is the grammar the wire prints a runner
// in and the one the result reader holds it to.
func TestAHostsKeyAndNamespacesAreKeptAsItSentThem(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()
	narrowed(t, pool, []string{"finance", "team-ops"})

	var joined Joined
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "narrow", nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err = w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(7), Labels: []string{},
			CPU: 1 << 33, MemoryBytes: 1 << 34, DiskBytes: 1 << 38,
			Architecture: "arm64", AgentVersion: "0.2.0",
			Namespaces:  []string{"finance"},
			Containment: &Containment{Runtime: "runsc", UsernsRemap: false},
		}, time.Hour, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !runnerForm.MatchString(joined.Runner) {
		t.Errorf("the runner was minted as %q, which the wire does not print a runner as", joined.Runner)
	}

	var listed []Runner
	var opened Runner
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		if listed, err = w.Runners(ctx); err != nil {
			return err
		}
		opened, err = w.Authenticate(ctx, joined.Credential, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("the inventory holds %d runners", len(listed))
	}
	for _, r := range []Runner{listed[0], opened} {
		if !bytes.Equal(r.PublicKey, hostKey(7)) {
			t.Errorf("the runner's key reads back as %x", r.PublicKey)
		}
		// The wire sets vCPU a least and no most, so a count past 32 bits is kept rather
		// than refused by the column as a failure of the installation's.
		if r.CPU != 1<<33 {
			t.Errorf("the host's vCPU read back as %d", r.CPU)
		}
		if len(r.Namespaces) != 1 || r.Namespaces[0] != "finance" {
			t.Errorf("the host's namespaces read back as %v", r.Namespaces)
		}
		if r.Containment == nil || *r.Containment != (Containment{Runtime: "runsc", UsernsRemap: false}) {
			t.Errorf("the host's containment reads back as %+v", r.Containment)
		}
	}

	// A host that narrows nothing and reports no containment reads back as having said
	// nothing, rather than as narrowed to no namespace or contained by no runtime.
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "narrow", nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		plain, err := w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(8),
			CPU: 8, MemoryBytes: 1 << 34, DiskBytes: 1 << 38,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, time.Hour, now)
		if err != nil {
			return err
		}
		r, err := w.Authenticate(ctx, plain.Credential, now)
		if err != nil {
			return err
		}
		if r.Namespaces != nil || r.Containment != nil {
			t.Errorf("a host that said nothing of either reads back as narrowed to %v and contained by %+v", r.Namespaces, r.Containment)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// "It narrows and never widens": a host naming a namespace its pool does not accept is refused
// whole, as a label its token does not permit is, and the token is left unspent.
func TestAHostCannotNarrowItselfToANamespaceItsPoolDoesNotAccept(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()
	narrowed(t, pool, []string{"finance"})

	var issued JoinToken
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		issued, err = w.IssueJoinToken(ctx, "narrow", nil, "admin", now, now.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	machine := Joining{
		Token: issued.Clear, PublicKey: hostKey(1), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
		Architecture: "amd64", AgentVersion: "0.2.0", Namespaces: []string{"finance", "payroll"},
	}
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.Join(ctx, machine, time.Hour, now)
		return err
	})
	if !errors.Is(err, ErrNoJoinToken) {
		t.Fatalf("a host widening its pool's namespaces answered %v", err)
	}

	// The refusal spent nothing, and the same host asking for what its pool accepts joins.
	machine.Namespaces = []string{"finance"}
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		_, err := w.Join(ctx, machine, time.Hour, now)
		return err
	})
	if err != nil {
		t.Errorf("the token a refused join presented answered %v", err)
	}

	// A pool accepting every namespace, which is one that lists none, takes any narrowing.
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		machine.Token, machine.Namespaces = issued.Clear, []string{"payroll"}
		_, err = w.Join(ctx, machine, time.Hour, now)
		return err
	})
	if err != nil {
		t.Errorf("a host narrowing a pool that accepts every namespace answered %v", err)
	}
}

// "It is single use, so a second registration presenting it is refused rather than producing a
// second runner." A second machine presenting the token while the first has spent it and not yet
// committed waits for the first, and is then refused as a spent token is: one token, one runner.
func TestTwoJoinsWithOneTokenAtOnceMakeOneRunner(t *testing.T) {
	pool, super := joining(t)
	now := time.Now().UTC()

	var issued JoinToken
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		issued, err = w.IssueJoinToken(ctx, "default", nil, "admin", now, now.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	machine := func(host byte) Joining {
		return Joining{
			Token: issued.Clear, PublicKey: hostKey(host),
			CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}
	}

	second := make(chan error, 1)
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		if _, err := w.Join(ctx, machine(1), time.Hour, now); err != nil {
			return err
		}
		go func() {
			second <- pool.Installation(context.Background(), RunnerInventory, func(ctx context.Context, w *Wide) error {
				_, err := w.Join(ctx, machine(2), time.Hour, now)
				return err
			})
		}()
		// The first commits only once the second is waiting on it, so that the second
		// has read the token before the first's spending of it is visible.
		waitingOnALock(t, super)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ErrNoJoinToken) {
		t.Errorf("a machine presenting a token another was spending answered %v", err)
	}

	var listed []Runner
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		listed, err = w.Runners(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Errorf("one token made %d runners", len(listed))
	}
}

// waitingOnALock returns once a session of the test's database is waiting on a lock another holds.
func waitingOnALock(t *testing.T, super string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := conn.QueryRow(t.Context(),
			`select count(*) from pg_stat_activity
			 where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session came to wait on a lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// "reimaged host: A new runner, which joins again, as is any host whose key is gone. The old
// record stays for the audit log." A host joining a second time, with a second token and even with
// the same key, is a second runner under a new identifier and a new credential, and the first is
// left as it was: never silently reused.
func TestASecondJoinFromOneHostIsASecondRunner(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC()

	join := func() Joined {
		t.Helper()
		var joined Joined
		err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
			issued, err := w.IssueJoinToken(ctx, "dmz", []string{"zone=dmz"}, "admin", now, now.Add(time.Hour))
			if err != nil {
				return err
			}
			joined, err = w.Join(ctx, Joining{
				Token: issued.Clear, PublicKey: hostKey(1), Labels: []string{"zone=dmz"},
				CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
				Architecture: "amd64", AgentVersion: "0.2.0",
			}, time.Hour, now)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return joined
	}
	first, second := join(), join()
	if first.Runner == second.Runner || first.Credential == second.Credential {
		t.Fatalf("a host joining twice was answered %+v and %+v", first, second)
	}

	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		listed, err := w.Runners(ctx)
		if err != nil {
			return err
		}
		if len(listed) != 2 {
			t.Errorf("a host joining twice left %d runners", len(listed))
		}
		for _, j := range []Joined{first, second} {
			r, err := w.Authenticate(ctx, j.Credential, now)
			if err != nil {
				t.Errorf("the credential of %s answered %v", j.Runner, err)
				continue
			}
			if r.ID != j.Runner || r.State != "ready" {
				t.Errorf("the credential of %s opened %+v", j.Runner, r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// narrowed creates a pool accepting only the namespaces named, for a host to narrow itself inside.
func narrowed(t *testing.T, p *Pool, namespaces []string) {
	t.Helper()
	err := p.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		return w.CreateRunnerPool(ctx, RunnerPool{Name: "narrow", AcceptedNamespaces: namespaces, CreatedBy: "admin"})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runnerForm is the wire's pattern for a runner, $defs/runnerRegistration/response/runner.
var runnerForm = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// beating is a heartbeat of a ready runner of four slots, holding the keys it is given.
func beating(holding ...agk.TaskID) Beating {
	return Beating{AgentVersion: "0.2.0", State: "ready", Concurrency: 4, Holding: holding}
}

// "A runner posts one heartbeat every 10 seconds to the API", and "three missed intervals move a
// task to lost". The tests that follow count in these two constants, so this one holds them to the
// figures the page gives.
func TestAHeartbeatIsTenSecondsAndThreeMissedAreALoss(t *testing.T) {
	if HeartbeatInterval != 10*time.Second {
		t.Errorf("a runner is told to report every %s, and the page says every 10 seconds", HeartbeatInterval)
	}
	if LostAfter != 3*HeartbeatInterval {
		t.Errorf("a task is lost after %s of silence, and the page says three intervals of %s", LostAfter, HeartbeatInterval)
	}
}

// "Three missed intervals move a task to lost", and a lost task is not a failed one: one is
// charged to the infrastructure and the other to the brick.
func TestATaskWhoseRunnerStoppedReportingIsLost(t *testing.T) {
	pool, super := joining(t)
	now := time.Now().UTC()

	var runner string
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: hostKey(1), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
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
		r, err := w.Beat(ctx, runner, beating(held), now)
		if err != nil {
			return err
		}
		if r.Runner.State != "ready" {
			t.Errorf("a runner that just joined is %q", r.Runner.State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing is lost yet.
	if lost, err := declaredLost(t, pool, time.Now().UTC()); err != nil || lost != 0 {
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
	lost, err := declaredLost(t, pool, time.Now().UTC())
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
			issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now, now.Add(time.Hour))
			if err != nil {
				return err
			}
			joined, err := w.Join(ctx, Joining{
				Token: issued.Clear, PublicKey: hostKey(1), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
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
		_, err := w.Beat(ctx, mine, beating(key), now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	lost, err := declaredLost(t, pool, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if lost != 1 {
		t.Errorf("a task kept alive by somebody else's heartbeat was lost %d times", lost)
	}
}

// "lost: The runner holding it stopped reporting." A task nobody has redeemed is waiting on the
// queue however long it waits, and a task redeemed after a long wait counts from its redemption
// rather than from the dispatch that put it on the queue.
func TestOnlyATaskARunnerHoldsIsLost(t *testing.T) {
	pool, super := joining(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// Two tasks dispatched five minutes ago, which is ten intervals of thirty seconds. One is
	// still on the queue and the other has just been taken.
	const waiting, taken = "01M2HWAAAAAAAAAAAAAAAAAAAA", "01M2HTAAAAAAAAAAAAAAAAAAAA"
	for i, row := range []string{waiting, taken} {
		if _, err := conn.Exec(ctx, `
			insert into tasks (namespace, id, run_id, step, attempt, state, dispatched_at)
			values ('finance', $1, $2, 'render', $3, 'dispatched', now() - interval '5 minutes')`,
			row, string(financeRun), i+1); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	key := agk.NewTaskID(financeRun, "render", 2, agk.Shard{})
	var clear string
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		granted, err := w.IssueGrant(ctx, "finance", key, taken,
			GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
		clear = granted.Clear
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, clear, key, "runner-dmz-02", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if lost, err := declaredLost(t, pool, time.Now().UTC()); err != nil || lost != 0 {
		t.Fatalf("a task on the queue and a task taken a moment ago were lost %d times, %v", lost, err)
	}

	// A pass that could not record the dispatch publishes the task again with a grant of its
	// own, which nobody redeems, since the task is already taken. The redemption still counts.
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		_, err := w.IssueGrant(ctx, "finance", key, taken,
			GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if lost, err := declaredLost(t, pool, time.Now().UTC()); err != nil || lost != 0 {
		t.Fatalf("a task taken a moment ago and issued a grant since was lost %d times, %v", lost, err)
	}

	// The runner that took it says nothing for ten intervals after taking it.
	if _, err := conn.Exec(ctx,
		`update task_grants set redeemed_at = now() - interval '5 minutes' where task_id = $1`, taken); err != nil {
		t.Fatal(err)
	}
	if lost, err := declaredLost(t, pool, time.Now().UTC()); err != nil || lost != 1 {
		t.Fatalf("a runner silent since it took its task lost %d tasks, %v", lost, err)
	}
	var states []string
	rows, err := conn.Query(ctx,
		`select id || ' ' || state || ' ' || coalesce(runner, '-') from tasks
		 where id in ($1, $2) order by attempt`, waiting, taken)
	if err != nil {
		t.Fatal(err)
	}
	if states, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	want := []string{waiting + " dispatched -", taken + " lost runner-dmz-02"}
	if len(states) != 2 || states[0] != want[0] || states[1] != want[1] {
		t.Errorf("the tasks read %q, want %q", states, want)
	}
}

// A decision takes its run's row and then writes each of that run's tasks. A sweep that took a
// task and then waited on its run would be the other half of a deadlock, and PostgreSQL would end
// one of the two, so the sweep takes the run first and passes over one a decision holds: it moves
// nothing of that run, the decision goes through, and the next sweep finds the loss.
func TestASweepPassesOverARunADecisionHolds(t *testing.T) {
	pool, super := joining(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// A task its runner redeemed five minutes ago and has said nothing of since, which is ten
	// times the bound.
	const row = "01M2HNAAAAAAAAAAAAAAAAAAAA"
	if _, err := conn.Exec(ctx, `
		insert into tasks (namespace, id, run_id, step, attempt, state, runner, dispatched_at)
		values ('finance', $1, $2, 'render', 1, 'running', 'runner-dmz-02', now() - interval '5 minutes')`,
		row, string(financeRun)); err != nil {
		t.Fatal(err)
	}

	// A decision on the run holds its row, as SaveDecision does before it writes the tasks.
	deciding, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer deciding.Close(ctx)
	decision, err := deciding.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer decision.Rollback(context.WithoutCancel(ctx))
	if _, err := decision.Exec(ctx,
		`update runs set seq = seq where namespace = 'finance' and id = $1`, string(financeRun)); err != nil {
		t.Fatal(err)
	}

	type sweep struct {
		lost int
		err  error
	}
	swept := make(chan sweep, 1)
	go func() {
		lost, err := declaredLost(t, pool, time.Now().UTC())
		swept <- sweep{lost, err}
	}()

	// The sweep is given a moment to reach the run, and the decision then writes the task.
	var first *sweep
	select {
	case s := <-swept:
		first = &s
	case <-time.After(time.Second):
	}
	if _, err := decision.Exec(ctx,
		`update tasks set log_lines = 0 where namespace = 'finance' and id = $1`, row); err != nil {
		t.Errorf("a decision writing its task beside a sweep answered %v", err)
	}
	if err := decision.Commit(ctx); err != nil {
		t.Errorf("a decision beside a sweep could not commit: %v", err)
	}
	if first == nil {
		select {
		case s := <-swept:
			first = &s
		case <-time.After(10 * time.Second):
			t.Fatal("the sweep never came back")
		}
	}
	if first.err != nil || first.lost != 0 {
		t.Errorf("a sweep beside a decision on the run moved %d tasks, answering %v", first.lost, first.err)
	}

	// The decision is over, and the next sweep finds the task it passed over.
	if lost, err := declaredLost(t, pool, time.Now().UTC()); err != nil || lost != 1 {
		t.Errorf("the sweep after the decision moved %d tasks, answering %v", lost, err)
	}
}

// A loss a runner reports takes the run's row before the task's, as a decision does, so one
// reported while its run is being decided waits for the decision rather than holding the task the
// decision is about to write.
func TestALossReportedWhileItsRunIsDecidedWaitsForTheDecision(t *testing.T) {
	pool, super := joining(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	const row = "01M2HRAAAAAAAAAAAAAAAAAAAA"
	key := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	if _, err := conn.Exec(ctx, `
		insert into tasks (namespace, id, run_id, step, attempt, state, runner, dispatched_at, published_at)
		values ('finance', $1, $2, 'render', 1, 'running', 'runner-1', now(), now())`,
		row, string(financeRun)); err != nil {
		t.Fatal(err)
	}

	deciding, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer deciding.Close(ctx)
	decision, err := deciding.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer decision.Rollback(context.WithoutCancel(ctx))
	if _, err := decision.Exec(ctx,
		`update runs set seq = seq where namespace = 'finance' and id = $1`, string(financeRun)); err != nil {
		t.Fatal(err)
	}

	type loss struct {
		moved bool
		err   error
	}
	reported := make(chan loss, 1)
	go func() {
		var moved bool
		err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			var err error
			moved, err = w.Lose(ctx, "finance", key, row, "runner-1", time.Now().UTC())
			return err
		})
		reported <- loss{moved, err}
	}()

	// The loss is given a moment to reach the run, and the decision then writes the task.
	time.Sleep(time.Second)
	if _, err := decision.Exec(ctx,
		`update tasks set log_lines = 0 where namespace = 'finance' and id = $1`, row); err != nil {
		t.Errorf("a decision writing its task beside a reported loss answered %v", err)
	}
	if err := decision.Commit(ctx); err != nil {
		t.Errorf("a decision beside a reported loss could not commit: %v", err)
	}
	select {
	case l := <-reported:
		if l.err != nil || !l.moved {
			t.Errorf("a loss reported beside a decision moved %v, answering %v", l.moved, l.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reported loss never came back")
	}
}

// declaredLost is the controller's sweep for silence as of now, on the door the controller's fence
// opens.
func declaredLost(t *testing.T, pool *Pool, now time.Time) (int, error) {
	t.Helper()
	var lost int
	err := pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		lost, err = w.Lost(ctx, now, 0)
		return err
	})
	return lost, err
}

// A drain takes a runner out of service and leaves it heard; a revocation hears it until its grace
// ends, and refuses it everywhere from then on. Who ordered each, and when, is on the row.
func TestARevokedRunnerIsHeardUntilItsGraceEnds(t *testing.T) {
	pool, super := joining(t)
	// To the microsecond, which is what PostgreSQL keeps of every instant here.
	now := time.Now().UTC().Truncate(time.Microsecond)
	joined := joinedWith(t, pool, privateKey(1), 30*24*time.Hour, now)
	in := func(fn func(ctx context.Context, w *Wide) error) {
		t.Helper()
		if err := pool.Installation(t.Context(), RunnerInventory, fn); err != nil {
			t.Fatal(err)
		}
	}

	// A task in its hands, which it reports at every heartbeat.
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	key := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	if _, err := conn.Exec(t.Context(), `
		insert into tasks (namespace, id, run_id, step, attempt, state, runner, dispatched_at)
		values ('finance', '01M2HAAAAAAAAAAAAAAAAAAAAA', $1, 'render', 1, 'running', $2, $3)`,
		string(financeRun), joined.Runner, now); err != nil {
		t.Fatal(err)
	}

	in(func(ctx context.Context, w *Wide) error {
		for what, drain := range map[string]func() (Runner, error){
			"by nobody":     func() (Runner, error) { return w.Drain(ctx, joined.Runner, "", "retired", now) },
			"for no reason": func() (Runner, error) { return w.Drain(ctx, joined.Runner, "admin", "", now) },
			"at no moment":  func() (Runner, error) { return w.Drain(ctx, joined.Runner, "admin", "retired", time.Time{}) },
		} {
			if _, err := drain(); err == nil {
				t.Errorf("a drain ordered %s was taken", what)
			}
		}
		if _, err := w.Drain(ctx, "nobody", "admin", "retired", now); !errors.Is(err, ErrNoRunner) {
			t.Errorf("draining a runner that is not there answered %v", err)
		}

		// Drained, and still opened and heard: it finishes what it holds.
		r, err := w.Drain(ctx, joined.Runner, "admin", "the host is being retired", now)
		if err != nil {
			return err
		}
		if r.State != "draining" || r.DrainReason != "the host is being retired" || r.DrainedBy != "admin" || !r.DrainedAt.Equal(now) {
			t.Errorf("a drained runner reads %+v", r)
		}
		if _, err := w.Authenticate(ctx, joined.Credential, now); err != nil {
			t.Errorf("a drained runner's credential answered %v", err)
		}
		// Drained again, it is answered as it stands: the first order is the one it obeys.
		again, err := w.Drain(ctx, joined.Runner, "bob", "another reason", now.Add(time.Minute))
		if err != nil {
			return err
		}
		if again.DrainedBy != "admin" || again.DrainReason != "the host is being retired" {
			t.Errorf("a runner drained twice reads %+v", again)
		}
		return nil
	})

	// Revoked, with a grace of an hour.
	grace := time.Hour
	at := now.Add(time.Minute)
	in(func(ctx context.Context, w *Wide) error {
		if _, err := w.Revoke(ctx, joined.Runner, "admin", "the credential leaked", at, 0); err == nil {
			t.Error("a revocation with no grace was taken")
		}
		if _, err := w.Revoke(ctx, "nobody", "admin", "the credential leaked", at, grace); !errors.Is(err, ErrNoRunner) {
			t.Errorf("revoking a runner that is not there answered %v", err)
		}
		r, err := w.Revoke(ctx, joined.Runner, "bob", "the credential leaked", at, grace)
		if err != nil {
			return err
		}
		if r.State != "revoked" || r.RevokedBy != "bob" || !r.RevokedAt.Equal(at) || !r.ResultsAcceptedUntil.Equal(at.Add(grace)) {
			t.Errorf("a revoked runner reads %+v", r)
		}
		if r.DrainedBy != "admin" || r.DrainReason != "the credential leaked" {
			t.Errorf("a runner drained and then revoked reads %+v, and keeps who drained it beside why it was revoked", r)
		}
		// Revoked again, it is given no more time.
		again, err := w.Revoke(ctx, joined.Runner, "carol", "again", at.Add(10*time.Minute), grace)
		if err != nil {
			return err
		}
		if !again.ResultsAcceptedUntil.Equal(at.Add(grace)) || again.RevokedBy != "bob" {
			t.Errorf("a runner revoked twice reads %+v", again)
		}
		// And a drain is not a way back.
		if _, err := w.Drain(ctx, joined.Runner, "admin", "back to draining", at); !errors.Is(err, ErrRunnerRevoked) {
			t.Errorf("draining a revoked runner answered %v", err)
		}
		return nil
	})

	// In its grace, it is opened as revoked and heard, and its heartbeat keeps its task alive.
	last := at.Add(grace - time.Microsecond)
	in(func(ctx context.Context, w *Wide) error {
		r, err := w.Authenticate(ctx, joined.Credential, last)
		if err != nil {
			t.Errorf("a revoked runner a moment before its grace ends answered %v", err)
		} else if r.State != "revoked" || !r.ResultsAcceptedUntil.Equal(at.Add(grace)) {
			t.Errorf("a revoked runner in its grace reads %+v", r)
		}
		return nil
	})
	if err := pool.Installation(t.Context(), Heartbeat, func(ctx context.Context, w *Wide) error {
		beaten, err := w.Beat(ctx, joined.Runner, beating(key), last)
		if err != nil {
			return err
		}
		if beaten.Runner.State != "revoked" {
			t.Errorf("a revoked runner's heartbeat is answered from %+v", beaten.Runner)
		}
		return nil
	}); err != nil {
		t.Fatalf("a revoked runner's heartbeat in its grace answered %s", err)
	}
	if lost, err := declaredLost(t, pool, last.Add(LostAfter-time.Second)); err != nil || lost != 0 {
		t.Fatalf("a task its revoked runner reported in the grace was declared lost: %d, %v", lost, err)
	}

	// From the end of the grace, it is opened nowhere and heard no more, and the task it still
	// holds is lost three intervals after it last said so.
	end := at.Add(grace)
	in(func(ctx context.Context, w *Wide) error {
		if _, err := w.Authenticate(ctx, joined.Credential, end); !errors.Is(err, ErrNoRunner) {
			t.Errorf("a revoked runner at the end of its grace answered %v", err)
		}
		return nil
	})
	if err := pool.Installation(t.Context(), Heartbeat, func(ctx context.Context, w *Wide) error {
		_, err := w.Beat(ctx, joined.Runner, beating(key), end)
		return err
	}); !errors.Is(err, ErrNoRunner) {
		t.Errorf("a revoked runner's heartbeat at the end of its grace answered %v", err)
	}
	if lost, err := declaredLost(t, pool, last.Add(LostAfter+time.Second)); err != nil || lost != 1 {
		t.Fatalf("the task a revoked runner still held after its grace was not declared lost: %d, %v", lost, err)
	}
}

// "cancel: Each key the request named whose dispatch, bound to this runner, the controller has
// ended as cancelled or timed_out. Never a lost dispatch: a host only cut off finishes its key."
// The answer is the backstop for agentiik.stops, which keeps nothing, so it names what a stop would
// have, to the runner a stop would have reached, and nothing else.
func TestAHeartbeatIsAnsweredWithWhatItsRunnerIsToStop(t *testing.T) {
	pool, super := joining(t)
	ctx := t.Context()
	now := time.Now().UTC()

	var mine, theirs string
	err := pool.Installation(ctx, RunnerInventory, func(ctx context.Context, w *Wide) error {
		for i, into := range []*string{&mine, &theirs} {
			issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now, now.Add(time.Hour))
			if err != nil {
				return err
			}
			joined, err := w.Join(ctx, Joining{
				Token: issued.Clear, PublicKey: hostKey(byte(i + 1)), CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
				Architecture: "amd64", AgentVersion: "0.2.0",
			}, time.Hour, now)
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

	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, row := range []struct {
		namespace, id, run, step string
		attempt, requeue         int
		state                    string
		runner                   *string
	}{
		// Mine, in flight, and ended by the cancellation below.
		{"finance", "01M2HC1AAAAAAAAAAAAAAAAAAA", financeRun, "render", 1, 0, "running", &mine},
		// Mine, lost, and requeued to theirs, which the cancellation then ends.
		{"finance", "01M2HC2AAAAAAAAAAAAAAAAAAA", financeRun, "render", 2, 0, "lost", &mine},
		{"finance", "01M2HC2BAAAAAAAAAAAAAAAAAA", financeRun, "render", 2, 1, "running", &theirs},
		// Theirs alone.
		{"finance", "01M2HC3AAAAAAAAAAAAAAAAAAA", financeRun, "render", 3, 0, "running", &theirs},
		// Nobody's: on the queue when the run was cancelled.
		{"finance", "01M2HC4AAAAAAAAAAAAAAAAAAA", financeRun, "render", 4, 0, "dispatched", nil},
		// Mine and cancelled, and not named by the heartbeat.
		{"finance", "01M2HC5AAAAAAAAAAAAAAAAAAA", financeRun, "render", 5, 0, "running", &mine},
		// Mine and past its deadline.
		{"finance", "01M2HC6AAAAAAAAAAAAAAAAAAA", financeRun, "render", 6, 0, "timed_out", &mine},
		// Mine and ended as it should be.
		{"finance", "01M2HC7AAAAAAAAAAAAAAAAAAA", financeRun, "render", 7, 0, "succeeded", &mine},
		// Mine, in another namespace, and still running.
		{"team-ops", "01M2HC8AAAAAAAAAAAAAAAAAAA", opsRun, "archive", 1, 0, "running", &mine},
	} {
		if _, err := conn.Exec(ctx, `
			insert into tasks (namespace, id, run_id, step, attempt, requeue, state, runner, dispatched_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
			row.namespace, row.id, row.run, row.step, row.attempt, row.requeue, row.state, row.runner); err != nil {
			t.Fatal(err)
		}
	}

	// The run is cancelled, which is what ends its tasks in flight as cancelled.
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		_, err := w.CancelTasks(ctx, "finance", financeRun, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	render := func(attempt int) agk.TaskID { return agk.NewTaskID(financeRun, "render", attempt, agk.Shard{}) }
	archive := agk.NewTaskID(opsRun, "archive", 1, agk.Shard{})
	beat := func(runner string, b Beating) Beaten {
		t.Helper()
		var beaten Beaten
		if err := pool.Installation(ctx, Heartbeat, func(ctx context.Context, w *Wide) error {
			var err error
			beaten, err = w.Beat(ctx, runner, b, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return beaten
	}

	got := beat(mine, Beating{
		AgentVersion: "0.2.1", State: "draining", Concurrency: 6,
		Holding: []agk.TaskID{render(7), render(6), render(4), render(3), render(2), render(1), archive},
	})
	if want := []agk.TaskID{render(1), render(6)}; !slices.Equal(got.Cancel, want) {
		t.Errorf("my heartbeat is told to stop %q, want %q", got.Cancel, want)
	}
	if got := beat(theirs, beating(render(2), render(3))); !slices.Equal(got.Cancel, []agk.TaskID{render(2), render(3)}) {
		t.Errorf("their heartbeat is told to stop %q, want the requeue and their own", got.Cancel)
	}
	if got := beat(theirs, beating()); got.Cancel == nil || len(got.Cancel) != 0 {
		t.Errorf("a heartbeat naming nothing is told to stop %#v, and it is always a list", got.Cancel)
	}

	// And what the runner said of itself is what the inventory now holds of it.
	var runners []Runner
	if err := pool.Installation(ctx, RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		runners, err = w.Runners(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range runners {
		if r.ID != mine {
			continue
		}
		if r.AgentVersion != "0.2.1" || r.ReportedState != "draining" || r.Concurrency != 6 {
			t.Errorf("the inventory holds %s at %s, %s, %d, and its heartbeat said 0.2.1, draining, 6",
				r.ID, r.AgentVersion, r.ReportedState, r.Concurrency)
		}
		if r.State != "ready" {
			t.Errorf("a runner that says it is draining was made %s, and only an order drains a runner", r.State)
		}
	}
}

// joinedWith joins one host of the default pool, its credential accepted for rotateAfter from now.
func joinedWith(t *testing.T, pool *Pool, key ed25519.PrivateKey, rotateAfter time.Duration, now time.Time) Joined {
	t.Helper()
	var joined Joined
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, "default", nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err = w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: key.Public().(ed25519.PublicKey),
			CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, rotateAfter, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return joined
}

// privateKey is the whole keypair hostKey is the public half of.
func privateKey(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}

// "The rotation window is rotate_by itself: a credential past it is refused everywhere, and that
// host joins again." Up to the instant, and not at it.
func TestACredentialIsRefusedFromItsRotateBy(t *testing.T) {
	pool, _ := joining(t)
	// To the microsecond, which is what PostgreSQL keeps of rotate_by.
	now := time.Now().UTC().Truncate(time.Microsecond)
	joined := joinedWith(t, pool, privateKey(1), time.Hour, now)

	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		if _, err := w.Authenticate(ctx, joined.Credential, now.Add(time.Hour-time.Microsecond)); err != nil {
			t.Errorf("a credential a moment before its rotate_by answered %v", err)
		}
		if _, err := w.Authenticate(ctx, joined.Credential, now.Add(time.Hour)); !errors.Is(err, ErrNoRunner) {
			t.Errorf("a credential at its rotate_by answered %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rotating is the rotation a host holding key sends for runner, signed at the moment given.
func rotating(credential, runner string, key ed25519.PrivateKey, at time.Time) Rotating {
	signed := []byte("agentiik runner rotation\n" + runner + "\n" + at.Format(time.RFC3339Nano))
	return Rotating{
		Credential: credential, Runner: runner, SignedAt: at,
		Signed: signed, Signature: ed25519.Sign(key, signed),
	}
}

// A rotation is renewed by the key the runner joined with and by nothing else, is good once, and
// leaves the credential it was presented with accepted until the new one is first used.
func TestARunnerCredentialRotatesWithTheKeyItJoinedWith(t *testing.T) {
	pool, _ := joining(t)
	// To the microsecond, which is what PostgreSQL keeps of rotate_by.
	now := time.Now().UTC().Truncate(time.Microsecond)
	key := privateKey(1)
	joined := joinedWith(t, pool, key, 30*24*time.Hour, now)
	other := joinedWith(t, pool, privateKey(2), 30*24*time.Hour, now)

	in := func(fn func(ctx context.Context, w *Wide) error) {
		t.Helper()
		if err := pool.Installation(t.Context(), RunnerInventory, fn); err != nil {
			t.Fatal(err)
		}
	}
	rotate := func(ro Rotating, at time.Time) (Rotated, error) {
		var rotated Rotated
		err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
			var err error
			rotated, err = w.Rotate(ctx, ro, 30*24*time.Hour, at)
			return err
		})
		return rotated, err
	}

	// Refused: another runner's key, a key nobody joined with, a message the signature is not
	// over, and a runner the credential does not open.
	tampered := rotating(joined.Credential, joined.Runner, key, now)
	tampered.Signed = []byte("agentiik runner rotation\n" + joined.Runner + "\n" + now.Add(time.Second).Format(time.RFC3339Nano))
	for name, c := range map[string]struct {
		ro   Rotating
		want error
	}{
		"signed by another runner's key":     {rotating(joined.Credential, joined.Runner, privateKey(2), now), ErrNotItsKey},
		"signed by a key nobody joined with": {rotating(joined.Credential, joined.Runner, privateKey(3), now), ErrNotItsKey},
		"signed over another message":        {tampered, ErrNotItsKey},
		"for a runner its credential is not": {rotating(joined.Credential, other.Runner, key, now), ErrNoRunner},
		"with a credential that opens none":  {rotating(joined.Credential+"x", joined.Runner, key, now), ErrNoRunner},
	} {
		if _, err := rotate(c.ro, now); !errors.Is(err, c.want) {
			t.Errorf("a rotation %s answered %v, want %v", name, err, c.want)
		}
	}

	first, err := rotate(rotating(joined.Credential, joined.Runner, key, now), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(time.Minute + 30*24*time.Hour); !first.RotateBy.Equal(want) {
		t.Errorf("the new credential rotates by %s, want %s", first.RotateBy, want)
	}

	// What Authenticate refuses, judged again under the lock: the hook may have let the
	// request through a moment before the credential went past its rotate_by or was revoked.
	if _, err := rotate(rotating(first.Credential, joined.Runner, key, now.Add(2*time.Minute)), first.RotateBy); !errors.Is(err, ErrNoRunner) {
		t.Errorf("a rotation at its credential's rotate_by answered %v", err)
	}
	in(func(ctx context.Context, w *Wide) error {
		_, err := w.Revoke(ctx, other.Runner, "admin", "the host was retired", now, time.Hour)
		return err
	})
	if _, err := rotate(rotating(other.Credential, other.Runner, privateKey(2), now), now); !errors.Is(err, ErrRunnerRevoked) {
		t.Errorf("a rotation by a revoked runner answered %v", err)
	}

	// Good once: the same signature, or one over an earlier moment, renews nothing again.
	if _, err := rotate(rotating(joined.Credential, joined.Runner, key, now), now.Add(time.Minute)); !errors.Is(err, ErrRotationReplayed) {
		t.Errorf("the same rotation sent again answered %v", err)
	}
	if _, err := rotate(rotating(joined.Credential, joined.Runner, key, now.Add(-time.Second)), now.Add(time.Minute)); !errors.Is(err, ErrRotationReplayed) {
		t.Errorf("a rotation signed before the last one answered %v", err)
	}

	in(func(ctx context.Context, w *Wide) error {
		at := now.Add(2 * time.Minute)
		r, err := w.Authenticate(ctx, joined.Credential, at)
		if err != nil {
			t.Errorf("the old credential, before the new one was used, answered %v", err)
		} else if !r.RotateBy.Equal(joined.RotateBy) {
			t.Errorf("the old credential is answered as rotating by %s, want its own %s", r.RotateBy, joined.RotateBy)
		}
		if _, err := w.Authenticate(ctx, first.Credential, at); err != nil {
			t.Errorf("the new credential answered %v", err)
		}
		return nil
	})
	in(func(ctx context.Context, w *Wide) error {
		if _, err := w.Authenticate(ctx, joined.Credential, now.Add(3*time.Minute)); !errors.Is(err, ErrNoRunner) {
			t.Errorf("the old credential, after the new one was used, answered %v", err)
		}
		return nil
	})
	if _, err := rotate(rotating(joined.Credential, joined.Runner, key, now.Add(3*time.Minute)), now.Add(3*time.Minute)); !errors.Is(err, ErrNoRunner) {
		t.Errorf("rotating with the old credential, after the new one was used, answered %v", err)
	}
}

// A request time written to the nanosecond is kept to the microsecond, and the same request sent
// again is still the same request: compared with what was kept, it would read as a later one.
func TestARotationSignedFinerThanAMicrosecondIsStillGoodOnce(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	key := privateKey(1)
	joined := joinedWith(t, pool, key, 30*24*time.Hour, now)

	rotate := func(ro Rotating) error {
		return pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
			_, err := w.Rotate(ctx, ro, 30*24*time.Hour, now.Add(time.Minute))
			return err
		})
	}
	signed := rotating(joined.Credential, joined.Runner, key, now.Add(time.Second+999*time.Nanosecond))
	if err := rotate(signed); err != nil {
		t.Fatal(err)
	}
	if err := rotate(signed); !errors.Is(err, ErrRotationReplayed) {
		t.Errorf("the same rotation, signed to the nanosecond, sent again answered %v", err)
	}
}

// The rotate_by a join or a rotation answers is the one Authenticate holds the credential to, to
// the microsecond PostgreSQL keeps, and not a promise of the nanoseconds past it.
func TestTheRotateByAnsweredIsTheOneEnforced(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC().Truncate(time.Microsecond).Add(999 * time.Nanosecond)
	key := privateKey(1)
	joined := joinedWith(t, pool, key, time.Hour, now)
	if want := now.Add(time.Hour).Truncate(time.Microsecond); !joined.RotateBy.Equal(want) {
		t.Errorf("a join answered rotate_by %s, and the credential is held to %s", joined.RotateBy, want)
	}

	var rotated Rotated
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		var err error
		rotated, err = w.Rotate(ctx, rotating(joined.Credential, joined.Runner, key, now), time.Hour, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		r, err := w.Authenticate(ctx, rotated.Credential, now)
		if err != nil {
			return err
		}
		if !r.RotateBy.Equal(rotated.RotateBy) {
			t.Errorf("a rotation answered rotate_by %s, and the credential is held to %s", rotated.RotateBy, r.RotateBy)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A draining runner rotates, since a drain takes no credential away and a drained runner stays up
// for as long as it is left drained. A revoked one does not: in its grace it is told it is revoked,
// since its credential still opens what the grace allows, and after it the credential opens nothing.
func TestADrainingRunnerRotatesAndARevokedOneDoesNot(t *testing.T) {
	pool, _ := joining(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	drained := joinedWith(t, pool, privateKey(1), 30*24*time.Hour, now)
	revoked := joinedWith(t, pool, privateKey(2), 30*24*time.Hour, now)
	if err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		if _, err := w.Drain(ctx, drained.Runner, "admin", "the host is being retired", now); err != nil {
			return err
		}
		_, err := w.Revoke(ctx, revoked.Runner, "admin", "the credential leaked", now, time.Hour)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rotate := func(ro Rotating, at time.Time) error {
		return pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
			_, err := w.Rotate(ctx, ro, 30*24*time.Hour, at)
			return err
		})
	}
	if err := rotate(rotating(drained.Credential, drained.Runner, privateKey(1), now), now.Add(time.Minute)); err != nil {
		t.Errorf("a draining runner's rotation answered %v", err)
	}
	if err := rotate(rotating(revoked.Credential, revoked.Runner, privateKey(2), now), now.Add(time.Minute)); !errors.Is(err, ErrRunnerRevoked) {
		t.Errorf("a revoked runner's rotation, in its grace, answered %v", err)
	}
	if err := rotate(rotating(revoked.Credential, revoked.Runner, privateKey(2), now.Add(time.Hour)), now.Add(time.Hour)); !errors.Is(err, ErrNoRunner) {
		t.Errorf("a revoked runner's rotation, at the end of its grace, answered %v", err)
	}
}
