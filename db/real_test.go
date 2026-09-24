package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/jackc/pgx/v5"
)

// The half of this package that only a real PostgreSQL can hold it to.
//
// Everything here skips when AGENTIIK_TEST_DATABASE_URL is unset and runs when it is,
// exactly as the driver's tests skip without a Docker daemon, so that continuous
// integration stays green on a machine with nothing installed and this machine tests for
// real. The variable names a superuser, because the tests create the schema and the
// unprivileged role the application uses; what is under test is what that role can see.
//
//	docker run -d --name agk-pg -e POSTGRES_PASSWORD=agk -e POSTGRES_DB=agk \
//	  -p 55432:5432 postgres:17-alpine
//	AGENTIIK_TEST_DATABASE_URL=postgres://postgres:agk@127.0.0.1:55432/agk go test ./db/

// database prepares a schema of this test's own, with the application role, and answers
// with the two addresses: the superuser's, for migrating, and the application's, which is
// the one the package is meant to be opened with.
//
// The role is the one Provision creates, as an installation's migrate step creates it, so
// every namespace check in this package is also a check of what Provision grants.
func database(t *testing.T) (super string, app string) {
	t.Helper()
	super, role := blank(t)

	ctx := t.Context()
	sc, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close(ctx)
	if _, err := Provision(ctx, sc, role, "test"); err != nil {
		t.Fatalf("the database could not be provisioned: %s", err)
	}
	return super, withCredentials(super, role, "test")
}

// blank prepares a database of this test's own and nothing in it, and answers with the
// superuser's address on it and the name of the role that goes with it, which nothing has
// created yet.
func blank(t *testing.T) (super string, role string) {
	t.Helper()
	url := os.Getenv("AGENTIIK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("no PostgreSQL on this machine: set AGENTIIK_TEST_DATABASE_URL")
	}

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Skipf("the database at AGENTIIK_TEST_DATABASE_URL could not be reached: %s", err)
	}
	defer conn.Close(ctx)

	// One database per test, so that two tests cannot see each other's rows and a
	// failure leaves something a person can open afterwards.
	name := "agk_" + strings.ToLower(strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}
	// A role left by a run that never tidied up goes too, once the database that granted it
	// something has, so that the role a test starts from is one it created.
	for _, stmt := range []string{
		fmt.Sprintf(`drop database if exists %s with (force)`, name),
		`drop role if exists ` + name,
		fmt.Sprintf(`create database %s`, name),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	// The role goes with the database and is named after it. A role is a property of the
	// cluster rather than of a database, so one role shared by every test is a role that
	// two packages testing at once each drop while the other is using it. Cleanups run in
	// the reverse of the order they are registered, and a role cannot be dropped while a
	// database still grants it anything, so the role's is registered first and runs last.
	t.Cleanup(func() { drop(ctx, url, `drop role if exists `+name) })
	t.Cleanup(func() {
		drop(ctx, url, fmt.Sprintf(`drop database if exists %s with (force)`, name))
	})
	return withDatabase(url, name), name
}

// drop runs one tidying statement and says nothing if it cannot: a test that has finished is
// not made to fail by a cluster that is already gone.
func drop(ctx context.Context, url, stmt string) {
	ctx = context.WithoutCancel(ctx)
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	c.Exec(ctx, stmt)
}

func withDatabase(url, name string) string {
	if i := strings.LastIndex(url, "/"); i > 0 {
		if j := strings.Index(url[i:], "?"); j > 0 {
			return url[:i+1] + name + url[i+j:]
		}
		return url[:i+1] + name
	}
	return url
}

func withCredentials(url, user, password string) string {
	i := strings.Index(url, "://")
	rest := url[i+3:]
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return url[:i+3] + user + ":" + password + "@" + rest
}

