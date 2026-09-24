package bus_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/bus/control"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// The order a runner takes a task in, against a real bus, a real database and the API it redeems
// at. It takes the message, writes the key down on its host, redeems the grant, which binds the
// task to it, and acknowledges the message; only then does it pull the image and run. Whoever the
// bus hands a message to, the redemption decides who runs the task, and the acknowledgement
// follows the redemption.
//
// The host's record, driver.Docker.Hold, is left out. It is on the host that took the message, and
// neither the bus nor another runner reads it.

// ackWait is the pool consumer's wait cut short, so that what follows it is seen in seconds rather
// than after bus.AckWait.
const ackWait = 2 * time.Second

// taking is one task of the dmz pool, dispatched and on the bus, the API that redeems its grant,
// and two runners of the pool to take it.
type taking struct {
	bus   *bus.Bus
	pool  *db.Pool
	conn  *pgx.Conn
	api   http.Handler
	row   string
	first runner
	other runner
}

// runner is one machine of the pool: who it is, the credential it presents to the API, and its
// own connection to the bus.
type runner struct {
	id, credential string
	bus            *bus.Bus
}

// dispatched is what the controller leaves behind when it hands a task out: the run, its version,
// the task's row, its grant, and the message on the pool's queue.
func dispatched(t *testing.T) taking {
	t.Helper()
	b := bus.Opened(t)
	b.Waiting(t, "dmz", ackWait)
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	ctx := t.Context()
	now := time.Now().UTC()

	run := agk.NewRunID()
	row := ulid.New()
	key := agk.NewTaskID(run, "render", 1, agk.Shard{})
	const commit = "a3f9c1e"

	// The version the run pins, with a tree of one file, since a redemption answers the tree
	// and refuses a version that has none.
	objects := artifact.Dir(t.TempDir())
	entry := []byte("apiVersion: agentiik.dev/v1\nkind: Workflow\n")
	sum := sha256.Sum256(entry)
	digest := hex.EncodeToString(sum[:])
	if err := objects.Put(ctx, artifact.Key("finance", digest), bytes.NewReader(entry)); err != nil {
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
			Workflow: "monthly-invoicing", Commit: commit, Entry: "agentiik.yaml", Document: entry,
			Tree:   []db.TreeFile{{Path: "agentiik.yaml", SHA256: digest, Size: int64(len(entry)), Mode: "0644"}},
			Author: "alice",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`insert into runs (namespace, id, workflow, commit, trigger)
		 values ('finance', $1, 'monthly-invoicing', 'a3f9c1e', 'manual')`,
		`insert into steps (namespace, run_id, step) values ('finance', $1, 'render')`,
	} {
		if _, err := conn.Exec(ctx, stmt, string(run)); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	if _, err := conn.Exec(ctx, `
		insert into tasks (namespace, id, run_id, step, attempt, state, dispatched_at, published_at)
		values ('finance', $1, $2, 'render', 1, 'dispatched', now(), now())`, row, string(run)); err != nil {
		t.Fatalf("seeding: %s", err)
	}

	// Two machines of the pool, each with its credential and its connection to the bus.
	var runners [2]runner
	if err := pool.Installation(ctx, db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, CreatedBy: "admin"}); err != nil {
			return err
		}
		for i := range runners {
			token, err := w.IssueJoinToken(ctx, "dmz", nil, "admin", now, now.Add(time.Hour))
			if err != nil {
				return err
			}
			joined, err := w.Join(ctx, db.Joining{
				Token: token.Clear, CPU: 4, MemoryBytes: 1 << 33, DiskBytes: 1 << 37,
				Architecture: "amd64", AgentVersion: "0.2.0",
			}, time.Hour, now)
			if err != nil {
				return err
			}
			runners[i].id, runners[i].credential = joined.Runner, joined.Credential
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := range runners {
		connected, err := bus.OpenRunner(bus.Options{URL: os.Getenv("AGENTIIK_TEST_BUS_URL"), Name: runners[i].id})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(connected.Close)
		runners[i].bus = connected
	}

	// The grant, and the message that carries it, published as the controller publishes one.
	var granted db.Granted
	if err := pool.Installation(ctx, db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		granted, err = w.IssueGrant(ctx, "finance", key, row, db.GrantScope{
			Run: run, Step: "render", Workflow: "monthly-invoicing", Commit: commit,
		}, now.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.New(b).Publish(ctx, controller.Dispatch{
		Task: graph.Task{
			ID: key, Run: run, Namespace: "finance", Workflow: "monthly-invoicing", Commit: commit,
			Step: "render", Attempt: 1,
			Image:     "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc",
			Outputs:   []agk.Port{"ok"},
			Resources: graph.Resources{CPU: "1", Memory: "512Mi", PIDs: 256},
			RunsOn:    []string{"pool=dmz"},
			Deadline:  now.Add(time.Hour),
		},
		Row: row, Grant: granted.Clear, Inputs: map[agk.Port]controller.InputRef{},
	}); err != nil {
		t.Fatal(err)
	}

	// The API a runner redeems at, speaking to runners and to nobody else.
	signed, err := artifact.NewSigned(objects, artifact.SignedOptions{
		Key: []byte("0123456789abcdef0123456789abcdef"), Base: "https://agentiik.example.com/objects",
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(api.DenyAll{}, func(*http.Request) (api.Principal, error) {
		return "", errors.New("these tests speak as runners and as nobody else")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{Pool: pool, Objects: objects, URLs: signed}); err != nil {
		t.Fatal(err)
	}

	return taking{bus: b, pool: pool, conn: conn, api: rt, row: row, first: runners[0], other: runners[1]}
}

// take is one runner asking its pool for work, and answering the one message it was handed, if it
// was handed one within wait.
func (r runner) take(t *testing.T, wait time.Duration) (bus.Taken, bool) {
	t.Helper()
	taken, err := r.bus.Take(t.Context(), "dmz", 1, wait)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) == 0 {
		return bus.Taken{}, false
	}
	return taken[0], true
}

// redeem is a runner redeeming the grant a message carries, as it does once the key is written down
// and before it acknowledges. It answers the status and what the body said.
func (tk taking) redeem(t *testing.T, r runner, m bus.TaskMessage) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(api.Redemption{Grant: m.Grant, TaskID: m.TaskID, IdempotencyKey: agk.TaskID(m.IdempotencyKey)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/redeem", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.credential)
	w := httptest.NewRecorder()
	tk.api.ServeHTTP(w, req)
	var answer map[string]any
	json.Unmarshal(w.Body.Bytes(), &answer)
	return w.Code, answer
}

// task reads the dispatch as the database has it: its state, and the runner bound to it or "-".
func (tk taking) task(t *testing.T) string {
	t.Helper()
	var state string
	if err := tk.conn.QueryRow(t.Context(),
		`select state || ' ' || coalesce(runner, '-') from tasks where id = $1`, tk.row).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// swept is the heartbeat's sweep as the controller runs it, at a moment of the test's choosing, and
// answers how many tasks it declared lost.
func (tk taking) swept(t *testing.T, at time.Time) int {
	t.Helper()
	var lost int
	if err := tk.pool.Installation(t.Context(), db.ControllerSweep, func(ctx context.Context, w *db.Wide) error {
		var err error
		lost, err = w.Lost(ctx, at, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return lost
}

// beat is one heartbeat of a runner naming the keys it holds, recorded as the API records it and
// at a moment of the test's choosing.
func (tk taking) beat(t *testing.T, r runner, at time.Time, holding ...agk.TaskID) {
	t.Helper()
	if err := tk.pool.Installation(t.Context(), db.Heartbeat, func(ctx context.Context, w *db.Wide) error {
		_, err := w.Beat(ctx, r.id, holding, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A runner that took a task and wrote its key down, and died before redeeming it, never
// acknowledged it. Nobody is bound to it, so the heartbeat's sweep declares nothing about it however
// long it waits, and the bus hands the message to another runner of the pool once AckWait has
// passed, and not before: until then the first may still be redeeming. The other runner redeems it,
// which binds the task to it, and acknowledges, and nothing is left on the queue.
func TestAMessageTakenAndNeverRedeemedGoesToAnotherRunnerAfterAckWait(t *testing.T) {
	tk := dispatched(t)
	first, ok := tk.first.take(t, 5*time.Second)
	if !ok {
		t.Fatal("the first runner was handed nothing")
	}
	tk.first.bus.Close()

	if again, ok := tk.other.take(t, ackWait/2); ok {
		t.Fatalf("a message nobody had acknowledged went to another runner inside AckWait: %+v", again.Task)
	}
	if n := tk.swept(t, time.Now().UTC().Add(time.Hour)); n != 0 {
		t.Errorf("the sweep declared %d tasks lost that nobody had redeemed", n)
	}
	if got, want := tk.task(t), "dispatched -"; got != want {
		t.Errorf("a task taken and never redeemed reads %q, want %q", got, want)
	}

	taken, ok := tk.other.take(t, 3*ackWait)
	if !ok {
		t.Fatal("the message the first runner never redeemed was not handed to another runner once AckWait had passed")
	}
	if taken.Task.TaskID != first.Task.TaskID {
		t.Fatalf("the other runner was handed %s, want %s", taken.Task.TaskID, first.Task.TaskID)
	}
	if code, answer := tk.redeem(t, tk.other, taken.Task); code != http.StatusOK {
		t.Fatalf("the other runner's redemption answered %d: %v", code, answer)
	}
	if err := taken.Held(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := tk.task(t), "dispatched "+tk.other.id; got != want {
		t.Errorf("the redeemed task reads %q, want %q", got, want)
	}
	if n := tk.bus.Outstanding(t, "dmz"); n != 0 {
		t.Errorf("a task redeemed and acknowledged leaves %d messages for the pool to hand out", n)
	}
}

// A runner that redeemed a task holds it, and has not acknowledged it inside AckWait: it is slow, or
// its acknowledgement was lost on the way. The message goes to another runner of the pool, whose
// redemption is refused, since the task is bound to the first, with nothing to start a container
// with. That runner acknowledges the message and starts nothing: nothing is left on the queue, the
// task is still the first runner's, and the first runner redeeming again, as it does to adopt its
// container, is still answered.
func TestARunnerRefusedATaskAnotherHoldsAcknowledgesAndStartsNothing(t *testing.T) {
	tk := dispatched(t)
	first, ok := tk.first.take(t, 5*time.Second)
	if !ok {
		t.Fatal("the first runner was handed nothing")
	}
	if code, answer := tk.redeem(t, tk.first, first.Task); code != http.StatusOK {
		t.Fatalf("the first runner's redemption answered %d: %v", code, answer)
	}

	taken, ok := tk.other.take(t, 3*ackWait)
	if !ok {
		t.Fatal("a message its holder had not acknowledged inside AckWait was not handed on")
	}
	code, answer := tk.redeem(t, tk.other, taken.Task)
	if code != http.StatusConflict {
		t.Fatalf("a redemption of a task another runner holds answered %d: %v", code, answer)
	}
	if _, said := answer["error"]; !said || len(answer) != 1 {
		t.Errorf("the refusal carried more than why it refused, where there is nothing to start: %v", answer)
	}
	if err := taken.Refused(t.Context()); err != nil {
		t.Fatal(err)
	}

	if n := tk.bus.Outstanding(t, "dmz"); n != 0 {
		t.Errorf("a refused task acknowledged by the runner refused it leaves %d messages for the pool to hand out", n)
	}
	if got, want := tk.task(t), "dispatched "+tk.first.id; got != want {
		t.Errorf("the task reads %q, want %q", got, want)
	}
	if code, answer := tk.redeem(t, tk.first, first.Task); code != http.StatusOK {
		t.Errorf("the holder redeeming again answered %d: %v", code, answer)
	}
}

// A runner that redeemed a task and died before acknowledging it leaves the task bound to it and
// silent. The heartbeat's sweep declares it lost three intervals after the redemption, and not
// before, and the message, which nobody acknowledged, goes to the next runner of the pool once
// AckWait has passed. That runner's redemption is refused, the dispatch being lost, and it
// acknowledges the message and starts nothing: the requeue of a lost task is a message of its own.
func TestARunnerThatDiesAfterRedeemingLeavesALossAndAMessageTheNextRunnerDrops(t *testing.T) {
	tk := dispatched(t)
	first, ok := tk.first.take(t, 5*time.Second)
	if !ok {
		t.Fatal("the first runner was handed nothing")
	}
	redeemed := time.Now().UTC()
	if code, answer := tk.redeem(t, tk.first, first.Task); code != http.StatusOK {
		t.Fatalf("the first runner's redemption answered %d: %v", code, answer)
	}
	tk.first.bus.Close()

	if n := tk.swept(t, redeemed.Add(db.LostAfter-5*time.Second)); n != 0 {
		t.Errorf("inside three intervals of its redemption the sweep declared %d tasks lost", n)
	}
	if n := tk.swept(t, redeemed.Add(db.LostAfter+5*time.Second)); n != 1 {
		t.Fatalf("three intervals after its runner redeemed it and went quiet, the sweep declared %d tasks lost", n)
	}
	if got, want := tk.task(t), "lost "+tk.first.id; got != want {
		t.Errorf("the task reads %q, want %q", got, want)
	}

	taken, ok := tk.other.take(t, 3*ackWait)
	if !ok {
		t.Fatal("the message a runner redeemed and never acknowledged was not handed on once AckWait had passed")
	}
	if code, answer := tk.redeem(t, tk.other, taken.Task); code != http.StatusConflict {
		t.Fatalf("a redemption of a lost dispatch answered %d: %v", code, answer)
	}
	if err := taken.Refused(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := tk.bus.Outstanding(t, "dmz"); n != 0 {
		t.Errorf("the message of a lost dispatch, refused and acknowledged, leaves %d messages for the pool to hand out", n)
	}
	if got, want := tk.task(t), "lost "+tk.first.id; got != want {
		t.Errorf("after the refusal the task reads %q, want %q", got, want)
	}
}

// A runner whose redemption bound the task and whose answer never reached it, a client that gave up
// on a slow API or a connection that dropped once the binding had committed, has heard nothing of
// whose the task is. It keeps the key, which its heartbeats go on naming, and redeems again as the
// holder it turns out to be, which is answered as the first time would have been, and acknowledges
// then. The heartbeat's sweep counts a bound task from its redemption and its last heartbeat, so
// it declares nothing lost three intervals after the redemption whose answer was lost, where a
// runner that let go of the key and waited for the message to come round would have left the task
// to be declared lost before AckWait had passed, spending a requeue on a host that was never lost.
func TestARunnerThatNeverHeardItsRedemptionAnsweredKeepsTheKeyAndRedeemsAgain(t *testing.T) {
	tk := dispatched(t)
	first, ok := tk.first.take(t, 5*time.Second)
	if !ok {
		t.Fatal("the first runner was handed nothing")
	}
	redeemed := time.Now().UTC()
	if code, answer := tk.redeem(t, tk.first, first.Task); code != http.StatusOK {
		t.Fatalf("the first runner's redemption answered %d: %v", code, answer)
	}
	// The answer is lost on the way back, and the runner goes on naming the key it kept.
	key := agk.TaskID(first.Task.IdempotencyKey)
	tk.beat(t, tk.first, redeemed.Add(db.HeartbeatInterval), key)
	tk.beat(t, tk.first, redeemed.Add(2*db.HeartbeatInterval), key)

	if n := tk.swept(t, redeemed.Add(db.LostAfter+5*time.Second)); n != 0 {
		t.Errorf("three intervals after a redemption whose answer was lost, the sweep declared %d tasks lost that their runner still named", n)
	}
	if code, answer := tk.redeem(t, tk.first, first.Task); code != http.StatusOK {
		t.Fatalf("the runner redeeming again as the holder it turned out to be answered %d: %v", code, answer)
	}
	if err := first.Held(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := tk.task(t), "dispatched "+tk.first.id; got != want {
		t.Errorf("the task reads %q, want %q", got, want)
	}
	if n := tk.bus.Outstanding(t, "dmz"); n != 0 {
		t.Errorf("a task redeemed again and acknowledged leaves %d messages for the pool to hand out", n)
	}
}
