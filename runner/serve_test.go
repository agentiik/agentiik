package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	server "github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/dockertest"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// A host serving against an installation: the real API's runner routes and object store on a real
// database, behind a server the host reaches, minting bus credentials on the shared NATS, and a pool
// of the installation's own, so that nothing another test does reaches its queue.

// served is an installation with one runner joined to its pool.
type served struct {
	url     string
	pool    *db.Pool
	conn    *pgx.Conn
	objects artifact.Objects
	signed  *artifact.Signed
	control *bus.Bus
	js      jetstream.JetStream

	poolName   string
	runner     string
	credential Secret
	run        agk.RunID

	// forward is how far the API's clock is ahead of the time of day, in nanoseconds.
	forward atomic.Int64

	// busTokens counts the bus credentials the API was asked for.
	busTokens atomic.Int32
}

// now is the API's clock.
func (in *served) now() time.Time {
	return time.Now().UTC().Add(time.Duration(in.forward.Load()))
}

// ahead moves the API's clock forward by d.
func (in *served) ahead(d time.Duration) { in.forward.Add(int64(d)) }

// heardAt is the last heartbeat that named a dispatch, by the API's clock.
func (in *served) heardAt(t *testing.T, row string) time.Time {
	t.Helper()
	var at *time.Time
	if err := in.conn.QueryRow(context.Background(),
		`select last_heartbeat_at from tasks where id = $1`, row).Scan(&at); err != nil {
		t.Error(err)
	}
	if at == nil {
		return time.Time{}
	}
	return *at
}

