package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The routes a runner reaches, and the ones it does not.

func withRunners(t *testing.T) (http.Handler, *db.Pool) {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(everything{who: "admin"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	return rt, pool
}

// issue mints a join token the way an administrator would.
func issue(t *testing.T, pool *db.Pool, labels []string) db.JoinToken {
	t.Helper()
	var token db.JoinToken
	err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		token, err = w.IssueJoinToken(ctx, "dmz", labels, "admin", time.Now().UTC().Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func aMachine(token string, labels ...string) api.Join {
	return api.Join{
		Token: token, Labels: labels,
		CPU: 8, MemoryBytes: 1 << 34, DiskBytes: 1 << 38,
		Architecture: "amd64", AgentVersion: "0.2.0",
	}
}

// The whole of what a machine does: joins, and then says it is there.
func TestAMachineJoinsAndThenSaysItIsThere(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, []string{"zone=dmz", "arch=amd64"})

	w, answer := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear, "zone=dmz"))
	if w.Code != http.StatusCreated {
		t.Fatalf("joining answered %d: %s", w.Code, w.Body)
	}
	credential, _ := answer["credential"].(string)
	if credential == "" || answer["pool"] != "dmz" {
		t.Fatalf("joining answered %v", answer)
	}
	if answer["rotate_by"] == nil {
		t.Error("the answer does not say when the credential stops being accepted")
	}

	// Registration is the one route outside both hooks, and it needed no credential.
	w, answer = call(t, h, "POST", "/api/v1/runners/heartbeat", credential, api.Beat{})
	if w.Code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d: %s", w.Code, w.Body)
	}
	if answer["interval_seconds"] == nil {
		t.Errorf("the answer does not say how often to come back: %v", answer)
	}
	if answer["drain"] != nil {
		t.Errorf("a runner that just joined was told to drain: %v", answer)
	}
}

// A runner route reached with no credential, or with one that opens nothing, answers the same
// thing: a machine that gets this joins again rather than retrying.
func TestARunnerRouteRefusesAnythingButARunner(t *testing.T) {
	h, _ := withRunners(t)

	for _, as := range []string{"", "agkrunner_notarealcredentialatallbutlongenoughtolookright", "alice"} {
		w, _ := call(t, h, "POST", "/api/v1/runners/heartbeat", as, api.Beat{})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("a heartbeat as %q answered %d", as, w.Code)
		}
	}
}

// A join token that is wrong, spent, expired or claiming a label it may not all answer the same
// thing, because a machine that gets a different answer for each is a machine somebody is using
// to find out which tokens exist.
func TestEveryWayAJoinFailsAnswersTheSameThing(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, []string{"zone=dmz"})

	// Spend it.
	w, _ := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	if w.Code != http.StatusCreated {
		t.Fatalf("the first join answered %d: %s", w.Code, w.Body)
	}

	var bodies []string
	for _, c := range []struct {
		name string
		join api.Join
	}{
		{"spent", aMachine(token.Clear)},
		{"wrong", aMachine("agkjoin_notarealtokenatallbutlongenoughtolookplausible")},
		{"claiming more", aMachine(issue(t, pool, []string{"zone=dmz"}).Clear, "zone=lan")},
	} {
		w, _ := call(t, h, "POST", "/api/v1/runners", "", c.join)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("a %s token answered %d", c.name, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("the answers differ:\n%s\n%s", bodies[0], bodies[i])
		}
	}
}

// A drained runner is told so at its next heartbeat, which is the only channel there is:
// "there is no separate liveness channel to keep in sync".
func TestADrainedRunnerIsToldAtItsNextHeartbeat(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, nil)

	_, answer := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	credential, _ := answer["credential"].(string)
	runner, _ := answer["runner"].(string)

	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.Drain(ctx, runner, "the host is being retired")
	}); err != nil {
		t.Fatal(err)
	}

	w, said := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, api.Beat{})
	if w.Code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d", w.Code)
	}
	if said["drain"] != true || said["reason"] != "the host is being retired" {
		t.Errorf("a drained runner was told %v", said)
	}

	// And a revoked one is told nothing at all, because its credential opens nothing.
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.Revoke(ctx, runner, "the credential leaked")
	}); err != nil {
		t.Fatal(err)
	}
	w, _ = call(t, h, "POST", "/api/v1/runners/heartbeat", credential, api.Beat{})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a revoked runner's heartbeat answered %d", w.Code)
	}
}

// The inventory is the installation's and not a runner's: "A user never learns which host
// executed a task beyond its runner name and labels."
func TestTheInventoryIsNotSomethingARunnerReads(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, []string{"zone=dmz"})
	_, answer := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear, "zone=dmz"))
	credential, _ := answer["credential"].(string)

	// The administrator sees it.
	w, listing := call(t, h, "GET", "/api/v1/runners", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the inventory answered %d: %s", w.Code, w.Body)
	}
	runners, _ := listing["runners"].([]any)
	if len(runners) != 1 {
		t.Fatalf("the inventory holds %d runners", len(runners))
	}
	first, _ := runners[0].(map[string]any)
	if first["pool"] != "dmz" || first["state"] != "ready" {
		t.Errorf("the runner reads %v", first)
	}

	// A runner credential is not a principal and reaches nothing but its own routes.
	w, _ = call(t, h, "GET", "/api/v1/runners", credential, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("a runner reading the inventory answered %d", w.Code)
	}
}

// A route authorised by a runner credential cannot be registered without something to check one
// against, which is the same refusal a route with no guard gets.
func TestARunnerRouteNeedsSomethingToCheckAgainst(t *testing.T) {
	rt, err := api.NewRouter(api.DenyAll{}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	err = rt.HandleRunner("POST", "/api/v1/runners/heartbeat", api.ForRunner{},
		func(http.ResponseWriter, *http.Request, api.Runner) {})
	if err == nil {
		t.Error("a runner route was registered with nothing to check a credential against")
	}
}
