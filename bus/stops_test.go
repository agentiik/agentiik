package bus

import (
	"context"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/nats-io/jwt/v2"
)

// A stop is held to wire.schema.json $defs/stop on both ends: what Stop writes, and what Stops
// reads. The runner is what the second half is for, and it is written against the document rather
// than against this package.

// allReasons is the four reasons a stop is sent for.
var allReasons = []graph.StopReason{graph.StopSuperseded, graph.StopSiblingFailed, graph.StopDeadline, graph.StopCancelled}

// Every reason is written as the wire describes it, and read back as the stop it was.
func TestAStopIsWrittenAsTheWireDescribesIt(t *testing.T) {
	s := wire(t, "stop")
	for _, reason := range allReasons {
		for _, task := range []string{"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2/3/8"} {
			want := graph.Stop{Task: aKey(task), Reason: reason}
			body, err := encodeStop(want)
			if err != nil {
				t.Fatalf("%+v: %s", want, err)
			}
			if err := validates(t, s, body); err != nil {
				t.Errorf("%s is refused by the wire: %s", body, err)
			}
			got, err := readStop(body)
			if err != nil {
				t.Fatalf("%s: %s", body, err)
			}
			if got != want {
				t.Errorf("%s was read back as %+v", body, got)
			}
		}
	}
}

// Every stop in the corpus is read the way the corpus says it is: a valid one is read, and written
// back out as the document it was read from, and an invalid one is refused.
func TestAStopIsReadAsTheWireDescribesIt(t *testing.T) {
	s := wire(t, "stop")
	cases, err := fixtures.Stops()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 2 {
		t.Fatalf("the vendored stop corpus holds %d documents", len(cases))
	}
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatalf("%s: %s", c.File, err)
		}
		bySchema := validates(t, s, body)
		stop, byReader := readStop(body)
		if !c.Valid {
			if bySchema == nil {
				t.Errorf("%s should be refused by the schema: %s", c.File, c.Rule)
			}
			if byReader == nil {
				t.Errorf("%s was read as %+v, and it is refused because %s", c.File, stop, c.Rule)
			}
			continue
		}
		if bySchema != nil {
			t.Errorf("%s should be accepted by the schema: %s", c.File, bySchema)
		}
		if byReader != nil {
			t.Errorf("%s, which covers %s, was refused: %s", c.File, c.Covers, byReader)
			continue
		}
		again, err := encodeStop(stop)
		if err != nil {
			t.Errorf("%s could not be written back out: %s", c.File, err)
			continue
		}
		if !sameDocument(t, body, again) {
			t.Errorf("%s was written back out as another document:\n%s\n\n%s", c.File, body, again)
		}
	}
}

// What the wire refuses, the reader refuses too, and none of it is read as some stop: a missing
// reason read as the first of the four would stop a container as superseded when nobody said so.
func TestWhatIsNotAStopIsNotReadAsOne(t *testing.T) {
	s := wire(t, "stop")
	for _, body := range []string{
		`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1"}`,
		`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1","reason":null}`,
		`{"reason":"cancelled"}`,
		`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize","reason":"cancelled"}`,
		`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1","reason":"stopped"}`,
		`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1","reason":"cancelled","runner":"runner-1"}`,
		`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1","reason":"cancelled"}{}`,
		`not a stop`,
	} {
		if stop, err := readStop([]byte(body)); err == nil {
			t.Errorf("%s was read as %+v", body, stop)
		}
		// The trailing document is the one case JSON Schema has no view on, since it is not
		// one document to hand it.
		if strings.HasSuffix(body, "{}") || body == "not a stop" {
			continue
		}
		if err := validates(t, s, []byte(body)); err == nil {
			t.Errorf("%s is accepted by the wire, and the reader refuses it", body)
		}
	}
}

// A stop that could not be read is not published, where it would be a container left running to
// its deadline while the control plane believed it had asked.
func TestAStopNobodyCouldReadIsNotWritten(t *testing.T) {
	for _, s := range []graph.Stop{
		{Task: aKey("01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize"), Reason: graph.StopCancelled},
		{Task: "", Reason: graph.StopCancelled},
		{Task: aKey("01JMZ8V1P9C4XQ7K2N4D6F8H0A/normalize/1"), Reason: graph.StopReason(len(allReasons))},
	} {
		if body, err := encodeStop(s); err == nil {
			t.Errorf("%+v was written as %s", s, body)
		}
	}
}

