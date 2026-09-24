package db

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// The grant's half of at-least-once delivery, against a real PostgreSQL: a redemption binds
// the task to the runner that made it, and a task that has ended is handed to nobody again.

// "the runner refuses to start a container for a key that has already completed", and the
// grant refuses it too, even to the runner that holds the task: a message delivered again
// after its result was recorded must not start the work a second time. Every one of the five
// endings counts, because each is a task nothing more is expected of.
func TestAnEndedTaskIsNotRedeemedEvenByItsRunner(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'render')`, financeRun); err != nil {
		t.Fatalf("seeding: %s", err)
	}

	now := time.Now().UTC()
	// dispatched writes one attempt of render the way the controller dispatches it: a row,
	// and the grant that goes into its message.
	dispatched := func(attempt int) (row string, key agk.TaskID, clear string) {
		row = "01M2G" + string(rune('A'+attempt)) + "AAAAAAAAAAAAAAAAAAAA"
		key = agk.NewTaskID(financeRun, "render", attempt, agk.Shard{})
		if _, err := conn.Exec(ctx, `
			insert into tasks (namespace, id, run_id, step, attempt, state)
			values ('finance', $1, $2, 'render', $3, 'dispatched')`, row, financeRun, attempt); err != nil {
			t.Fatalf("seeding attempt %d: %s", attempt, err)
		}
		err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			granted, err := w.IssueGrant(ctx, "finance", key, row,
				GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
			clear = granted.Clear
			return err
		})
		if err != nil {
			t.Fatalf("issuing the grant of attempt %d: %s", attempt, err)
		}
		return row, key, clear
	}
	redeem := func(clear string, key agk.TaskID) error {
		return pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
			_, err := w.Redeem(ctx, clear, key, "runner-1", now)
			return err
		})
	}

	// A task still dispatched redeems again for the runner that holds it, which a runner
	// that lost the answer to its first redemption depends on. It is also what makes the
	// refusals below about the state and not about the holder.
	_, key, clear := dispatched(1)
	for i := range 2 {
		if err := redeem(clear, key); err != nil {
			t.Fatalf("redemption %d of a task still dispatched, by the runner that holds it: %s", i+1, err)
		}
	}

	for i, ending := range []agk.TaskState{agk.TaskSucceeded, agk.TaskFailed, agk.TaskLost, agk.TaskTimedOut, agk.TaskCancelled} {
		row, key, clear := dispatched(i + 2)
		if err := redeem(clear, key); err != nil {
			t.Fatalf("the first redemption of the task that ends %s: %s", ending, err)
		}
		// The result is recorded, which moves the row to its ending and leaves it
		// bound to the runner that ran it.
		if _, err := conn.Exec(ctx,
			`update tasks set state = $2 where namespace = 'finance' and id = $1`, row, ending.String()); err != nil {
			t.Fatal(err)
		}
		if err := redeem(clear, key); !errors.Is(err, ErrTaskHeld) {
			t.Errorf("a task in state %s was redeemed again by its own runner, answering %v", ending, err)
		}
	}
}

// The binding a redemption makes is what a result is checked against on the way out, so it reads
// back as the runner that redeemed, as nobody before any redemption, and not at all for a dispatch
// named by the row of one task and the key of another.
func TestADispatchIsHeldByTheRunnerThatRedeemedIt(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'render')`, financeRun); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	const row = "01M2GHAAAAAAAAAAAAAAAAAAAA"
	key := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	if _, err := conn.Exec(ctx, `
		insert into tasks (namespace, id, run_id, step, attempt, state)
		values ('finance', $1, $2, 'render', 1, 'dispatched')`, row, financeRun); err != nil {
		t.Fatalf("seeding the task: %s", err)
	}

	now := time.Now().UTC()
	var clear string
	heldBy := func(key agk.TaskID, row string) (string, error) {
		var runner string
		err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			var err error
			runner, err = w.HeldBy(ctx, "finance", key, row)
			return err
		})
		return runner, err
	}
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		granted, err := w.IssueGrant(ctx, "finance", key, row, GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
		clear = granted.Clear
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if runner, err := heldBy(key, row); err != nil || runner != "" {
		t.Errorf("a dispatch nobody redeemed is held by %q, answering %v", runner, err)
	}
	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, clear, key, "runner-dmz-02", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if runner, err := heldBy(key, row); err != nil || runner != "runner-dmz-02" {
		t.Errorf("a dispatch redeemed by runner-dmz-02 is held by %q, answering %v", runner, err)
	}

	for _, c := range []struct {
		why string
		key agk.TaskID
		row string
	}{
		{"the key of another task", agk.NewTaskID(financeRun, "render", 2, agk.Shard{}), row},
		{"a row nobody wrote", key, "01M2GHZZZZZZZZZZZZZZZZZZZZ"},
		{"a row that is not an identifier at all", key, "not a ulid; drop table tasks"},
	} {
		if _, err := heldBy(c.key, c.row); !errors.Is(err, ErrNoDispatch) {
			t.Errorf("a dispatch named by %s answered %v", c.why, err)
		}
	}
}