// swept is the controller's sweep for silence at a moment of the test's choosing, and answers how
// many tasks it declared lost.
func (in *served) swept(t *testing.T, at time.Time) int {
	t.Helper()
	var lost int
	if err := in.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		lost, err = w.Lost(ctx, at, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return lost
}

const servedCommit = "a3f9c1e"

func anInstallationServing(t *testing.T) *served {
	t.Helper()
	return anInstallationRotating(t, time.Hour, make(ed25519.PublicKey, ed25519.PublicKeySize))
}

// anInstallationRotating is an installation whose credentials are accepted for rotation, and whose
// runner joined with key.
func anInstallationRotating(t *testing.T, rotation time.Duration, key ed25519.PublicKey) *served {
	t.Helper()
	url := os.Getenv("AGENTIIK_TEST_BUS_URL")
	if url == "" {
		t.Skip("no NATS on this machine: set AGENTIIK_TEST_BUS_URL")
	}
	control, err := bus.Open(t.Context(), bus.Options{URL: url})
	if err != nil {
		t.Skipf("the bus at AGENTIIK_TEST_BUS_URL could not be reached: %s", err)
	}
	t.Cleanup(control.Close)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	ctx := t.Context()
	in := &served{pool: pool, conn: conn, control: control, js: js, run: agk.NewRunID(),
		poolName: "loop-" + strings.ToLower(ulid.New()[16:])}
	t.Cleanup(func() { js.DeleteConsumer(context.Background(), bus.Stream, bus.Durable(in.poolName)) })

	srv := httptest.NewUnstartedServer(nil)
	in.objects = artifact.Dir(t.TempDir())
	in.signed, err = artifact.NewSigned(in.objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "http://" + srv.Listener.Addr().String() + "/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := account.Seed()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := bus.NewIssuer(string(seed), url)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := server.NewRouter(server.DenyAll{}, func(*http.Request) (server.Principal, error) {
		return "", errors.New("this installation speaks to runners and to nobody else")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.NewObjects(rt, in.signed); err != nil {
		t.Fatal(err)
	}
	if _, err := server.NewRunners(rt, server.RunnerOptions{
		Pool: pool, Objects: in.objects, URLs: in.signed, BusIssuer: issuer, BusConsumers: control, Now: in.now,
		JoinRotation: rotation,
	}); err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/bus/token" {
			in.busTokens.Add(1)
		}
		rt.ServeHTTP(w, r)
	})
	srv.Start()
	t.Cleanup(srv.Close)
	in.url = srv.URL

	// The version every task runs, with a tree of one file.
	entry := []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n")
	digest := sha(entry)
	if err := in.objects.Put(ctx, artifact.Key("finance", digest), bytes.NewReader(entry)); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	if err := pool.In(ctx, "finance", func(ctx context.Context, ns *db.NS) error {
		_, err := ns.SaveVersion(ctx, db.Version{
			Workflow: "monthly-invoicing", Commit: servedCommit, Entry: "agentiik.yaml", Document: entry,
			Tree:   []db.TreeFile{{Path: "agentiik.yaml", SHA256: digest, Size: int64(len(entry)), Mode: "0644"}},
			Author: "alice",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `insert into runs (namespace, id, workflow, commit, trigger)
		values ('finance', $1, 'monthly-invoicing', $2, 'manual')`, string(in.run), servedCommit); err != nil {
		t.Fatalf("seeding: %s", err)
	}

	// The pool, and one runner of it.
	now := time.Now().UTC()
	if err := pool.Installation(ctx, db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateRunnerPool(ctx, db.RunnerPool{Name: in.poolName, Labels: []string{"zone=dmz"}, AcceptedNamespaces: []string{"finance"}, CreatedBy: "admin"}); err != nil {
			return err
		}
		token, err := w.IssueJoinToken(ctx, in.poolName, nil, "admin", now, now.Add(time.Hour))
		if err != nil {
			return err
		}
		joined, err := w.Join(ctx, db.Joining{
			Token: token.Clear, PublicKey: key, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
			Architecture: "amd64", AgentVersion: "0.2.0",
		}, rotation, now)
		in.runner, in.credential = joined.Runner, Secret(joined.Credential)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// And its queue, which the API makes ready as the pool is created, and never when a runner
	// asks for its bus credential.
	if err := control.Consumer(ctx, in.poolName); err != nil {
		t.Fatal(err)
	}
	return in
}

// dispatch hands out one task of the run, as the controller does: its row, its grant, and its
// message on the pool's queue.
func (in *served) dispatch(t *testing.T, step agk.Step) bus.TaskMessage {
	t.Helper()
	ctx := t.Context()
	row := ulid.New()
	key := agk.NewTaskID(in.run, step, 1, agk.Shard{})
	if _, err := in.conn.Exec(ctx, `insert into steps (namespace, run_id, step) values ('finance', $1, $2)`,
		string(in.run), string(step)); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	if _, err := in.conn.Exec(ctx, `insert into tasks (namespace, id, run_id, step, attempt, state, dispatched_at, published_at)
		values ('finance', $1, $2, $3, 1, 'dispatched', now(), now())`, row, string(in.run), string(step)); err != nil {
		t.Fatalf("seeding: %s", err)
	}
	deadline := time.Now().Add(time.Hour).UTC()
	var granted db.Granted
	if err := in.pool.Installation(ctx, db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", key, row, db.GrantScope{
			Run: in.run, Step: step, Workflow: "monthly-invoicing", Commit: servedCommit,
		}, deadline)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := bus.TaskMessage{
		TaskID: row, IdempotencyKey: string(key), RunID: string(in.run), Namespace: "finance",
		Workflow: "monthly-invoicing@" + servedCommit, Step: string(step), Attempt: 1,
		Image:  "ghcr.io/acme/agk-invoice@" + imageDigest,
		Params: map[string]any{}, Secrets: []bus.SecretMount{}, Inputs: []bus.Input{}, Outputs: []string{"out"},
		Resources: bus.Resources{CPU: "0.5", Memory: "256Mi", PIDs: 128},
		Network:   "none", RunsOn: []string{"zone=dmz"},
		Deadline: deadline.Format(time.RFC3339Nano), Grant: granted.Clear,
	}
	if err := in.control.Publish(ctx, in.poolName, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// queue is what the pool's consumer still means to hand out: the messages nobody has been handed
// yet, and those handed and not acknowledged.
func (in *served) queue(t *testing.T) (waiting, unacknowledged int) {
	t.Helper()
	consumer, err := in.js.Consumer(context.Background(), bus.Stream, bus.Durable(in.poolName))
	if err != nil {
		t.Error(err)
		return -1, -1
	}
	info, err := consumer.Info(context.Background())
	if err != nil {
		t.Error(err)
		return -1, -1
	}
	return int(info.NumPending), info.NumAckPending
}

// state is a dispatch as the database has it: its state, and the runner bound to it or "-".
func (in *served) state(t *testing.T, row string) string {
	t.Helper()
	var state string
	if err := in.conn.QueryRow(context.Background(),
		`select state || ' ' || coalesce(runner, '-') from tasks where id = $1`, row).Scan(&state); err != nil {
		t.Error(err)
	}
	return state
}

// lastResult is the last thing the runner published on its results subject.
func (in *served) lastResult(t *testing.T) map[string]any {
	t.Helper()
	stream, err := in.js.Stream(context.Background(), bus.Results)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := stream.GetLastMsgForSubject(context.Background(), bus.ResultSubject(in.runner))
	if err != nil {
		t.Fatalf("nothing was published on %s: %s", bus.ResultSubject(in.runner), err)
	}
	var said map[string]any
	if err := json.Unmarshal(msg.Data, &said); err != nil {
		t.Fatal(err)
	}
	return said
}

// eventually waits for cond, and fails saying what was waited on where it never holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s never happened", what)
}

// A host holding one task at once takes one, and while its container runs it asks the bus for
// nothing: the second task waits on the queue, handed to nobody, for a runner with room. Once the
// first ends, the freed slot takes the second, and each runs and is reported.
func TestAFullHostTakesNothingAndAFreedSlotTakesOne(t *testing.T) {
	in := anInstallationServing(t)
	var (
		mu       sync.Mutex
		released = map[string]chan struct{}{}
	)
	release := func(key string) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		if released[key] == nil {
			released[key] = make(chan struct{})
		}
		return released[key]
	}
	c := carrier(t, func(c dockertest.Container) (int, error) {
		<-release(c.Labels[driver.LabelTask])
		return 0, nil
	})
	client, err := NewClient(in.url, in.credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	var logged sync.Mutex
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Agent{
			Config: Config{
				API: in.url, Runner: in.runner, Pool: in.poolName, Concurrency: 1,
				WorkDir: c.root, Credential: in.credential, Labels: []string{"zone=dmz"},
			},
			Driver: c.carrier.Driver.(*driver.Docker), Client: client, Endings: c.carrier.Endings,
			Log: func(s string) { logged.Lock(); log.WriteString(s + "\n"); logged.Unlock() },
		})
	}()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %s", err)
		}
		logged.Lock()
		t.Log(log.String())
		logged.Unlock()
	}()

	first := in.dispatch(t, "invoice")
	second := in.dispatch(t, "render")
	running := func(key string) func() bool {
		return func() bool {
			for _, made := range c.daemon.Created() {
				if made.Labels[driver.LabelTask] == key {
					return true
				}
			}
			return false
		}
	}
	eventually(t, "the first task's container starting", running(first.IdempotencyKey))

	// Full: for as long as a take would have had its answer, nothing more is taken.
	for range 3 {
		time.Sleep(500 * time.Millisecond)
		if waiting, unacknowledged := in.queue(t); waiting != 1 || unacknowledged != 0 {
			t.Fatalf("a full host left the queue with %d waiting and %d handed out, want the second task waiting and nothing handed out", waiting, unacknowledged)
		}
	}
	if running(second.IdempotencyKey)() {
		t.Fatal("a full host started a second container")
	}
	if got, want := in.state(t, first.TaskID), "dispatched "+in.runner; got != want {
		t.Errorf("the first task reads %q, want %q: redeemed, and so bound to the runner", got, want)
	}

	close(release(first.IdempotencyKey))
	eventually(t, "the second task's container starting once the first ended", running(second.IdempotencyKey))
	close(release(second.IdempotencyKey))
	eventually(t, "the second task's result", func() bool {
		said := in.lastResult(t)
		return said["task_id"] == second.TaskID && said["state"] == "succeeded"
	})
	if waiting, unacknowledged := in.queue(t); waiting+unacknowledged != 0 {
		t.Errorf("both tasks ran and the queue still holds %d waiting and %d handed out", waiting, unacknowledged)
	}
	if left, _ := os.ReadDir(filepath.Join(c.root, ResultsDir)); len(left) != 0 {
		t.Errorf("results are still kept under the work root once published: %v", left)
	}
}
