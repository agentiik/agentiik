package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/bus/control"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/bustest"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/stopsignal"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/version"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// The program started twice against a real PostgreSQL and a real NATS, as two processes, since
// what is under test is what happens when one of them dies: a process killed releases nothing
// itself, and the lock frees only because the database notices its session has ended.
//
// A process is this test binary started again, which TestMain turns into the controller. It is
// handed its configuration rather than reading it, because the test's database and bus speak
// plaintext and config.ReadController refuses both, as it should: what reading refuses is
// internal/config's to test, and config_test.go here holds that what it reads reaches the core.
//
// The bus is a server of the test's own, for the reason package bus/control gives: package bus
// empties the shared one before each of its tests. It trusts one operator and one account, as an
// installation's does, so a controller that forgot to present the control plane's credential
// would be refused rather than let in.

// instanceVariable holds the configuration a process of this test binary runs the controller on,
// and is how TestMain knows it is one.
const instanceVariable = "AGENTIIK_CONTROLLER_TEST_INSTANCE"

// slowStopVariable makes a process of this test binary one that takes the first signal as the
// controller does and then never finishes stopping.
const slowStopVariable = "AGENTIIK_CONTROLLER_TEST_SLOW_STOP"

func TestMain(m *testing.M) {
	if raw := os.Getenv(instanceVariable); raw != "" {
		os.Exit(instance(raw))
	}
	if os.Getenv(slowStopVariable) != "" {
		ctx, _ := stopsignal.Context()
		fmt.Println("waiting")
		<-ctx.Done()
		fmt.Println("stopping")
		select {}
	}
	os.Exit(m.Run())
}

// instanceConfig is what a test hands a process of its own.
type instanceConfig struct {
	Database, Bus, JWT, Seed, Objects string

	// MetricsListen and MetricsTokenHash are where the metrics are answered and to whom, and
	// empty for an instance that answers none.
	MetricsListen, MetricsTokenHash string
}

// instance is the controller, as main runs it once the configuration is read: started the one way
// main starts it, and handed what reading would have given.
func instance(raw string) int {
	var s instanceConfig
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		fmt.Fprintf(os.Stderr, "the configuration a test handed over could not be read: %s\n", err)
		return exitUsage
	}
	return untilSignalled(func(ctx context.Context) int {
		return start(ctx, config.Controller{
			Database:    config.Database{URL: s.Database},
			Bus:         config.Bus{URL: s.Bus, JWT: s.JWT, Seed: config.Secret(s.Seed)},
			Objects:     s.Objects,
			MaxRequeues: graph.DefaultMaxRequeues,
			TaskCeiling: config.DefaultTaskCeiling,
			Metrics:     config.Metrics{Listen: s.MetricsListen, TokenHash: s.MetricsTokenHash},
		}, os.Stderr)
	})
}

// The workflow every run of these tests is of: normalize is ready at once and archive waits on
// it, so a first pass publishes one task. normalize may be retried after a loss, which is what a
// requeue needs.
const (
	theImage = "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"

	theWorkflow = `
apiVersion: agentiik.dev/v1
kind: Workflow
metadata: { name: monthly-invoicing, namespace: finance }
inputs:
  orders: { schema: { type: array } }
outputs:
  invoices: { from: { step: archive, port: ok } }
steps:
  normalize:
    image: ` + theImage + `
    retry: { max: 1, on: [lost, failed] }
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs: [ok, rejected]
  archive:
    image: ` + theImage + `
    needs:
      - { step: normalize, port: ok, as: orders }
    outputs: [ok]
`

	theManifest = `
apiVersion: agentiik.dev/v1
kind: Brick
metadata: { name: invoice, version: 1.0.0 }
spec:
  inputs:
    orders: {}
  outputs:
    ok: {}
    rejected: {}
  runtime: { user: "65532:65532" }
`

	// goodCommit is a version that builds, and brokenCommit one whose entry point is not a
	// workflow at all, so a run of it cannot be decided.
	goodCommit   = "a3f9c1e"
	brokenCommit = "b4e0d2f"
)

