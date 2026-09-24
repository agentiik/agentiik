package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The routes a runner reaches, and the ones it does not.

func withRunners(t *testing.T) (http.Handler, *db.Pool) {
	t.Helper()
	return withRunnersAuthorizedBy(t, everything{who: "admin"})
}

// withRunnersAuthorizedBy is withRunners with the principals the authorizer allows, rather than
// an administrator allowed everything.
func withRunnersAuthorizedBy(t *testing.T, auth api.Authorizer) (http.Handler, *db.Pool) {
	t.Helper()
	h, pool, _ := runnersOn(t, auth)
	return h, pool
}

// runnersOn is withRunnersAuthorizedBy, answering also the superuser's connection string, for a
// test that writes what only the controller writes.
func runnersOn(t *testing.T, auth api.Authorizer) (http.Handler, *db.Pool, string) {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}

	// What an administrator creates before any machine exists.
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{
			Name: "dmz", Labels: []string{"zone=dmz", "arch=amd64"}, CreatedBy: "admin",
		})
	}); err != nil {
		t.Fatal(err)
	}

	rt, err := api.NewRouter(auth, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	return rt, pool, super
}

// issue mints a join token the way an administrator would.
func issue(t *testing.T, pool *db.Pool, labels []string) db.JoinToken {
	t.Helper()
	var token db.JoinToken
	err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		var err error
		now := time.Now().UTC()
		token, err = w.IssueJoinToken(ctx, "dmz", labels, "admin", now, now.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// aKey is the public half of a host's keypair, as the wire's own example writes one.
const aKey = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAXiy2zvWwTpj67NwwKIgCbjFcQdrNAboeffNXm+aJUcM=\n-----END PUBLIC KEY-----\n"

func aMachine(token string, labels ...string) api.Join {
	if labels == nil {
		labels = []string{}
	}
	return api.Join{
		Token: token, PublicKey: aKey, Labels: labels,
		Capacity:     &api.Capacity{VCPU: 8, Memory: "16Gi", Disk: "256Gi"},
		Architecture: "amd64", AgentVersion: "0.2.0",
	}
}

// aBeat is a heartbeat as the wire's example writes one, from the runner named and holding the keys
// given.
func aBeat(runner string, holding ...agk.TaskID) api.Beat {
	if holding == nil {
		holding = []agk.TaskID{}
	}
	return api.Beat{
		Runner: runner, AgentVersion: "0.2.0", State: "ready", Concurrency: 8,
		Tasks: holding, SentAt: time.Date(2026, 9, 10, 6, 41, 9, 104e6, time.UTC),
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
	runner, _ := answer["runner"].(string)
	if credential == "" || answer["pool"] != "dmz" {
		t.Fatalf("joining answered %v", answer)
	}
	if answer["rotate_by"] == nil {
		t.Error("the answer does not say when the credential stops being accepted")
	}

	// Registration is the one route outside both hooks, and it needed no credential.
	w, answer = call(t, h, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner))
	if w.Code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d: %s", w.Code, w.Body)
	}
	if answer["drain"] != false {
		t.Errorf("a runner that just joined was told drain: %v", answer["drain"])
	}
	if _, there := answer["reason"]; there {
		t.Errorf("a runner nobody drained was given a reason: %v", answer)
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

// The join reads a body from anybody on the network, so what reading one costs is what anybody can
// make the API spend: it is held to a small body and a count of labels, and a body past either is
// refused with 413 before the token in it is looked at, as a body the heartbeat does not read is.
func TestAJoinLargerThanAMachineIsDescribedByIsTooLarge(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, []string{"zone=dmz"})

	many := make([]string, 1025)
	for i := range many {
		many[i] = fmt.Sprintf("l%d", i)
	}
	long := aMachine(token.Clear, "zone=dmz")
	long.AgentVersion = strings.Repeat("0", 64<<10)
	for what, join := range map[string]api.Join{
		"more labels than a machine is described by": aMachine(token.Clear, many...),
		"a body larger than a join is":               long,
	} {
		w, answer := call(t, h, "POST", "/api/v1/runners", "", join)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("a join carrying %s answered %d: %s", what, w.Code, w.Body)
		}
		if said, _ := answer["error"].(string); said == "" {
			t.Errorf("a join carrying %s was refused with no reason", what)
		}
	}

	// The token was never looked at, and a machine sending what a join is joins with it.
	if w, _ := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear, "zone=dmz")); w.Code != http.StatusCreated {
		t.Errorf("the token of two refused joins answered %d: %s", w.Code, w.Body)
	}
}

// A drained runner is told so at its next heartbeat, which is the only channel there is:
// "there is no separate liveness channel to keep in sync". So is a revoked one, which is still
// heard in its grace, because "revoking a credential never destroys work already done".
func TestADrainedRunnerIsToldAtItsNextHeartbeat(t *testing.T) {
	h, pool := withRunners(t)
	token := issue(t, pool, nil)

	_, answer := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	credential, _ := answer["credential"].(string)
	runner, _ := answer["runner"].(string)

	if w, _ := call(t, h, "POST", "/api/v1/runners/"+runner+"/drain", "admin", api.Order{Reason: "the host is being retired"}); w.Code != http.StatusOK {
		t.Fatalf("draining answered %d: %s", w.Code, w.Body)
	}
	w, said := call(t, h, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner))
	if w.Code != http.StatusOK {
		t.Fatalf("the heartbeat answered %d", w.Code)
	}
	if said["drain"] != true || said["reason"] != "the host is being retired" {
		t.Errorf("a drained runner was told %v", said)
	}
	if _, there := said["results_accepted_until"]; there {
		t.Errorf("a drained runner nobody revoked was given a grace: %v", said)
	}

	if w, _ := call(t, h, "POST", "/api/v1/runners/"+runner+"/revoke", "admin", api.Order{Reason: "the credential leaked"}); w.Code != http.StatusOK {
		t.Fatalf("revoking answered %d: %s", w.Code, w.Body)
	}
	w, said = call(t, h, "POST", "/api/v1/runners/heartbeat", credential, aBeat(runner))
	if w.Code != http.StatusOK {
		t.Fatalf("a revoked runner's heartbeat in its grace answered %d", w.Code)
	}
	if said["drain"] != true || said["reason"] != "the credential leaked" || said["results_accepted_until"] == nil {
		t.Errorf("a revoked runner was told %v", said)
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