// A task that never reached a container ended before anybody redeemed it, and the first runner to
// say so is bound to it, as a redemption would have bound it. A second runner saying the same is
// told who holds it, and so is one reporting about a dispatch a redemption bound first.
func TestADispatchNobodyRedeemedIsBoundToTheFirstRunnerToEndIt(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'render')`, financeRun); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	const unredeemed, redeemed = "01M2GHAAAAAAAAAAAAAAAAAAAA", "01M2GHBBBBBBBBBBBBBBBBBBBB"
	first := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	second := agk.NewTaskID(financeRun, "render", 2, agk.Shard{})
	for row, attempt := range map[string]int{unredeemed: 1, redeemed: 2} {
		if _, err := conn.Exec(ctx, `
			insert into tasks (namespace, id, run_id, step, attempt, state)
			values ('finance', $1, $2, 'render', $3, 'dispatched')`, row, financeRun, attempt); err != nil {
			t.Fatalf("seeding the task: %s", err)
		}
	}

	now := time.Now().UTC()
	var clear string
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		granted, err := w.IssueGrant(ctx, "finance", second, redeemed, GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
		clear = granted.Clear
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, clear, second, "runner-dmz-02", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	bind := func(key agk.TaskID, row, runner string) (string, error) {
		var holder string
		err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			var err error
			holder, err = w.BindUnredeemed(ctx, "finance", key, row, runner)
			return err
		})
		return holder, err
	}
	if holder, err := bind(first, unredeemed, "runner-dmz-01"); err != nil || holder != "runner-dmz-01" {
		t.Errorf("the first runner to end a dispatch nobody redeemed left it held by %q, answering %v", holder, err)
	}
	if holder, err := bind(first, unredeemed, "runner-dmz-03"); err != nil || holder != "runner-dmz-01" {
		t.Errorf("a second runner ending it left it held by %q, answering %v", holder, err)
	}
	if holder, err := bind(second, redeemed, "runner-dmz-03"); err != nil || holder != "runner-dmz-02" {
		t.Errorf("a dispatch redeemed by runner-dmz-02 is held by %q once another runner ended it, answering %v", holder, err)
	}
	if _, err := bind(second, unredeemed, "runner-dmz-03"); !errors.Is(err, ErrNoDispatch) {
		t.Errorf("a dispatch named by the row of one task and the key of another answered %v", err)
	}
}

// A requeue that comes back to the host which already ended its key is answered from that host's
// record, and nobody redeems it. Its ending is held instead to the redemption of a dispatch of the
// same key handed out before it: the runner that redeemed one was given that work. Not a runner
// that redeemed nothing of the key, not a dispatch it merely ended without reaching a container,
// and not the requeue itself or anything after it.
func TestARequeueIsAnsweredByTheRunnerThatRedeemedADispatchBeforeIt(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'render')`, financeRun); err != nil {
		t.Fatalf("seeding: %s", err)
	}

	now := time.Now().UTC()
	// dispatch writes one dispatch of one attempt of render, with its grant, and answers the
	// grant's clear value.
	dispatch := func(row string, attempt, requeue int) string {
		t.Helper()
		key := agk.NewTaskID(financeRun, "render", attempt, agk.Shard{})
		if _, err := conn.Exec(ctx, `
			insert into tasks (namespace, id, run_id, step, attempt, requeue, state)
			values ('finance', $1, $2, 'render', $3, $4, 'dispatched')`, row, financeRun, attempt, requeue); err != nil {
			t.Fatalf("seeding dispatch %d of attempt %d: %s", requeue, attempt, err)
		}
		var clear string
		if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			granted, err := w.IssueGrant(ctx, "finance", key, row, GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
			clear = granted.Clear
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return clear
	}
	lose := func(row string) {
		t.Helper()
		if _, err := conn.Exec(ctx, `update tasks set state = 'lost' where id = $1`, row); err != nil {
			t.Fatal(err)
		}
	}

	// Attempt 1 is redeemed by runner-dmz-01, lost, and requeued.
	const redeemed, requeue = "01M2GHAAAAAAAAAAAAAAAAAAAA", "01M2GHBBBBBBBBBBBBBBBBBBBB"
	first := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	clear := dispatch(redeemed, 1, 0)
	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, clear, first, "runner-dmz-01", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	lose(redeemed)
	dispatch(requeue, 1, 1)

	// Attempt 2 is bound to runner-dmz-03 with no redemption behind it, lost, and requeued.
	const bound, boundRequeue = "01M2GHCCCCCCCCCCCCCCCCCCCC", "01M2GHDDDDDDDDDDDDDDDDDDDD"
	second := agk.NewTaskID(financeRun, "render", 2, agk.Shard{})
	dispatch(bound, 2, 0)
	if _, err := conn.Exec(ctx, `update tasks set runner = 'runner-dmz-03', state = 'lost' where id = $1`, bound); err != nil {
		t.Fatal(err)
	}
	dispatch(boundRequeue, 2, 1)

	// Attempt 3 is lost before anybody took it, and its requeue is redeemed by runner-dmz-04.
	const unredeemed, laterRedeemed = "01M2GHEEEEEEEEEEEEEEEEEEEE", "01M2GHFFFFFFFFFFFFFFFFFFFF"
	third := agk.NewTaskID(financeRun, "render", 3, agk.Shard{})
	dispatch(unredeemed, 3, 0)
	lose(unredeemed)
	laterClear := dispatch(laterRedeemed, 3, 1)
	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, laterClear, third, "runner-dmz-04", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		why    string
		key    agk.TaskID
		row    string
		runner string
		want   bool
	}{
		{"the runner that redeemed the dispatch before the requeue", first, requeue, "runner-dmz-01", true},
		{"a runner that redeemed nothing of the key", first, requeue, "runner-dmz-02", false},
		{"the dispatch that runner redeemed, which nothing came before", first, redeemed, "runner-dmz-01", false},
		{"a runner bound to the dispatch before without redeeming it", second, boundRequeue, "runner-dmz-03", false},
		{"the requeue under the key of another attempt", second, requeue, "runner-dmz-01", false},
		{"a dispatch before the one that runner redeemed", third, unredeemed, "runner-dmz-04", false},
		{"a row that is not an identifier at all", first, "not a ulid; drop table tasks", "runner-dmz-01", false},
		{"no runner", first, requeue, "", false},
	} {
		var got bool
		if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			var err error
			got, err = w.RedeemedBefore(ctx, "finance", c.key, c.row, c.runner)
			return err
		}); err != nil {
			t.Errorf("%s: %s", c.why, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s answered %v", c.why, got)
		}
	}
}