// seeded stores both versions of finance/monthly-invoicing, through the application role as a
// push stores them.
func seeded(t *testing.T, pool *db.Pool, super string) {
	t.Helper()
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.SaveWorkflow(ctx, "monthly-invoicing", "main"); err != nil {
			return err
		}
		for commit, document := range map[string]string{goodCommit: theWorkflow, brokenCommit: "steps: [this is not a workflow"} {
			if _, err := ns.SaveVersion(ctx, db.Version{
				Workflow: "monthly-invoicing", Commit: commit, Entry: "agentiik.yaml", Document: []byte(document),
				Manifests: map[string][]byte{theImage: []byte(theManifest)},
				Author:    "alice",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// started creates a run of commit and notifies the controller, in one transaction as the API
// does.
func started(t *testing.T, pool *db.Pool, commit string) agk.RunID {
	t.Helper()
	run := agk.NewRunID()
	err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
		if err := ns.CreateRun(ctx, db.NewRun{
			ID: run, Workflow: "monthly-invoicing", Commit: commit,
			Trigger: agk.TriggerManual, TriggeredBy: "alice",
			Inputs: json.RawMessage(`{"orders": [{"customer_id": "C-1042"}]}`),
			Steps:  []agk.Step{"normalize", "archive"},
		}); err != nil {
			return err
		}
		return ns.NotifyRun(ctx, run)
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// installationBus is a NATS server trusting one operator and one account, and a way to mint the
// control plane's credential on it.
type installationBus struct {
	url    string
	issuer *bus.Issuer
}

func withInstallationBus(t *testing.T) installationBus {
	t.Helper()
	operator, err := nkeys.CreateOperator()
	if err != nil {
		t.Fatal(err)
	}
	operatorPublic, _ := operator.PublicKey()

	account, _ := nkeys.CreateAccount()
	accountPublic, _ := account.PublicKey()
	accountSeed, _ := account.Seed()
	claims := jwt.NewAccountClaims(accountPublic)
	claims.Name = "agentiik"
	claims.Limits.JetStreamLimits.DiskStorage = -1
	claims.Limits.JetStreamLimits.MemoryStorage = -1
	accountJWT, err := claims.Encode(operator)
	if err != nil {
		t.Fatal(err)
	}

	// JetStream refuses to start without a system account.
	system, _ := nkeys.CreateAccount()
	systemPublic, _ := system.PublicKey()
	systemClaims := jwt.NewAccountClaims(systemPublic)
	systemClaims.Name = "SYS"
	systemJWT, err := systemClaims.Encode(operator)
	if err != nil {
		t.Fatal(err)
	}

	operatorClaims := jwt.NewOperatorClaims(operatorPublic)
	operatorClaims.Name = "agentiik"
	operatorClaims.SystemAccount = systemPublic
	operatorJWT, err := operatorClaims.Encode(operator)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := jwt.DecodeOperatorClaims(operatorJWT)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &natsserver.MemAccResolver{}
	for public, encoded := range map[string]string{accountPublic: accountJWT, systemPublic: systemJWT} {
		if err := resolver.Store(public, encoded); err != nil {
			t.Fatal(err)
		}
	}

	server, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: bustest.StoreDir(t),
		TrustedOperators: []*jwt.OperatorClaims{trusted},
		AccountResolver:  resolver,
		SystemAccount:    systemPublic,
		NoLog:            true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the bus did not come up")
	}
	t.Cleanup(server.Shutdown)

	issuer, err := bus.NewIssuer(string(accountSeed), server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	return installationBus{url: server.ClientURL(), issuer: issuer}
}

// controlPlane mints a credential the control plane holds, for an hour.
func (b installationBus) controlPlane(t *testing.T, name string) bus.Credentials {
	t.Helper()
	c, err := b.issuer.ForControlPlane(name, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// streams connects the test as the control plane, to read the streams the controllers write.
func (b installationBus) streams(t *testing.T) jetstream.JetStream {
	t.Helper()
	c := b.controlPlane(t, "the-test")
	conn, err := nats.Connect(b.url, nats.UserJWTAndSeed(c.JWT, c.Seed))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// held counts the messages a stream holds, and is -1 while it is not there yet.
func held(t *testing.T, js jetstream.JetStream, stream string) int {
	t.Helper()
	s, err := js.Stream(t.Context(), stream)
	if err != nil {
		return -1
	}
	info, err := s.Info(t.Context())
	if err != nil {
		return -1
	}
	return int(info.State.Msgs)
}

// process is one controller, running as a process of its own.
type process struct {
	name   string
	cmd    *exec.Cmd
	output *output
	exited chan struct{}
	err    error
}

// output is what a process wrote, kept for a failure to show.
type output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// startController starts one controller on c, and kills it when the test ends if nothing has.
func startController(t *testing.T, c instanceConfig) *process {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := &process{output: &output{}, exited: make(chan struct{})}
	p.cmd = exec.Command(self, "-test.run=^$")
	p.cmd.Env = append(os.Environ(), instanceVariable+"="+string(raw))
	p.cmd.Stdout, p.cmd.Stderr = p.output, p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.name = instanceName(p.cmd.Process.Pid)
	go func() {
		p.err = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		p.cmd.Process.Kill()
		<-p.exited
	})
	return p
}

// running says whether the process is still up.
func (p *process) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// term is the controller term as the database holds it.
type term struct {
	token  int64
	holder string
}

func termOf(t *testing.T, conn *pgx.Conn) term {
	t.Helper()
	var tm term
	var holder *string
	if err := conn.QueryRow(t.Context(), `select token, holder from controller_term`).Scan(&tm.token, &holder); err != nil {
		t.Fatal(err)
	}
	if holder != nil {
		tm.holder = *holder
	}
	return tm
}

// advisoryLocks counts the advisory locks granted in the test's database, which is the one lock
// the active controller holds, or none.
func advisoryLocks(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(t.Context(),
		`select count(*) from pg_locks where locktype = 'advisory' and granted
		 and database = (select oid from pg_database where datname = current_database())`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// eventually waits up to a deadline for ok, and fails naming what with both processes' output.
func eventually(t *testing.T, within time.Duration, what string, ok func() bool, ps ...*process) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			var out strings.Builder
			for _, p := range ps {
				fmt.Fprintf(&out, "\n%s wrote:\n%s", p.name, p.output)
			}
			t.Fatalf("%s did not happen within %s%s", what, within, out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Two instances of the program, one database and one bus. One takes the lock and the term, and
// the other stands by without taking either. The one leading decides the runs it is told of,
// publishing a task per ready step, and takes results off the bus; a run it cannot decide is
// reported and does not end its term. Killed, it releases nothing, and the database notices its
// session has gone: the other takes the lock and a new term, and decides the next run. Stopped
// with SIGTERM, it lets the lock go and exits 0.
func TestOfTwoControllersOneLeadsAndTheOtherTakesOverWhenTheFirstIsKilled(t *testing.T) {
	super := dbtest.Migrated(t)
	pool, err := db.Open(t.Context(), dbtest.Application(super))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	seeded(t, pool, super)
	conn := dbtest.Superuser(t, super)

	b := withInstallationBus(t)
	js := b.streams(t)
	credential := b.controlPlane(t, "agentiik-controller")
	c := instanceConfig{
		Database: dbtest.Application(super), Bus: b.url,
		JWT: credential.JWT, Seed: credential.Seed, Objects: t.TempDir(),
	}
	first, second := startController(t, c), startController(t, c)
	both := []*process{first, second}

	// One leads.
	var leader, standby *process
	eventually(t, 30*time.Second, "a controller leading", func() bool {
		switch termOf(t, conn).holder {
		case first.name:
			leader, standby = first, second
		case second.name:
			leader, standby = second, first
		}
		return leader != nil
	}, both...)
	began := termOf(t, conn)
	eventually(t, 10*time.Second, "the other standing by", func() bool {
		return strings.Contains(standby.output.String(), "standing by for the lock")
	}, both...)

	// It decides what it is told of, and a run it cannot decide ends nothing. The broken run
	// is notified first, so the good one is decided after it or not at all.
	started(t, pool, brokenCommit)
	started(t, pool, goodCommit)
	eventually(t, 30*time.Second, "the leader publishing the ready task of the run it could decide", func() bool {
		return held(t, js, bus.Stream) == 1
	}, both...)
	eventually(t, 10*time.Second, "the leader reporting the run it could not decide", func() bool {
		return strings.Contains(leader.output.String(), "could not be decided")
	}, both...)

	// It takes results off the bus. This one is not a result at all, so it is taken off the
	// queue and reported rather than delivered again, which only the consumer the leader runs
	// can do.
	if _, err := js.Publish(t.Context(), bus.ResultSubject("runner-1"), []byte("not a result")); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the leader taking the result off the bus", func() bool {
		return held(t, js, bus.Results) == 0
	}, both...)

	// And through all of it, the other only waited.
	if now := termOf(t, conn); now != began || !leader.running() || !standby.running() {
		t.Fatalf("the term went from %+v to %+v, and the leader is running: %t, the standby: %t%s%s",
			began, now, leader.running(), standby.running(), leader.output, standby.output)
	}
	if n := advisoryLocks(t, conn); n != 1 {
		t.Fatalf("%d advisory locks are held with one controller leading", n)
	}

	// Killed, the leader releases nothing, and the other takes over once the database notices.
	if err := leader.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-leader.exited
	eventually(t, 30*time.Second, "the standby taking over", func() bool {
		now := termOf(t, conn)
		return now.holder == standby.name && now.token > began.token
	}, both...)
	started(t, pool, goodCommit)
	eventually(t, 30*time.Second, "the new leader publishing the next run's ready task", func() bool {
		return held(t, js, bus.Stream) == 2
	}, both...)

	// Stopped, it lets the lock go and says it stopped as asked.
	if err := standby.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-standby.exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("the controller did not stop within 30s of SIGTERM%s", standby.output)
	}
	if standby.err != nil {
		t.Errorf("stopped with SIGTERM, the controller exited with %v%s", standby.err, standby.output)
	}
	if n := advisoryLocks(t, conn); n != 0 {
		t.Errorf("%d advisory locks are held once every controller is gone", n)
	}
}

// A term ends at the first write the fence refuses, whichever side met it. Taking results off the
// bus leaves one it could not record for another delivery and carries on, which is right for any
// other error and wrong for this one: a former holder that carried on would take every result off
// the bus, answer each with the fence's refusal, and hold them back from the controller that now
// leads until its own next sweep told it it had lost. So the term the test takes from under the
// one leading ends that one at the first answer it meets, with the sweep an hour away.
func TestATermEndsAtTheFirstAnswerTheFenceRefuses(t *testing.T) {
	pool, super := dbtest.Open(t)
	seeded(t, pool, super)
	b := withInstallationBus(t)
	credential := b.controlPlane(t, "agentiik-controller")
	connected, err := bus.Open(t.Context(), bus.Options{URL: b.url, Name: "leading", Credentials: &credential})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connected.Close)
	queue := control.New(connected)

	ctl, err := controller.New(pool, "leading")
	if err != nil {
		t.Fatal(err)
	}
	ctl.Sweep = time.Hour
	tm, err := pool.BeginTerm(t.Context(), "leading")
	if err != nil {
		t.Fatal(err)
	}
	var log output
	c := config.Controller{Objects: t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour}
	o := options(c, queue, versionsOf(t, pool))
	ended := make(chan error, 1)
	go func() { ended <- lead(t.Context(), ctl, tm, queue, o, nil, logger(&log)) }()

	// Once results are being taken, and the sweep a term begins with has had time to pass.
	js := b.streams(t)
	eventually(t, 10*time.Second, "the term taking results", func() bool {
		_, err := js.Consumer(t.Context(), bus.Results, "controller")
		return err == nil
	})
	time.Sleep(time.Second)

	if _, err := pool.BeginTerm(t.Context(), "usurper"); err != nil {
		t.Fatal(err)
	}
	if err := connected.Report(t.Context(), bus.TaskResult{
		TaskID:         ulid.New(),
		IdempotencyKey: string(agk.NewTaskID(agk.NewRunID(), "normalize", 1, agk.Shard{})),
		Runner:         "runner-1",
		State:          agk.TaskLost,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, db.ErrFenced) {
			t.Errorf("the term ended with %v, and the fence refused its answer", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the term was still going 20s after the fence refused its answer:\n%s", log.String())
	}
}

// versionsOf is the version store the program builds, on pool.
func versionsOf(t *testing.T, pool *db.Pool) *version.Store {
	t.Helper()
	v, err := version.New(pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The bus refuses the control plane's credential from the second it expires, and a controller
// running past it would publish nothing and hear nothing while looking alive. A standby most of
// all, since nothing it runs while it waits for the lock touches the bus, and it would find out
// only on taking over. So the program ends then, saying so, whether it leads or not: here it
// stands by, behind a controller whose credential lasts.
func TestTheProgramEndsWhenItsBusCredentialExpires(t *testing.T) {
	super := dbtest.Migrated(t)
	conn := dbtest.Superuser(t, super)
	b := withInstallationBus(t)
	configured := func(until time.Time) config.Controller {
		credential, err := b.issuer.ForControlPlane("agentiik-controller", until)
		if err != nil {
			t.Fatal(err)
		}
		return config.Controller{
			Database: config.Database{URL: dbtest.Application(super)},
			Bus:      config.Bus{URL: b.url, JWT: credential.JWT, Seed: config.Secret(credential.Seed), Expires: credential.ExpiresAt},
			Objects:  t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour,
		}
	}

	var leading, standing output
	led := make(chan error, 1)
	go func() { led <- serve(t.Context(), configured(time.Now().Add(time.Hour)), logger(&leading)) }()
	// The test's context is done before its cleanups run, so this waits for the leader to stop.
	t.Cleanup(func() { <-led })
	eventually(t, 30*time.Second, "a controller leading", func() bool {
		return strings.Contains(leading.String(), "leading")
	})
	began := termOf(t, conn)

	ended := make(chan error, 1)
	go func() { ended <- serve(t.Context(), configured(time.Now().Add(3*time.Second)), logger(&standing)) }()
	select {
	case err := <-ended:
		if !errors.Is(err, errCredentialExpired) {
			t.Errorf("the standby ended with %v once its bus credential expired\n%s", err, standing.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the standby was still running 30s after its bus credential expired\n%s", standing.String())
	}
	if !strings.Contains(standing.String(), "standing by") || termOf(t, conn) != began {
		t.Errorf("the controller whose credential expired was not standing by: the term went from %+v to %+v\n%s", began, termOf(t, conn), standing.String())
	}
}

// The first SIGTERM is a stop asked for, and the program takes it. The second is somebody for whom
// the way out is taking too long, and it ends the process as it would any other.
func TestASecondSignalEndsAProcessStillStopping(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := &output{}
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Env = append(os.Environ(), slowStopVariable+"=1")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })

	for _, want := range []string{"waiting", "stopping"} {
		if want == "stopping" {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, 10*time.Second, "the process "+want, func() bool { return strings.Contains(out.String(), want) })
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Errorf("the second SIGTERM ended the process with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second SIGTERM did not end a process still stopping")
	}
}

// The server is asked to probe every connection of the controller, so that a lock held by a
// session whose controller was cut off without a reset is released within half a minute rather
// than when the operating system's two hours are up. A URL that sets a keepalive of its own keeps
// it.
func TestTheServerProbesTheControllersConnections(t *testing.T) {
	super := dbtest.Migrated(t)
	for _, c := range []struct {
		url, idle string
	}{
		{dbtest.Application(super), "10"},
		{dbtest.Application(super) + "?application_name=agentiik-controller&tcp_keepalives_idle=42", "42"},
	} {
		conn, err := pgx.Connect(t.Context(), db.WithKeepalives(c.url))
		if err != nil {
			t.Fatal(err)
		}
		settings := map[string]string{}
		for _, k := range db.Keepalives {
			var v string
			if err := conn.QueryRow(t.Context(), "select current_setting($1)", k.Name).Scan(&v); err != nil {
				t.Fatal(err)
			}
			settings[k.Name] = v
		}
		conn.Close(context.WithoutCancel(t.Context()))
		if settings["tcp_keepalives_idle"] != c.idle || settings["tcp_keepalives_interval"] != "5" || settings["tcp_keepalives_count"] != "3" {
			t.Errorf("a session opened on %s asks the server for %v", c.url, settings)
		}
	}
}

// partition stands between a controller and PostgreSQL and, once frozen, forwards nothing either
// way while keeping every connection open: a network that stopped answering without a reset,
// which neither end can tell from a slow one.
type partition struct {
	listener net.Listener
	frozen   atomic.Bool
}

func withPartition(t *testing.T, target string) *partition {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &partition{listener: l}
	var conns sync.WaitGroup
	t.Cleanup(func() {
		l.Close()
		p.frozen.Store(false)
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
					for p.frozen.Load() {
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
	return p
}

// A leader whose database stops answering, with no reset to say so, stops leading within a few
// polls rather than going on with every statement waiting on a network that does not answer, and
// its way out is bounded as well: the rollback of a transaction the stop came in the middle of,
// the unlisten and the unlock go on connections nothing answers on either.
//
// Asked to stop while cut off, before it has noticed, it stops as well, and says nothing went wrong.
func TestALeaderCutOffFromItsDatabaseStops(t *testing.T) {
	for _, c := range []struct {
		name    string
		stopped bool
	}{{"noticing", false}, {"asked to stop", true}} {
		t.Run(c.name, func(t *testing.T) {
			super := dbtest.Migrated(t)
			u, err := url.Parse(dbtest.Application(super))
			if err != nil {
				t.Fatal(err)
			}
			p := withPartition(t, u.Host)
			u.Host = p.listener.Addr().String()

			b := withInstallationBus(t)
			credential := b.controlPlane(t, "agentiik-controller")
			var log output
			configured := config.Controller{
				Database: config.Database{URL: u.String()},
				Bus:      config.Bus{URL: b.url, JWT: credential.JWT, Seed: config.Secret(credential.Seed)},
				Objects:  t.TempDir(), MaxRequeues: graph.DefaultMaxRequeues, TaskCeiling: time.Hour,
			}
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			ended := make(chan error, 1)
			go func() { ended <- serve(ctx, configured, logger(&log)) }()
			eventually(t, 30*time.Second, "the controller leading", func() bool {
				return strings.Contains(log.String(), "leading")
			})

			p.frozen.Store(true)
			if c.stopped {
				stop()
			}
			select {
			case err := <-ended:
				switch {
				case c.stopped && err != nil:
					t.Errorf("asked to stop while cut off from its database, the controller ended with %v\n%s", err, log.String())
				case !c.stopped && !errors.Is(err, controller.ErrLockLost):
					t.Errorf("cut off from its database, the controller ended with %v\n%s", err, log.String())
				}
			// The bounds on the way out, end to end: the rollback, the unlisten and the
			// unlock at five seconds each, one after the other, then the fifteen pgx gives
			// a connection it closed, which closing the pool waits for. Thirty seconds in
			// all, so 45 is room for a slow machine and not for a wait with no bound.
			case <-time.After(45 * time.Second):
				t.Fatalf("the controller was still running 45s after its database stopped answering\n%s", log.String())
			}
		})
	}
}
