package bus

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/graph"
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

// withAccounts runs a server on the bus identity NewInstallation writes, with the features named
// turned on, and an Issuer holding the account seed it wrote for the API. So every test here holds
// the credentials the API mints to a server configured the way an installation's is, rather than to
// one a test built for itself.
func withAccounts(t *testing.T, features ...string) authenticated {
	t.Helper()
	in, err := NewInstallation(t.TempDir(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	url := serveFrom(t, in, features...)
	seed, err := os.ReadFile(in.AccountSeed)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewIssuer(strings.TrimSpace(string(seed)), url)
	if err != nil {
		t.Fatal(err)
	}
	return authenticated{url: url, issuer: issuer}
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
	if err := b.Publish(t.Context(), message("mine", "pool=dmz")); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context(), message("theirs", "pool=lan")); err != nil {
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
	if err := b.Publish(t.Context(), message("mine", "pool=dmz")); err != nil {
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
	got := reporting(t, b, func(heard) error { return nil })
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

// A revoked runner finishes its grace holding a credential that publishes its results and hears
// stops, and does nothing else: "narrowed to publishing results and hearing stops, no pull". Its
// result reaches the controller as its own, and the work still on its pool's queue is left there
// for another runner.
func TestARevokedRunnerPublishesItsResultsAndTakesNothing(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)
	control := openControlPlane(t, a, until)
	if err := control.Consumer(t.Context(), "dmz"); err != nil {
		t.Fatal(err)
	}
	if err := control.Publish(t.Context(), message("waiting", "pool=dmz")); err != nil {
		t.Fatal(err)
	}
	got := reporting(t, control, func(heard) error { return nil })

	minted, err := a.issuer.ForRevokedRunner("runner-1", until)
	if err != nil {
		t.Fatal(err)
	}
	if !minted.ExpiresAt.Equal(until.Truncate(time.Second)) {
		t.Errorf("the credential expires at %s, and was minted until %s", minted.ExpiresAt, until)
	}
	if _, err := a.issuer.ForRevokedRunner("Runner One", until); err == nil {
		t.Error("a credential was minted for a runner no subject can name")
	}
	runner, err := OpenRunner(Options{URL: minted.URL, Name: "runner-1", Credentials: &minted})
	if err != nil {
		t.Fatalf("the revoked runner could not connect: %s", err)
	}
	defer runner.Close()

	// It publishes the result of what it holds, and the controller hears it from that runner.
	result := aResult(aTask("mine"))
	result.Runner = "runner-1"
	if err := runner.Report(t.Context(), result); err != nil {
		t.Fatalf("a revoked runner's result was refused: %s", err)
	}
	select {
	case h := <-got:
		if h.sender != "runner-1" || h.result.TaskID != result.TaskID {
			t.Errorf("the controller heard %+v", h)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked runner's result never reached the controller")
	}

	// It hears a stop.
	heard := make(chan graph.Stop, 1)
	if err := runner.Stops(t.Context(), func(s graph.Stop) { heard <- s }); err != nil {
		t.Fatalf("the revoked runner could not listen for stops: %s", err)
	}
	stop := graph.Stop{Task: aKey("01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"), Reason: graph.StopCancelled}
	if err := control.Stop(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-heard:
		if s != stop {
			t.Errorf("the revoked runner heard %+v", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked runner never heard the stop")
	}

	// And takes nothing: not from its pool, not by asking after the consumer, not by
	// acknowledging, and not as anybody else.
	short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if taken, err := runner.Take(short, "dmz", 8, 300*time.Millisecond); err == nil {
		t.Errorf("a revoked runner took %d tasks from its pool", len(taken))
	}
	for _, subject := range []string{
		"$JS.API.CONSUMER.MSG.NEXT." + Stream + "." + Durable("dmz"),
		"$JS.ACK." + Stream + "." + Durable("dmz") + ".1.1.1.1.1",
		ResultSubject("runner-2"),
	} {
		runner.conn.Publish(subject, []byte("{}"))
		runner.conn.Flush()
		if err := runner.conn.LastError(); err == nil || !strings.Contains(err.Error(), subject) {
			t.Errorf("a revoked runner published on %s, and the server said %v", subject, err)
		}
	}

	// The task it could not take is still on the queue, for a runner that may.
	theirs, err := control.Take(t.Context(), "dmz", 8, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 1 || theirs[0].Task.Step != "waiting" {
		t.Errorf("the pool holds %+v, and the task published to it was left alone", theirs)
	}
}

// A runner says how the tasks it holds are getting on under the credential it takes work with, and
// under the narrower one it finishes a revocation's grace with, since "Revoking a credential never
// destroys work already done" and the work is still showing. Both publish on the runner's results
// subject and nowhere else, so neither can say how another runner's tasks are getting on.
func TestARunnerAndARevokedRunnerSayHowTheirTasksAreGettingOn(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)
	control := openControlPlane(t, a, until)
	got := reporting(t, control, func(heard) error { return nil })

	taking, err := a.issuer.ForRunner("runner-1", "dmz", until)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := a.issuer.ForRevokedRunner("runner-1", until)
	if err != nil {
		t.Fatal(err)
	}
	for name, minted := range map[string]Credentials{"a runner": taking, "a revoked runner": revoked} {
		runner, err := OpenRunner(Options{URL: minted.URL, Name: "runner-1", Credentials: &minted})
		if err != nil {
			t.Fatalf("%s could not connect: %s", name, err)
		}
		p := aProgress(aTask("mine").ID)
		p.Runner = "runner-1"
		if err := runner.Progress(t.Context(), p); err != nil {
			t.Errorf("%s's progress was refused: %s", name, err)
		}
		select {
		case h := <-got:
			if h.sender != "runner-1" || h.progress == nil || *h.progress != p {
				t.Errorf("the controller heard %s as %+v from %s", name, h.progress, h.sender)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s's progress never reached the controller", name)
		}

		theirs := aProgress(aTask("theirs").ID)
		theirs.Runner = "runner-2"
		short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		if err := runner.Progress(short, theirs); err == nil {
			t.Errorf("%s published progress as another runner", name)
		}
		cancel()
		runner.Close()
	}
}