// A redemption is checked, answered and only then bound, so the check has to refuse everything the
// binding refuses and bind nothing: a runner told it may have the task, and then refused an answer,
// has not taken it.
func TestAGrantIsCheckedWithoutBindingItsTask(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'render')`, financeRun); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	const row = "01M2GHAAAAAAAAAAAAAAAAAAAA"
	key := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	if _, err := conn.Exec(ctx, `
		insert into tasks (namespace, id, run_id, step, attempt, state)
		values ('finance', $1, $2, 'render', 1, 'dispatched')`, row, financeRun); err != nil {
		t.Fatalf("seeding the task: %s", err)
	}

	now := time.Now().UTC()
	scope := GrantScope{Run: financeRun, Step: "render", Secrets: []GrantSecret{{Name: "billing", Mount: "/agk/secrets/billing"}}}
	var granted Granted
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", key, row, scope, now.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	checked := func(clear string, key agk.TaskID, runner string, at time.Time) (Redeemed, error) {
		var got Redeemed
		err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
			var err error
			got, err = w.Redeemable(ctx, clear, key, runner, at)
			return err
		})
		return got, err
	}
	bound := func() string {
		t.Helper()
		var runner, redeemed *string
		if err := conn.QueryRow(ctx, `
			select t.runner, g.redeemed_at::text from tasks t join task_grants g on g.task_id = t.id
			where t.id = $1`, row).Scan(&runner, &redeemed); err != nil {
			t.Fatal(err)
		}
		switch {
		case runner != nil:
			return *runner
		case redeemed != nil:
			return "nobody, redeemed at " + *redeemed
		}
		return ""
	}

	// It answers what Redeem would, and twice, since nothing it does changes what it reads.
	for range 2 {
		got, err := checked(granted.Clear, key, "runner-dmz-01", now)
		if err != nil {
			t.Fatalf("a grant a redemption would take was refused: %s", err)
		}
		if got.Namespace != "finance" || got.Row != row || got.Task != key || !slices.Equal(got.Scope.Secrets, scope.Secrets) {
			t.Errorf("the check answered %+v", got)
		}
		if holder := bound(); holder != "" {
			t.Fatalf("checking a grant bound its task to %s", holder)
		}
	}

	// And it refuses what Redeem refuses, for the same reasons.
	for _, c := range []struct {
		why    string
		clear  string
		key    agk.TaskID
		runner string
		at     time.Time
		want   error
	}{
		{"a value that opens nothing", granted.Clear + "x", key, "runner-dmz-01", now, ErrNoGrant},
		{"the key of another attempt", granted.Clear, agk.NewTaskID(financeRun, "render", 2, agk.Shard{}), "runner-dmz-01", now, ErrNoGrant},
		{"a grant past its expiry", granted.Clear, key, "runner-dmz-01", granted.ExpiresAt, ErrNoGrant},
	} {
		if _, err := checked(c.clear, c.key, c.runner, c.at); !errors.Is(err, c.want) {
			t.Errorf("checking %s answered %v", c.why, err)
		}
	}

	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, granted.Clear, key, "runner-dmz-01", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := checked(granted.Clear, key, "runner-dmz-02", now); !errors.Is(err, ErrTaskHeld) {
		t.Errorf("checking a grant another runner redeemed answered %v", err)
	}
	if _, err := checked(granted.Clear, key, "runner-dmz-01", now); err != nil {
		t.Errorf("checking a grant again for the runner that holds its task answered %v", err)
	}
	if _, err := conn.Exec(ctx, `update tasks set state = 'succeeded' where id = $1`, row); err != nil {
		t.Fatal(err)
	}
	if _, err := checked(granted.Clear, key, "runner-dmz-01", now); !errors.Is(err, ErrTaskHeld) {
		t.Errorf("checking the grant of a task that has ended answered %v", err)
	}
}

// "A redemption by a draining or revoked runner gets 403, binds nothing": both take nothing new,
// checked and bound alike, and the task stays free for a runner that does. What a runner already
// holds is not new, and it redeems that again once drained or revoked, to finish it.
func TestADrainingOrRevokedRunnerRedeemsNothing(t *testing.T) {
	pool, super := joining(t)
	ctx := t.Context()
	now := time.Now().UTC()
	draining := joinedWith(t, pool, privateKey(1), time.Hour, now)
	revoked := joinedWith(t, pool, privateKey(2), time.Hour, now)
	ready := joinedWith(t, pool, privateKey(3), time.Hour, now)
	if err := pool.Installation(ctx, RunnerInventory, func(ctx context.Context, w *Wide) error {
		if _, err := w.Drain(ctx, draining.Runner, "admin", "the host is being retired", now); err != nil {
			return err
		}
		_, err := w.Revoke(ctx, revoked.Runner, "admin", "the credential leaked", now, time.Hour)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	const row = "01M2GHAAAAAAAAAAAAAAAAAAAA"
	key := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	if _, err := conn.Exec(ctx, `
		insert into tasks (namespace, id, run_id, step, attempt, state)
		values ('finance', $1, $2, 'render', 1, 'dispatched')`, row, financeRun); err != nil {
		t.Fatalf("seeding the task: %s", err)
	}
	var clear string
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		granted, err := w.IssueGrant(ctx, "finance", key, row, GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
		clear = granted.Clear
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, runner := range []string{draining.Runner, revoked.Runner} {
		if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
			if _, err := w.Redeemable(ctx, clear, key, runner, now); !errors.Is(err, ErrRunnerNotTaking) {
				t.Errorf("checking a redemption by %s answered %v", runner, err)
			}
			if _, err := w.Redeem(ctx, clear, key, runner, now); !errors.Is(err, ErrRunnerNotTaking) {
				t.Errorf("a redemption by %s answered %v", runner, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var holder *string
	if err := conn.QueryRow(ctx, `select runner from tasks where id = $1`, row).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder != nil {
		t.Fatalf("a refused redemption bound the task to %s", *holder)
	}

	// The grant was good all along, and a runner that takes work takes it.
	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, clear, key, ready.Runner, now)
		return err
	}); err != nil {
		t.Errorf("a ready runner's redemption of the same grant answered %v", err)
	}

	// Drained, and then revoked, it redeems what it holds again, as it does after a lost answer
	// or a restart, and the task stays its own.
	for _, order := range []func(ctx context.Context, w *Wide) error{
		func(ctx context.Context, w *Wide) error {
			_, err := w.Drain(ctx, ready.Runner, "admin", "the host is being retired", now)
			return err
		},
		func(ctx context.Context, w *Wide) error {
			_, err := w.Revoke(ctx, ready.Runner, "admin", "the credential leaked", now, time.Hour)
			return err
		},
	} {
		if err := pool.Installation(ctx, RunnerInventory, order); err != nil {
			t.Fatal(err)
		}
		if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
			if _, err := w.Redeemable(ctx, clear, key, ready.Runner, now); err != nil {
				return err
			}
			_, err := w.Redeem(ctx, clear, key, ready.Runner, now)
			return err
		}); err != nil {
			t.Errorf("the holder's redemption once withdrawn answered %v", err)
		}
	}
	if err := conn.QueryRow(ctx, `select runner from tasks where id = $1`, row).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder == nil || *holder != ready.Runner {
		t.Errorf("the task is held by %v, and was %s's", holder, ready.Runner)
	}
}

// "The API checks the pool's namespaces again at the redemption (422)", and a host's own narrowing
// as well (403), each before anything binds, checked and bound alike. The runner that holds the task
// already passed them when it bound it, and is not asked again: a runner's standing decides who
// takes a task, and a task taken is its holder's to finish.
func TestARedemptionIsHeldToThePoolAndTheHostsNamespaces(t *testing.T) {
	pool, super := joining(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := pool.Installation(ctx, RunnerInventory, func(ctx context.Context, w *Wide) error {
		if err := w.CreateRunnerPool(ctx, RunnerPool{Name: "ops", AcceptedNamespaces: []string{"team-ops"}, CreatedBy: "admin"}); err != nil {
			return err
		}
		return w.CreateRunnerPool(ctx, RunnerPool{Name: "shared", AcceptedNamespaces: []string{"finance", "team-ops"}, CreatedBy: "admin"})
	}); err != nil {
		t.Fatal(err)
	}
	outside := joinedTo(t, pool, "ops", nil, privateKey(1), now)
	narrowed := joinedTo(t, pool, "shared", []string{"team-ops"}, privateKey(2), now)
	within := joinedTo(t, pool, "shared", []string{"finance"}, privateKey(3), now)

	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	const row = "01M2GHBBBBBBBBBBBBBBBBBBBB"
	key := agk.NewTaskID(financeRun, "render", 1, agk.Shard{})
	if _, err := conn.Exec(ctx, `
		insert into tasks (namespace, id, run_id, step, attempt, state)
		values ('finance', $1, $2, 'render', 1, 'dispatched')`, row, financeRun); err != nil {
		t.Fatalf("seeding the task: %s", err)
	}
	var clear string
	if err := pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
		granted, err := w.IssueGrant(ctx, "finance", key, row, GrantScope{Run: financeRun, Step: "render"}, now.Add(time.Hour))
		clear = granted.Clear
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		runner string
		want   error
	}{
		{outside.Runner, ErrPoolRefusesNamespace},
		{narrowed.Runner, ErrRunnerNarrowed},
	} {
		if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
			if _, err := w.Redeemable(ctx, clear, key, c.runner, now); !errors.Is(err, c.want) {
				t.Errorf("checking a redemption by %s answered %v, not %v", c.runner, err, c.want)
			}
			if _, err := w.Redeem(ctx, clear, key, c.runner, now); !errors.Is(err, c.want) {
				t.Errorf("a redemption by %s answered %v, not %v", c.runner, err, c.want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var holder *string
	if err := conn.QueryRow(ctx, `select runner from tasks where id = $1`, row).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder != nil {
		t.Fatalf("a refused redemption bound the task to %s", *holder)
	}

	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, clear, key, within.Runner, now)
		return err
	}); err != nil {
		t.Fatalf("a redemption by a runner whose pool and narrowing both keep finance answered %v", err)
	}

	// Bound, the task is its holder's, and a narrowing that leaves its namespace out now, which
	// only a hand on the database can write, does not take it back.
	if _, err := conn.Exec(ctx, `update runners set accepted_namespaces = '{team-ops}' where id = $1`, within.Runner); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(ctx, Redemption, func(ctx context.Context, w *Wide) error {
		_, err := w.Redeem(ctx, clear, key, within.Runner, now)
		return err
	}); err != nil {
		t.Errorf("the holder's redemption again answered %v", err)
	}
}

// joinedTo puts a machine in a pool, narrowed to the namespaces given where there are any.
func joinedTo(t *testing.T, pool *Pool, into string, namespaces []string, key ed25519.PrivateKey, now time.Time) Joined {
	t.Helper()
	var joined Joined
	err := pool.Installation(t.Context(), RunnerInventory, func(ctx context.Context, w *Wide) error {
		issued, err := w.IssueJoinToken(ctx, into, nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err = w.Join(ctx, Joining{
			Token: issued.Clear, PublicKey: key.Public().(ed25519.PublicKey),
			CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0", Namespaces: namespaces,
		}, time.Hour, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return joined
}
