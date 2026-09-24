package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ending is a result as a runner reports one: a success of finance's invoice step.
func ending(taskID, key string) bus.TaskResult {
	code := 0
	started := time.Date(2026, 9, 10, 6, 41, 9, 104e6, time.UTC)
	return bus.TaskResult{
		TaskID: taskID, IdempotencyKey: key, Runner: "runner-dmz-02", State: agk.TaskSucceeded,
		ExitCode: &code, StartedAt: started, FinishedAt: started.Add(83 * time.Second),
		Outputs: []bus.Output{{Port: "out", Digest: "sha256:7c2e1f4a9b8c0d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c9f11", Items: 3}},
		Usage:   &bus.Usage{ImagePullMS: 0},
	}
}

var errUnreachable = errors.New("nats: no responders available for request")

// A result the bus did not take is kept and named in the heartbeat's keys, and goes out with the
// next Flush, after which nothing of it is left.
func TestAResultTheBusDidNotTakeIsKeptUntilItGoesOut(t *testing.T) {
	root := t.TempDir()
	b := &published{refuse: errUnreachable}
	results, err := OpenResults(root, b)
	if err != nil {
		t.Fatal(err)
	}
	r := ending("01M2AAZ9G62NQXFAFCXKRPJEH5", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1")
	if err := results.Report(t.Context(), r); !errors.Is(err, errUnreachable) {
		t.Fatalf("a result the bus refused answered %v", err)
	}
	if keys := results.Keys(); !slices.Equal(keys, []string{r.IdempotencyKey}) {
		t.Errorf("the heartbeat would name %v while the result of %s is kept", keys, r.IdempotencyKey)
	}
	if _, err := os.Stat(filepath.Join(root, ResultsDir, r.TaskID+".json")); err != nil {
		t.Errorf("the result is not kept under the work root: %s", err)
	}
	if err := results.Flush(t.Context()); err == nil {
		t.Error("a flush the bus refused answered nil")
	}

	b.refuse = nil
	if err := results.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.all(); len(got) != 1 || got[0].TaskID != r.TaskID {
		t.Errorf("the flush published %+v", got)
	}
	if keys := results.Keys(); len(keys) != 0 {
		t.Errorf("the heartbeat would still name %v once the result is out", keys)
	}
	if left, _ := os.ReadDir(filepath.Join(root, ResultsDir)); len(left) != 0 {
		t.Errorf("%d files are left under the results once every one is out", len(left))
	}
}

// A result is written down before it is published, so that an agent stopped between the two
// publishes it when it comes back. One the bus took leaves nothing behind, and one the wire would
// refuse is never kept, since no publication would ever take it and its key would be named for ever.
func TestAResultIsKeptOnlyUntilTheBusTakesItAndOnlyIfItCouldGoOut(t *testing.T) {
	root := t.TempDir()
	b := &published{}
	results, err := OpenResults(root, b)
	if err != nil {
		t.Fatal(err)
	}
	r := ending("01M2AAZ9G62NQXFAFCXKRPJEH5", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1")
	b.look = func() string {
		if _, err := os.Stat(filepath.Join(root, ResultsDir, r.TaskID+".json")); err != nil {
			return "not kept"
		}
		return "kept"
	}
	if err := results.Report(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.seen, []string{"kept"}) {
		t.Errorf("the result was published while it was %v on the host, and it is written down first", b.seen)
	}
	refused := ending("01M2AAZ9G62NQXFAFCXKRPJEH6", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2")
	refused.State = agk.TaskRunning
	if err := results.Report(t.Context(), refused); err == nil {
		t.Error("a result that is not an ending was taken")
	}
	if keys := results.Keys(); len(keys) != 0 {
		t.Errorf("the heartbeat would name %v", keys)
	}
	if left, _ := os.ReadDir(filepath.Join(root, ResultsDir)); len(left) != 0 {
		t.Errorf("%d files are left under the results", len(left))
	}
	if got := b.all(); len(got) != 1 {
		t.Errorf("%d results were published", len(got))
	}
}

// What a previous agent kept is read back by the next and published, and what never became a
// result, a write cut short or a file that is not one, is taken away rather than named for ever.
func TestAKeptResultOutlivesTheAgent(t *testing.T) {
	root := t.TempDir()
	first, err := OpenResults(root, &published{refuse: errUnreachable})
	if err != nil {
		t.Fatal(err)
	}
	r := ending("01M2AAZ9G62NQXFAFCXKRPJEH5", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1")
	first.Report(t.Context(), r)

	dir := filepath.Join(root, ResultsDir)
	for name, body := range map[string]string{
		".writing-1234":                   `{"task_id":`,
		"01M2AAZ9G62NQXFAFCXKRPJEH7.json": `{"task_id":"01M2AAZ9G62NQXFAFCXKRPJEH7"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	b := &published{}
	again, err := OpenResults(root, b)
	if err == nil {
		t.Error("a kept file that is no result was taken away without a word")
	}
	if again == nil {
		t.Fatal("the results were not opened")
	}
	if keys := again.Keys(); !slices.Equal(keys, []string{r.IdempotencyKey}) {
		t.Errorf("the restarted agent would name %v", keys)
	}
	if err := again.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := b.all()
	if len(got) != 1 || mustJSON(t, got[0]) != mustJSON(t, r) {
		t.Errorf("the restarted agent published %+v, and the result kept was %+v", got, r)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("%d files are left under the results", len(left))
	}
}

// jetStream is a NATS server of the test's own, with JetStream, and a bus opened on it as the
// control plane opens one, which makes the result stream. It is the server itself and not a fake,
// because what is under test is what the stream does with a copy.
func jetStream(t *testing.T) (*bus.Bus, jetstream.Stream) {
	t.Helper()
	server, err := natsserver.NewServer(&natsserver.Options{
		Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the server did not come up")
	}
	t.Cleanup(server.Shutdown)
	b, err := bus.Open(t.Context(), bus.Options{URL: server.ClientURL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	nc, err := nats.Connect(server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := js.Stream(t.Context(), bus.Results)
	if err != nil {
		t.Fatal(err)
	}
	return b, s
}

// held counts the results the stream holds.
func held(t *testing.T, s jetstream.Stream) uint64 {
	t.Helper()
	info, err := s.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

// unheard is a bus whose answer never arrives: the result goes out and the runner is told it did
// not, which is what a connection dropping under the acknowledgement looks like from the runner.
type unheard struct{ b *bus.Bus }

func (u unheard) Report(ctx context.Context, r bus.TaskResult) error {
	if err := u.b.Report(ctx, r); err != nil {
		return err
	}
	return errUnreachable
}

// On a real bus: a result whose publication failed goes out once the agent is back, and a result
// that went out while the agent heard otherwise goes out again and is held once, the stream dropping
// the copy as the same runner's word on the same dispatch and ending.
func TestAKeptResultGoesOutOnceAfterARestart(t *testing.T) {
	b, stream := jetStream(t)
	root := t.TempDir()

	before, err := OpenResults(root, &published{refuse: errUnreachable})
	if err != nil {
		t.Fatal(err)
	}
	failed := ending("01M2AAZ9G62NQXFAFCXKRPJEH5", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/1")
	if err := before.Report(t.Context(), failed); err == nil {
		t.Fatal("a result the bus refused was reported")
	}
	gone, err := OpenResults(root, unheard{b})
	if err != nil {
		t.Fatal(err)
	}
	copied := ending("01M2AAZ9G62NQXFAFCXKRPJEH6", "01JMZ8V1P9C4XQ7K2N4D6F8H0A/invoice/2")
	if err := gone.Report(t.Context(), copied); err == nil {
		t.Fatal("a result whose answer never arrived was reported")
	}
	if n := held(t, stream); n != 1 {
		t.Fatalf("the stream holds %d results before the restart, want the one whose answer was lost", n)
	}

	// The agent comes back.
	after, err := OpenResults(root, b)
	if err != nil {
		t.Fatal(err)
	}
	if keys := after.Keys(); !slices.Equal(keys, []string{failed.IdempotencyKey, copied.IdempotencyKey}) {
		t.Errorf("the restarted agent names %v", keys)
	}
	if err := after.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := after.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := held(t, stream); n != 2 {
		t.Errorf("the stream holds %d results, want each of the two once", n)
	}
	if keys := after.Keys(); len(keys) != 0 {
		t.Errorf("the agent still names %v once both are out", keys)
	}
}
