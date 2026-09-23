package api_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/jackc/pgx/v5"
)

// When a redemption reads a secret value, and what it does with one.
//
// "The runner obtains the value at the last moment, by redeeming at the API the per-task grant the
// controller issued for that one task and that one secret." The last moment is once nothing else
// can refuse the redemption, and before the task is bound to the runner asking, so that a runner
// told it cannot have a secret has not taken a task it cannot run.

// rotated is a secret store a test writes values into as a rotation would, and which remembers
// every read it answered, as namespace/name, in the order they came.
type rotated struct {
	mu     sync.Mutex
	values map[string]string
	broken error
	reads  []string
}

func (r *rotated) Value(_ context.Context, namespace, name string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, namespace+"/"+name)
	if r.broken != nil {
		return nil, r.broken
	}
	v, ok := r.values[namespace+"/"+name]
	if !ok {
		return nil, api.ErrNoSecret
	}
	return []byte(v), nil
}

// holds writes a value, and mends the store if it was broken.
func (r *rotated) holds(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key], r.broken = value, nil
}

// read is every read so far.
func (r *rotated) read() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads)
}

// A secret the store cannot give leaves the task where it was: the store is read before the task
// is bound, and a redemption that cannot answer takes nothing. The runner refused reports that no
// container ran, and that report is what binds it. The one other way the task is redeemed is the
// bus delivering it twice, and that second delivery, redeemed once the store holds the value, is
// given it, after which the first runner is told the task is not its own.
func TestASecretTheStoreCannotGiveLeavesTheTaskUntaken(t *testing.T) {
	for what, broken := range map[string]error{
		"a secret nobody holds":             nil,
		"a secret held that cannot be read": errors.New("secret: finance/stripe was sealed under a master key this installation's keyring does not hold"),
	} {
		t.Run(what, func(t *testing.T) {
			store := &rotated{broken: broken}
			g := withGrants(t, store)
			first, second := g.joined(t), g.joined(t)
			clear, _, _ := g.dispatched(t, []string{"stripe"})

			w, answer := call(t, g.handler, "POST", "/api/v1/tasks/redeem", first, asking(clear))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("%s answered %d: %s", what, w.Code, w.Body)
			}
			if said, _ := answer["error"].(string); !strings.Contains(said, "stripe") {
				t.Errorf("the refusal does not name the secret: %q", said)
			}
			if runner := g.bound(t); runner != nil {
				t.Fatalf("%s bound the task to %s, and a runner that could not be given its secret has not taken it", what, *runner)
			}

			store.holds("finance/stripe", "sk_live_notreal")
			given := g.redeemed(t, second, asking(clear))
			if len(given.Secrets) != 1 || given.Secrets[0].Value != "sk_live_notreal" {
				t.Errorf("the runner redeeming a second delivery was given %+v", given.Secrets)
			}
			if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", first, asking(clear)); w.Code != http.StatusConflict {
				t.Errorf("the first runner, redeeming again once another took the task, answered %d", w.Code)
			}
		})
	}
}

// And the store is read only once nothing else can refuse: not for a grant that opens nothing, a
// version with no tree, an input the object store does not hold, a task another runner holds, or
// one that has ended. Each of those is refused without a value ever being read for it.
func TestASecretIsReadOnlyOnceNothingElseCanRefuse(t *testing.T) {
	store := &rotated{values: map[string]string{"finance/stripe": "sk_live_notreal"}}
	g := withGrants(t, store)
	first, second := g.joined(t), g.joined(t)
	clear, envelope, _ := g.dispatched(t, []string{"stripe"})
	stripe := []db.GrantSecret{{Name: "stripe", Mount: "/agk/secrets/stripe"}}

	unread := func(what string) {
		t.Helper()
		if reads := store.read(); len(reads) != 0 {
			t.Errorf("%s read %v", what, reads)
		}
		if runner := g.bound(t); runner != nil {
			t.Fatalf("%s bound the task to %s", what, *runner)
		}
	}
	for _, c := range []struct {
		name  string
		grant string
		want  int
	}{
		{"a grant that opens nothing", clear + "x", http.StatusUnauthorized},
		{"a version nobody recorded", g.granted(t, db.GrantScope{
			Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "deadbee",
			Inputs:  []db.GrantInput{{Port: "in", Digest: envelope, Items: 1}},
			Secrets: stripe,
		}), http.StatusInternalServerError},
		{"an input the object store does not hold", g.granted(t, db.GrantScope{
			Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "a3f9c1e",
			Inputs:  []db.GrantInput{{Port: "in", Digest: digestOf([]byte("an envelope nobody wrote")), Items: 1}},
			Secrets: stripe,
		}), http.StatusInternalServerError},
	} {
		if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", first, asking(c.grant)); w.Code != c.want {
			t.Errorf("%s answered %d: %s", c.name, w.Code, w.Body)
		}
		unread(c.name)
	}

	// The one redemption that answers reads the one secret the task names, once.
	g.redeemed(t, second, asking(clear))
	if reads := store.read(); !slices.Equal(reads, []string{"finance/stripe"}) {
		t.Fatalf("the redemption read %v", reads)
	}

	// And a task somebody holds, or that has ended, is refused before its secret is read again.
	if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", first, asking(clear)); w.Code != http.StatusConflict {
		t.Errorf("a task another runner holds answered %d", w.Code)
	}
	if _, err := dbtest.Superuser(t, g.super).Exec(t.Context(),
		`update tasks set state = 'succeeded' where id = $1`, grantTaskRow); err != nil {
		t.Fatal(err)
	}
	if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", second, asking(clear)); w.Code != http.StatusConflict {
		t.Errorf("a task that has ended answered %d", w.Code)
	}
	if reads := store.read(); len(reads) != 1 {
		t.Errorf("refusing a task that is not the runner's to work on read %v", reads[1:])
	}
}