// A runner hears stops over the connection OpenRunner opened, holding the credential it was minted
// and nothing else, as a real server enforces it: it is handed each stop the control plane
// publishes, it cannot publish one itself, and once it stops listening it is handed nothing more.
func TestARunnerHearsStopsOverItsOwnConnection(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)
	control := controlPlane(t, a, until)

	minted, err := a.issuer.ForRunner("runner-1", "dmz", until)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := OpenRunner(Options{URL: minted.URL, Name: "runner-1", Credentials: &minted})
	if err != nil {
		t.Fatalf("the runner could not connect: %s", err)
	}
	defer runner.Close()

	heard := make(chan graph.Stop, 16)
	listening, stopListening := context.WithCancel(t.Context())
	defer stopListening()
	if err := runner.Stops(listening, func(s graph.Stop) { heard <- s }); err != nil {
		t.Fatalf("the runner could not listen for stops: %s", err)
	}

	// Published the moment Stops has answered, which is what its answer promises.
	for _, reason := range allReasons {
		want := graph.Stop{Task: aKey("01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2/3/8"), Reason: reason}
		if err := control.Stop(t.Context(), want); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-heard:
			if got != want {
				t.Errorf("the runner heard %+v for %+v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the runner never heard %+v", want)
		}
	}

	// A runner cannot stop anybody's task, its own pool's or another's.
	runner.conn.Publish(StopSubject, []byte(`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1","reason":"cancelled"}`))
	runner.conn.Flush()
	if err := runner.conn.LastError(); err == nil || !strings.Contains(err.Error(), StopSubject) {
		t.Errorf("a runner published a stop, and the server said %v", err)
	}
	select {
	case s := <-heard:
		t.Errorf("a stop a runner published was handed over: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}

	// And once it has stopped listening, nothing more is handed over.
	stopListening()
	if err := control.Stop(t.Context(), graph.Stop{Task: aKey("01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"), Reason: graph.StopCancelled}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-heard:
		t.Errorf("a stop was handed over after the runner stopped listening: %+v", s)
	case <-time.After(500 * time.Millisecond):
	}
}

// A credential that may not hear stops is told so, rather than handed a subscription that hears
// nothing: a runner that believed it was listening would run every stopped container to its
// deadline.
func TestARunnerThatMayNotHearStopsIsToldSo(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)
	deaf, err := a.issuer.mint("runner-1", until, func(c *jwt.UserClaims) {
		c.Sub.Allow.Add(Inbox("runner-1") + ".>")
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := OpenRunner(Options{URL: deaf.URL, Name: "runner-1", Credentials: &deaf})
	if err != nil {
		t.Fatalf("the runner could not connect: %s", err)
	}
	defer runner.Close()
	err = runner.Stops(t.Context(), func(graph.Stop) {})
	if err == nil || !strings.Contains(err.Error(), "may not hear stops") {
		t.Errorf("a runner that may not hear stops was told %v", err)
	}
}

// A stop nobody can read is said out loud and nothing is handed over for it, and the stops after it
// still are.
func TestAStopNobodyCanReadIsSaidAndPassedOver(t *testing.T) {
	a := withAccounts(t)
	until := time.Now().UTC().Add(time.Hour)
	control := controlPlane(t, a, until)

	minted, err := a.issuer.ForRunner("runner-1", "dmz", until)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := OpenRunner(Options{URL: minted.URL, Name: "runner-1", Credentials: &minted})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	var mu sync.Mutex
	var trouble []string
	runner.Trouble = func(subject string, err error) {
		mu.Lock()
		defer mu.Unlock()
		trouble = append(trouble, subject+": "+err.Error())
	}
	heard := make(chan graph.Stop, 16)
	if err := runner.Stops(t.Context(), func(s graph.Stop) { heard <- s }); err != nil {
		t.Fatal(err)
	}

	if err := control.conn.Publish(StopSubject, []byte(`{"task":"01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"}`)); err != nil {
		t.Fatal(err)
	}
	want := graph.Stop{Task: aKey("01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1"), Reason: graph.StopDeadline}
	if err := control.Stop(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-heard:
		if got != want {
			t.Errorf("the runner was handed %+v, where the one it could read was %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop after the unreadable one was never handed over")
	}
	select {
	case s := <-heard:
		t.Errorf("more was handed over: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(trouble) != 1 || !strings.HasPrefix(trouble[0], StopSubject+": ") || !strings.Contains(trouble[0], "gives no reason") {
		t.Errorf("what was said reads %q", trouble)
	}
}

// controlPlane opens the bus as the controller does, under the credential it is minted.
func controlPlane(t *testing.T, a authenticated, until time.Time) *Bus {
	t.Helper()
	minted, err := a.issuer.ForControlPlane("controller", until)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(t.Context(), Options{URL: a.url, Name: "controller", Credentials: &minted})
	if err != nil {
		t.Fatalf("the control plane could not connect: %s", err)
	}
	t.Cleanup(b.Close)
	return b
}

// aKey is an idempotency key as a stop names it.
func aKey(s string) agk.TaskID { return agk.TaskID(s) }
