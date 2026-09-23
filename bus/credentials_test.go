package bus

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/controller"
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

// withAccounts runs a server that trusts one operator and one account, with the features named
// turned on.
func withAccounts(t *testing.T, features ...string) authenticated {
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

	flags := map[string]bool{}
	for _, f := range features {
		flags[f] = true
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
		FeatureFlags:     flags,
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

	// Under its own inbox, which is the one its replies may come back under.
	conn, err := nats.Connect(a.url, nats.UserJWTAndSeed(runner.JWT, runner.Seed), nats.CustomInboxPrefix(Inbox("runner-1")))
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

	// And says what happened, as itself.
	if err := conn.Publish(ResultSubject("runner-1"), []byte(`{"task_id":"one"}`)); err != nil {
		t.Errorf("publishing a result: %s", err)
	}
	if err := conn.Flush(); err != nil {
		t.Errorf("publishing a result: %s", err)
	}
	if err := conn.LastError(); err != nil {
		t.Errorf("publishing a result on its own subject: %s", err)
	}

	// And as nobody else: the subject a result arrives on is who sent it.
	conn.Publish(ResultSubject("runner-2"), []byte(`{"task_id":"theirs"}`))
	conn.Flush()
	if err := conn.LastError(); err == nil || !strings.Contains(err.Error(), ResultSubject("runner-2")) {
		t.Errorf("a runner published a result as another runner, and the server said %v", err)
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
	if err := conn.LastError(); err == nil || !strings.Contains(err.Error(), Subject("dmz")) {
		t.Errorf("a runner published a task of its own, and the server said %v", err)
	}
}

// Take, holding the credential a runner is minted and nothing else, from the consumer the control
// plane created. It is the whole path a runner walks, and the one that failed while each half of
// it passed on its own: Take created a consumer of its own, which the credential could not do and
// which the stream refused beside the pool's. It is walked against both forms a server writes an
// acknowledgement subject in, since the credential allows each by name.
func TestARunnerTakesFromThePoolsConsumer(t *testing.T) {
	for name, features := range map[string][]string{
		"acknowledging in the form servers write today":       nil,
		"acknowledging in the form a later server will write": {natsserver.FeatureFlagJsAckFormatV2},
	} {
		t.Run(name, func(t *testing.T) { takesFromThePoolsConsumer(t, withAccounts(t, features...)) })
	}
}

func takesFromThePoolsConsumer(t *testing.T, a authenticated) {
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
	result := aResult(aTask("mine"))
	result.TaskID, result.Runner = taken[0].Task.TaskID, "runner-1"
	if err := runner.Report(t.Context(), result); err != nil {
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

// "Its reach is the tasks in its hands." JetStream hands a pulled message to the inbox the pull
// named, grant and all, so a runner allowed every inbox would hear every task handed to every other
// runner, and could redeem a grant before the runner it was handed to got there. Each runner hears
// under its own inbox, and a machine of the same pool or of another one is refused the others, and
// hears nothing of a task taken, acknowledged and answered beside it. Nor can it acknowledge what
// was handed to somebody else, outside its own pool's consumer.
func TestARunnerHearsNothingHandedToAnotherRunner(t *testing.T) {
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

	// Two machines listening wherever they are allowed to, one of the same pool and one of
	// another, each on a connection of its own with the inbox every client shares by default.
	type spy struct {
		name    string
		conn    *nats.Conn
		subs    []*nats.Subscription
		refused chan error
	}
	var spies []*spy
	for _, s := range []struct{ name, pool string }{{"runner-2", "dmz"}, {"runner-spy", "lan"}} {
		minted, err := a.issuer.ForRunner(s.name, s.pool, until)
		if err != nil {
			t.Fatal(err)
		}
		refused := make(chan error, 16)
		conn, err := nats.Connect(a.url, nats.UserJWTAndSeed(minted.JWT, minted.Seed),
			nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { refused <- err }))
		if err != nil {
			t.Fatalf("%s could not connect: %s", s.name, err)
		}
		defer conn.Close()
		sp := &spy{name: s.name, conn: conn, refused: refused}
		for _, subject := range []string{"_INBOX.>", Inbox("runner-1") + ".>", "$JS.ACK.>"} {
			sub, err := conn.SubscribeSync(subject)
			if err != nil {
				t.Fatal(err)
			}
			sp.subs = append(sp.subs, sub)
		}
		if err := conn.Flush(); err != nil {
			t.Fatal(err)
		}
		spies = append(spies, sp)
	}

	// runner-1 takes a task of dmz, holds it and answers it, and the controller takes the
	// answer back.
	if err := b.Publish(t.Context(), dispatch("mine", "pool=dmz")); err != nil {
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
	if err != nil || len(taken) != 1 {
		t.Fatalf("taking: %v, %d", err, len(taken))
	}
	if err := taken[0].Held(t.Context()); err != nil {
		t.Fatalf("acknowledging: %s", err)
	}
	result := aResult(aTask("mine"))
	result.TaskID, result.Runner = taken[0].Task.TaskID, "runner-1"
	if err := runner.Report(t.Context(), result); err != nil {
		t.Fatalf("reporting: %s", err)
	}
	got := answering(t, b, func(controller.Answer) error { return nil })
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the controller never took the result back")
	}

	for _, sp := range spies {
		for _, sub := range sp.subs {
			if msg, err := sub.NextMsg(300 * time.Millisecond); err == nil {
				t.Errorf("%s listening on %s heard %s: %s", sp.name, sub.Subject, msg.Subject, msg.Data)
			}
		}
		heard := map[string]bool{}
		for waiting := true; waiting; {
			select {
			case err := <-sp.refused:
				for _, sub := range sp.subs {
					if strings.Contains(err.Error(), "Subscription to \""+sub.Subject+"\"") {
						heard[sub.Subject] = true
					}
				}
			default:
				waiting = false
			}
		}
		for _, sub := range sp.subs {
			if !heard[sub.Subject] {
				t.Errorf("%s was allowed to listen on %s", sp.name, sub.Subject)
			}
		}
	}

	// And acknowledging is its own pool's consumer's and nobody else's: not another pool's
	// work, and not the results the controller has yet to acknowledge.
	outsider := spies[1].conn
	for _, subject := range []string{
		"$JS.ACK." + Stream + "." + Durable("dmz") + ".1.1.1.1.0",
		"$JS.ACK._.hash." + Stream + "." + Durable("dmz") + ".1.1.1.1.0",
		"$JS.ACK." + Results + ".controller.1.1.1.1.0",
	} {
		outsider.Publish(subject, []byte("+TERM"))
		outsider.Flush()
		if err := outsider.LastError(); err == nil || !strings.Contains(err.Error(), subject) {
			t.Errorf("a runner of lan acknowledged on %s, and the server said %v", subject, err)
		}
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
		{"a runner whose results would be every runner's", func() error {
			_, err := a.issuer.ForRunner(">", "dmz", until)
			return err
		}},
		{"a runner reaching another one's results", func() error {
			_, err := a.issuer.ForRunner("runner-1.runner-2", "dmz", until)
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