// meeting is a store whose reads wait for one another until as many have arrived as it expects:
// runners redeeming at once are then all past their check before any of them is bound. The wait is
// bounded, so that a redemption reading under the task's lock, which makes the others wait for it
// rather than meet it, slows the test down instead of hanging it.
type meeting struct {
	*rotated
	expected int32
	arrived  atomic.Int32
	all      chan struct{}
}

func (m *meeting) Value(ctx context.Context, namespace, name string) ([]byte, error) {
	if m.arrived.Add(1) == m.expected {
		close(m.all)
	}
	select {
	case <-m.all:
	case <-time.After(2 * time.Second):
	}
	return m.rotated.Value(ctx, namespace, name)
}

// Runners redeeming one task at the same moment are each checked and may each read its secret
// before any of them is bound, and the binding decides: one is given the task, and every other is
// told the task is not its own and given no value. The one given it is the one it is bound to, so
// asking again is answered for it and refused for the rest.
func TestOfRunnersRedeemingATaskAtOnceOneTakesIt(t *testing.T) {
	const runners = 6
	store := &meeting{rotated: &rotated{}, expected: runners, all: make(chan struct{})}
	store.holds("finance/stripe", "sk_live_notreal")
	g := withGrants(t, store)
	clear, _, _ := g.dispatched(t, []string{"stripe"})
	credentials := make([]string, runners)
	for i := range credentials {
		credentials[i] = g.joined(t)
	}

	codes := make([]int, len(credentials))
	bodies := make([]string, len(credentials))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, credential := range credentials {
		wg.Go(func() {
			<-start
			w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear))
			codes[i], bodies[i] = w.Code, w.Body.String()
		})
	}
	close(start)
	wg.Wait()

	taken := -1
	for i, code := range codes {
		switch {
		case code == http.StatusOK && taken < 0:
			taken = i
		case code == http.StatusOK:
			t.Errorf("runners %d and %d were both given the task", taken, i)
		case code != http.StatusConflict:
			t.Errorf("runner %d answered %d: %s", i, code, bodies[i])
		case strings.Contains(bodies[i], "sk_live_notreal"):
			t.Errorf("runner %d was refused the task and given its secret: %s", i, bodies[i])
		}
	}
	if taken < 0 {
		t.Fatalf("no runner was given the task: %v", codes)
	}
	for i, credential := range credentials {
		want := http.StatusConflict
		if i == taken {
			want = http.StatusOK
		}
		if w, _ := call(t, g.handler, "POST", "/api/v1/tasks/redeem", credential, asking(clear)); w.Code != want {
			t.Errorf("runner %d, asking again, answered %d", i, w.Code)
		}
	}
}

