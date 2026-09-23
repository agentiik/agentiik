package api_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
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
// is bound, and a redemption that cannot answer takes nothing. So the next runner to redeem the
// task, once the store holds the value, is given it, and the first is then told the task is not
// its own.
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
				t.Errorf("the next runner to redeem was given %+v", given.Secrets)
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
