package bus

import (
	"context"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// What a runner credential may do, enforced by a real server.
//
// Asserting the claims this package writes would be asserting its own JSON. What matters is what
// the server does with them, and the first version of this permitted $JS.API.> and let a runner
// create a stream. So the test runs one.

type authenticated struct {
	url    string
	issuer *Issuer
}

func withAccounts(t *testing.T) authenticated {
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

	// JetStream refuses to start without a system account, which is a thing an operator
	// configures and not a thing this package has an opinion about.
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
		Port:             -1,
		JetStream:        true,
		StoreDir:         t.TempDir(),
		TrustedOperators: []*jwt.OperatorClaims{trusted},
		AccountResolver:  resolver,
		SystemAccount:    systemPublic,
		NoLog:            true,
		NoSigs:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the server did not come up")
	}
	t.Cleanup(server.Shutdown)

	issuer, err := NewIssuer(string(accountSeed), server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	return authenticated{url: server.ClientURL(), issuer: issuer}
}

func TestARunnerTakesItsOwnWorkAndCanDoNothingElse(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)

	control, err := a.issuer.ForControlPlane("controller", until)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(t.Context(), Options{URL: a.url, Name: "controller", Credentials: &control})
	if err != nil {
		t.Fatalf("the control plane could not connect: %s", err)
	}
	defer b.Close()
	if err := b.Consumer(t.Context(), "dmz"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.js.Publish(t.Context(), Subject("dmz"), []byte(`{"task_id":"one"}`)); err != nil {
		t.Fatal(err)
	}

	runner, err := a.issuer.ForRunner("runner-1", "dmz", until)
	if err != nil {
		t.Fatal(err)
	}
	if runner.Kind != Kind || runner.URL != a.url || !runner.ExpiresAt.Equal(until.Truncate(time.Second)) {
		t.Errorf("the credential reads %+v", runner)
	}

	conn, err := nats.Connect(a.url, nats.UserJWTAndSeed(runner.JWT, runner.Seed))
	if err != nil {
		t.Fatalf("the runner could not connect: %s", err)
	}
	defer conn.Close()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}

	// It takes its own work, and acknowledges it.
	consumer, err := js.Consumer(t.Context(), Stream, Durable("dmz"))
	if err != nil {
		t.Fatalf("binding to its own pool's consumer: %s", err)
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	took := 0
	for msg := range batch.Messages() {
		took++
		if err := msg.Ack(); err != nil {
			t.Errorf("acknowledging: %s", err)
		}
	}
	if took != 1 {
		t.Fatalf("the runner took %d messages", took)
	}

	// And says what happened.
	if err := conn.Publish(ResultSubject, []byte(`{"task_id":"one"}`)); err != nil {
		t.Errorf("publishing a result: %s", err)
	}
	if err := conn.Flush(); err != nil {
		t.Errorf("publishing a result: %s", err)
	}

	// What it cannot do, in the server's own words.
	short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := js.CreateOrUpdateStream(short, jetstream.StreamConfig{Name: "MINE", Subjects: []string{"mine.>"}}); err == nil {
		t.Error("a runner created a stream")
	}
	if _, err := js.Consumer(short, Stream, Durable("lan")); err == nil {
		t.Error("a runner reached another pool's consumer")
	}
	if err := js.DeleteStream(short, Stream); err == nil {
		t.Error("a runner deleted the task stream")
	}

	conn.Publish(Subject("dmz"), []byte(`{"task_id":"mine"}`))
	conn.Flush()
	if conn.LastError() == nil {
		t.Error("a runner published a task of its own")
	}
}

// Take, holding the credential a runner is minted and nothing else, from the consumer the control
// plane created. It is the whole path a runner walks, and the one that failed while each half of
// it passed on its own: Take created a consumer of its own, which the credential could not do and
// which the stream refused beside the pool's.
func TestARunnerTakesFromThePoolsConsumer(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)

	control, err := a.issuer.ForControlPlane("controller", until)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(t.Context(), Options{URL: a.url, Name: "controller", Credentials: &control})
	if err != nil {
		t.Fatalf("the control plane could not connect: %s", err)
	}
	defer b.Close()
	for _, pool := range []string{"dmz", "lan"} {
		if err := b.Consumer(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Publish(t.Context(), dispatch("mine", "pool=dmz")); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context(), dispatch("theirs", "pool=lan")); err != nil {
		t.Fatal(err)
	}

	minted, err := a.issuer.ForRunner("runner-1", "dmz", until)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := OpenRunner(Options{URL: minted.URL, Name: "runner-1", Credentials: &minted})
	if err != nil {
		t.Fatalf("the runner could not connect: %s", err)
	}
	defer runner.Close()

	taken, err := runner.Take(t.Context(), "dmz", 8, 3*time.Second)
	if err != nil {
		t.Fatalf("the runner could not take from its own pool: %s", err)
	}
	if len(taken) != 1 || taken[0].Task.Step != "mine" {
		t.Fatalf("the runner took %+v, and its pool holds one task", taken)
	}
	if err := taken[0].Held(t.Context()); err != nil {
		t.Fatalf("acknowledging: %s", err)
	}

	// And says what happened, on the same connection.
	if err := runner.Report(t.Context(), controller.Answer{
		Result: graph.Result{Task: aTask("mine").ID, State: agk.TaskSucceeded},
		Runner: "runner-1",
	}); err != nil {
		t.Errorf("reporting: %s", err)
	}

	// Another pool's work is not this runner's, and it is still there for that pool.
	short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if got, err := runner.Take(short, "lan", 8, 300*time.Millisecond); err == nil {
		t.Errorf("a runner of dmz took %d tasks from lan", len(got))
	}
	theirs, err := b.Take(t.Context(), "lan", 8, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 1 || theirs[0].Task.Step != "theirs" {
		t.Errorf("the lan pool holds %+v, and the task published to it was left alone", theirs)
	}
}

// A credential that has run out is a credential a stolen disk holds and nothing more.
func TestABusCredentialStopsWorking(t *testing.T) {
	a := withAccounts(t)
	runner, err := a.issuer.ForRunner("runner-1", "dmz", time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := nats.Connect(a.url, nats.UserJWTAndSeed(runner.JWT, runner.Seed))
	if err == nil {
		conn.Close()
		t.Fatal("an expired credential connected")
	}
}

func TestWhatCannotBeMinted(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)

	for _, c := range []struct {
		name string
		mint func() error
	}{
		{"a credential for nobody", func() error {
			_, err := a.issuer.ForRunner("", "dmz", until)
			return err
		}},
		{"a credential for no pool", func() error {
			_, err := a.issuer.ForRunner("runner-1", "", until)
			return err
		}},
		{"a pool that is every pool", func() error {
			_, err := a.issuer.ForRunner("runner-1", ">", until)
			return err
		}},
		{"a pool reaching another one", func() error {
			_, err := a.issuer.ForRunner("runner-1", "dmz.lan", until)
			return err
		}},
		{"a credential that never expires", func() error {
			_, err := a.issuer.ForRunner("runner-1", "dmz", time.Time{})
			return err
		}},
	} {
		if err := c.mint(); err == nil {
			t.Errorf("%s was minted", c.name)
		}
	}

	// And an issuer that could not sign anything a server would trust.
	for _, c := range []struct{ name, seed, url string }{
		{"no address", string(mustAccountSeed(t)), ""},
		{"a seed that is not a key", "not a seed", "nats://127.0.0.1:4222"},
		{"a user seed rather than an account's", string(mustUserSeed(t)), "nats://127.0.0.1:4222"},
	} {
		if _, err := NewIssuer(c.seed, c.url); err == nil {
			t.Errorf("an issuer with %s was built", c.name)
		}
	}
}

func mustAccountSeed(t *testing.T) []byte {
	t.Helper()
	pair, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := pair.Seed()
	return seed
}

func mustUserSeed(t *testing.T) []byte {
	t.Helper()
	pair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := pair.Seed()
	return seed
}