// The last moment is every redemption, and nothing between the store and the answer keeps a value.
// Nothing reads one before a runner redeems, so a value rotated once the task was dispatched is the
// one its first redemption is given, and each redemption reads every secret the task names once,
// whichever grant of the task it redeems. Nor is one written into the database on the way, where a
// copy of the table would be a copy of the credential.
//
// What asking again answers for a value rotated since the first redemption is left out on purpose.
// The documentation says asking again after a lost answer gets the same answer while the grant
// lives, and a runner adopting its container after a restart redeems again for the values its
// masker matches, which are the ones the container was given, while the store read again answers
// the rotated one. Which of the two is meant is not for a test to decide.
func TestASecretIsReadAtEveryRedemptionAndKeptByNothing(t *testing.T) {
	store := &rotated{}
	store.holds("finance/billing", "bk_live_dispatched")
	store.holds("finance/stripe", "sk_live_dispatched")
	g := withGrants(t, store)
	credential := g.joined(t)
	clear, envelope, _ := g.dispatched(t, []string{"billing", "stripe"})
	if reads := store.read(); len(reads) != 0 {
		t.Fatalf("the store was read before any redemption: %v", reads)
	}

	// The task published again, as a pass that could not record its dispatch does, carries a
	// grant of its own beside the first.
	again := g.granted(t, db.GrantScope{
		Run: grantRun, Step: "render", Workflow: "monthly-invoicing", Commit: "a3f9c1e",
		Inputs: []db.GrantInput{{Port: "in", Digest: envelope, Items: 1}},
		Secrets: []db.GrantSecret{
			{Name: "billing", Mount: "/agk/secrets/billing"},
			{Name: "stripe", Mount: "/agk/secrets/stripe"},
		},
	})

	// Rotated once the task is dispatched, and before anything redeems it.
	store.holds("finance/billing", "bk_live_rotated")
	store.holds("finance/stripe", "sk_live_rotated")
	want := []string{"billing=bk_live_rotated", "stripe=sk_live_rotated"}

	for i, c := range []struct {
		why   string
		grant string
	}{
		{"the first redemption", clear},
		{"asking again after a lost answer", clear},
		{"the grant of the task published again", again},
	} {
		answer := g.redeemed(t, credential, asking(c.grant))
		var got []string
		for _, s := range answer.Secrets {
			got = append(got, s.Name+"="+s.Value)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s was given %v, and the store holds %v", c.why, got, want)
		}
		if reads, want := len(store.read()), 2*(i+1); reads != want {
			t.Errorf("after %s the store was read %d times, and %d redemptions of two secrets read it %d", c.why, reads, i+1, want)
		}
	}

	for _, held := range kept(t, g.super, "bk_live_dispatched", "sk_live_dispatched", "bk_live_rotated", "sk_live_rotated") {
		t.Errorf("a value the store held is kept in the database: %s", held)
	}
}

// kept names each table of the database holding one of the values in some row, written as text or
// as the hexadecimal PostgreSQL writes bytes in, read as the superuser from behind every policy.
func kept(t *testing.T, super string, values ...string) []string {
	t.Helper()
	conn := dbtest.Superuser(t, super)
	rows, err := conn.Query(t.Context(), `select quote_ident(tablename) from pg_tables where schemaname = 'public' order by tablename`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	// A search that read no table would find nothing anywhere.
	for _, table := range []string{"task_grants", "tasks", "secret_values"} {
		if !slices.Contains(tables, table) {
			t.Fatalf("the search does not reach %s, so what it does not find says nothing", table)
		}
	}
	var found []string
	for _, table := range tables {
		for _, v := range values {
			var n int
			if err := conn.QueryRow(t.Context(),
				`select count(*) from `+table+` r where strpos(r::text, $1) > 0 or strpos(r::text, $2) > 0`,
				v, hex.EncodeToString([]byte(v))).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				found = append(found, fmt.Sprintf("%s holds %s in %d rows", table, v, n))
			}
		}
	}
	return found
}

// The store is asked for the secrets the grant names, in the namespace the grant was issued in,
// and for nothing else: not the same name in another namespace, and not another secret the task's
// own namespace holds.
func TestOnlyTheSecretsTheGrantNamesAreRead(t *testing.T) {
	store := &rotated{}
	store.holds("finance/stripe", "sk_live_finance")
	store.holds("ops/stripe", "sk_live_ops")
	store.holds("finance/other", "ot_live_finance")
	g := withGrants(t, store)
	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, []string{"stripe"})

	answer := g.redeemed(t, credential, asking(clear))
	if reads := store.read(); !slices.Equal(reads, []string{"finance/stripe"}) {
		t.Errorf("the redemption asked the store for %v", reads)
	}
	want := []api.Secret{{Name: "stripe", Mount: "/agk/secrets/stripe", Encoding: api.EncodingUTF8, Value: "sk_live_finance"}}
	if !slices.Equal(answer.Secrets, want) {
		t.Errorf("the redemption answered the secrets %+v, want %+v", answer.Secrets, want)
	}
}