// seed writes two namespaces of identical shape, which is what makes "one namespace cannot
// see another" a question with a wrong answer available.
func seed(t *testing.T, super string) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance'), ('team-ops')`,
		`insert into workflows (namespace, name) values ('finance','monthly-invoicing'), ('team-ops','nightly')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at)
		   values ('finance','monthly-invoicing','a3f9c1e','{}','alice', now()),
		          ('team-ops','nightly','b1c2d3e','{}','bob', now())`,
		`insert into runs (namespace, id, workflow, commit, trigger)
		   values ('finance','01JMZ8V1P9C4XQ7K2N4D6F8H0A','monthly-invoicing','a3f9c1e','manual'),
		          ('team-ops','01M2AAZ9G62NQXFAFCXKRPJEH5','nightly','b1c2d3e','schedule')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
}

// The sentence this whole package exists for, executed: with no namespace bound, a read
// returns nothing. Not an error, not another tenant's rows. Nothing.
func TestAQueryThatCarriesNoNamespaceReadsNothing(t *testing.T) {
	super, app := database(t)
	seed(t, super)

	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Reaching the table without a door is what this package makes unexpressible, so
	// the test reaches for it the only way anything can: through a door, and then
	// unbinding what the door bound.
	var count int
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.tx.Exec(ctx, `select set_config('agentiik.namespace', '', true)`); err != nil {
			return err
		}
		return ns.tx.QueryRow(ctx, `select count(*) from runs`).Scan(&count)
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a query with no namespace bound read %d rows, and the promise is that it reads none", count)
	}
}

// One namespace sees its own rows and not the other's, which is the same property from the
// side somebody actually uses.
func TestOneNamespaceCannotSeeAnother(t *testing.T) {
	super, app := database(t)
	seed(t, super)

	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	for _, c := range []struct{ namespace, workflow string }{
		{"finance", "monthly-invoicing"},
		{"team-ops", "nightly"},
	} {
		var count int
		var workflow string
		err := pool.In(t.Context(), c.namespace, func(ctx context.Context, ns *NS) error {
			// No namespace in the statement. That is the point: the policy is what
			// filters, so a query somebody forgot to filter is filtered anyway.
			return ns.tx.QueryRow(ctx, `select count(*), min(workflow) from runs`).Scan(&count, &workflow)
		})
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 || workflow != c.workflow {
			t.Errorf("%s saw %d runs, the first being %q", c.namespace, count, workflow)
		}
	}
}

// A write cannot land in a namespace the caller is not in, which the reading half alone
// would not give: a handle that can only read its own rows but write anywhere is a handle
// that can plant one.
func TestAWriteCannotLandInAnotherNamespace(t *testing.T) {
	super, app := database(t)
	seed(t, super)

	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.tx.Exec(ctx,
			`insert into runs (namespace, id, workflow, commit, trigger)
			 values ('team-ops','01M2BBBBBBBBBBBBBBBBBBBBBB','nightly','b1c2d3e','manual')`)
		return err
	})
	if err == nil {
		t.Fatal("a row was written into another namespace")
	}
	if !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("the write was refused, and not by the policy: %s", err)
	}
}

// The door for what legitimately has no namespace. Without it the controller's sweep, the
// three purges and the collector would read an empty database while every namespaced test
// passed, which is the failure the reading of this design nearly shipped.
func TestTheInstallationDoorSeesEveryNamespace(t *testing.T) {
	super, app := database(t)
	seed(t, super)

	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var count int
	err = pool.Installation(t.Context(), ControllerSweep, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx, `select count(*) from runs`).Scan(&count)
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("the sweep saw %d runs of the two that exist, so the controller would be scheduling against an empty database", count)
	}

	// And it insists on being told why.
	if err := pool.Installation(t.Context(), "", func(context.Context, *Wide) error { return nil }); err == nil {
		t.Error("the installation door opened with no reason given")
	}
}

// What binds the namespace lasts exactly as long as the transaction. A pooled connection
// carrying one into the next caller would be the same bug as a forgotten filter, arriving
// by a route nobody would look down.
func TestTheNamespaceDoesNotOutliveItsTransaction(t *testing.T) {
	super, app := database(t)
	seed(t, super)

	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := pool.In(t.Context(), "team-ops", func(context.Context, *NS) error { return nil }); err != nil {
		t.Fatal(err)
	}

	// The next door onto the same pool, and very likely the same connection, is the
	// installation one: if the previous namespace were still bound it would see one row
	// rather than two.
	var count int
	err = pool.Installation(t.Context(), Purge, func(ctx context.Context, w *Wide) error {
		return w.tx.QueryRow(ctx, `select count(*) from runs`).Scan(&count)
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("a later transaction saw %d runs, so what one bound survived into another", count)
	}
}

// A connection that walks through the policies makes every test above pass and the
// property false, so it is refused where it can still be caught.
func TestOpenRefusesAConnectionThePolicyDoesNotApplyTo(t *testing.T) {
	super, _ := database(t)

	_, err := Open(t.Context(), super)
	if err == nil {
		t.Fatal("a superuser connection was accepted, and row level security does not apply to one")
	}
	for _, want := range []string{"NOSUPERUSER", "enforced nowhere"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %s", want, err)
		}
	}
}

// An installation is upgraded rather than rebuilt, so applying the schema twice is not an
// error and the second time does nothing.
func TestMigratingTwiceAppliesNothingTheSecondTime(t *testing.T) {
	super, _ := database(t)

	ctx := t.Context()
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// database() already migrated once, so this is the second and third times.
	again, err := Migrate(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a second migration applied %v", again)
	}
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var recorded int
	if err := conn.QueryRow(ctx, `select count(*) from schema_migrations`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != len(all) {
		t.Fatalf("%d migrations are recorded and %d are embedded", recorded, len(all))
	}
}

// The key a runner refuses a second container on is computed by the database from the four
// columns that already say it, so a writer cannot get it wrong and two writers cannot
// disagree.
func TestTheIdempotencyKeyIsComputedAndUnique(t *testing.T) {
	super, app := database(t)
	seed(t, super)

	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	const run agk.RunID = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
	var fanned, alone string
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.tx.Exec(ctx,
			`insert into steps (namespace, run_id, step) values ('finance', $1, 'invoice')`, run); err != nil {
			return err
		}
		if _, err := ns.tx.Exec(ctx,
			`insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of)
			 values ('finance','01M2CCCCCCCCCCCCCCCCCCCCCC',$1,'invoice',2,3,8)`, run); err != nil {
			return err
		}
		if _, err := ns.tx.Exec(ctx,
			`insert into tasks (namespace, id, run_id, step, attempt)
			 values ('finance','01M2DDDDDDDDDDDDDDDDDDDDDD',$1,'invoice',1)`, run); err != nil {
			return err
		}
		if err := ns.tx.QueryRow(ctx,
			`select idempotency_key from tasks where attempt = 2`).Scan(&fanned); err != nil {
			return err
		}
		return ns.tx.QueryRow(ctx,
			`select idempotency_key from tasks where attempt = 1`).Scan(&alone)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Compared against what agk mints rather than against a literal, because the point of
	// the column is that it equals the identifier on the wire and a literal here would let
	// the two drift while both tests stayed green.
	if want := string(agk.NewTaskID(run, "invoice", 2, agk.Shard{Index: 3, Of: 8})); fanned != want {
		t.Errorf("a fanned out task's key is %q and agk mints %q", fanned, want)
	}
	if want := string(agk.NewTaskID(run, "invoice", 1, agk.Shard{})); alone != want {
		t.Errorf("a task with no shard has key %q and agk mints %q", alone, want)
	}

	// The same unit of work twice is refused, which is what makes at-least-once
	// delivery survivable, and it is refused as a second dispatch too: a key has one row
	// that is not lost.
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.tx.Exec(ctx,
			`insert into tasks (namespace, id, run_id, step, attempt, shard_index, shard_of, requeue)
			 values ('finance','01M2EEEEEEEEEEEEEEEEEEEEEE',$1,'invoice',2,3,8,1)`, run)
		return err
	})
	if err == nil {
		t.Fatal("one unit of work was written twice")
	}
	if !strings.Contains(err.Error(), "tasks_by_idempotency_key") {
		t.Fatalf("the second write was refused, and not by the uniqueness rule: %s", err)
	}
}

// "A requeue after loss keeps the idempotency key and takes a new task_id." Once the dispatch a
// key had is lost, the key takes another, and each dispatch is written once.
func TestALostTaskTakesANewRowUnderItsKey(t *testing.T) {
	super, app := database(t)
	seed(t, super)
	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	const run agk.RunID = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
	insert := func(ctx context.Context, ns *NS, id string, requeue int) error {
		_, err := ns.tx.Exec(ctx,
			`insert into tasks (namespace, id, run_id, step, attempt, requeue, state)
			 values ('finance', $1, $2, 'invoice', 1, $3, 'dispatched')`, id, run, requeue)
		return err
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.tx.Exec(ctx,
			`insert into steps (namespace, run_id, step) values ('finance', $1, 'invoice')`, run); err != nil {
			return err
		}
		if err := insert(ctx, ns, "01M2HAAAAAAAAAAAAAAAAAAAAA", 0); err != nil {
			return err
		}
		if _, err := ns.tx.Exec(ctx,
			`update tasks set state = 'lost' where id = '01M2HAAAAAAAAAAAAAAAAAAAAA'`); err != nil {
			return err
		}
		return insert(ctx, ns, "01M2HBBBBBBBBBBBBBBBBBBBBB", 1)
	}); err != nil {
		t.Fatalf("a lost task could not be handed out again under its key: %s", err)
	}

	var keys []string
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		rows, err := ns.tx.Query(ctx, `select idempotency_key from tasks where run_id = $1 order by requeue`, run)
		if err != nil {
			return err
		}
		keys, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := string(agk.NewTaskID(run, "invoice", 1, agk.Shard{}))
	if len(keys) != 2 || keys[0] != want || keys[1] != want {
		t.Errorf("the two dispatches carry the keys %v, want %s twice", keys, want)
	}

	// The second dispatch lost in its turn, a third written as the second again is refused:
	// each dispatch of a key exists once.
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		if _, err := ns.tx.Exec(ctx,
			`update tasks set state = 'lost' where id = '01M2HBBBBBBBBBBBBBBBBBBBBB'`); err != nil {
			return err
		}
		return insert(ctx, ns, "01M2HCCCCCCCCCCCCCCCCCCCCC", 1)
	})
	if err == nil {
		t.Fatal("one dispatch of a key was written twice")
	}
	if !strings.Contains(err.Error(), "tasks_by_dispatch") {
		t.Fatalf("the second write of a dispatch was refused, and not by the rule for dispatches: %s", err)
	}
}

// An exit code is read off a container that decided something. A task stopped at its
// deadline or by a cancellation decided nothing, and a row claiming both would be a row
// the retry policy reads backwards.
func TestOnlyATaskThatRanCarriesAnExitCode(t *testing.T) {
	super, app := database(t)
	seed(t, super)

	pool, err := Open(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	const run = "01JMZ8V1P9C4XQ7K2N4D6F8H0A"
	err = pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
		_, err := ns.tx.Exec(ctx,
			`insert into steps (namespace, run_id, step) values ('finance', $1, 'invoice')`, run)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	for i, c := range []struct {
		state   string
		code    int
		allowed bool
	}{
		{"failed", 108, true},
		{"succeeded", 0, true},
		{"timed_out", 137, false},
		{"cancelled", 143, false},
		{"lost", 1, false},
	} {
		// A task identifier of the right alphabet, one per case, so that a refusal is
		// the check constraint and never a collision on the primary key.
		id := fmt.Sprintf("01M2F%021d", i)
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *NS) error {
			_, err := ns.tx.Exec(ctx,
				`insert into tasks (namespace, id, run_id, step, attempt, state, exit_code)
				 values ('finance', $1, $2, 'invoice', $3, $4, $5)`,
				id, run, i+1, c.state, c.code)
			return err
		})
		if c.allowed && err != nil {
			t.Errorf("a %s task could not carry an exit code: %s", c.state, err)
		}
		if !c.allowed && err == nil {
			t.Errorf("a %s task carried exit code %d, and nothing decided it", c.state, c.code)
		}
	}
}

// A transaction whose context ends between two statements, on a connection that has stopped
// answering, still returns. The statement after the end refuses the context before it writes
// anything, so the connection is left alive, inside the transaction and silent, and the rollback
// owed on it waits for an answer that is not coming. Unbounded, that wait is as long as TCP's: a
// controller asked to stop in the middle of a sweep while cut off from its database stayed there.
func TestARollbackNothingAnswersIsGivenUp(t *testing.T) {
	_, app := database(t)
	u, err := url.Parse(app)
	if err != nil {
		t.Fatal(err)
	}
	cut := silence(t, u.Host)
	u.Host = cut.listener.Addr().String()

	pool, err := Open(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Answering again first, so that closing the pool does not wait on the silence as well.
	t.Cleanup(func() { cut.silent.Store(false) })

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	returned := make(chan error, 1)
	go func() {
		returned <- pool.Installation(ctx, ControllerSweep, func(ctx context.Context, w *Wide) error {
			cut.silent.Store(true)
			stop()
			return w.tx.QueryRow(ctx, `select 1`).Scan(new(int))
		})
	}()

	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the transaction ended with %v, and it was its context that ended", err)
		}
	case <-time.After(rollbackWithin + 10*time.Second):
		t.Fatalf("the transaction had not returned %s after its context ended on a connection nothing answers on", rollbackWithin+10*time.Second)
	}
}

// cutOff stands between a pool and PostgreSQL and, once silent, forwards nothing either way while
// keeping every connection open: a network that stopped answering without a reset.
type cutOff struct {
	listener net.Listener
	silent   atomic.Bool
}

func silence(t *testing.T, target string) *cutOff {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &cutOff{listener: l}
	var conns sync.WaitGroup
	t.Cleanup(func() {
		l.Close()
		c.silent.Store(false)
		conns.Wait()
	})
	go func() {
		for {
			near, err := l.Accept()
			if err != nil {
				return
			}
			far, err := net.Dial("tcp", target)
			if err != nil {
				near.Close()
				continue
			}
			conns.Add(2)
			relay := func(to, from net.Conn) {
				defer conns.Done()
				defer to.Close()
				defer from.Close()
				buf := make([]byte, 32<<10)
				for {
					n, err := from.Read(buf)
					for c.silent.Load() {
						time.Sleep(10 * time.Millisecond)
					}
					if n > 0 {
						if _, err := to.Write(buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}
			go relay(far, near)
			go relay(near, far)
		}
	}()
	return c
}
