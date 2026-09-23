package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The credential a runner asks for, and the pool it cannot choose.

// minting records what it was asked for, because what matters here is that the pool comes from
// the runner and never from the request.
type minting struct {
	pools []string
	names []string
	fail  bool
}

func (m *minting) ForRunner(name, pool string, until time.Time) (bus.Credentials, error) {
	m.names = append(m.names, name)
	m.pools = append(m.pools, pool)
	if m.fail {
		return bus.Credentials{}, context.DeadlineExceeded
	}
	return bus.Credentials{
		Kind: bus.Kind, URL: "nats://bus.example.com:4222",
		JWT: "a.signed.credential", Seed: "SUAFAKESEED",
		ExpiresAt: until.UTC().Truncate(time.Second),
	}, nil
}

type consumers struct{ made []string }

func (c *consumers) Consumer(_ context.Context, pool string) error {
	c.made = append(c.made, pool)
	return nil
}

func withBus(t *testing.T, issuer api.BusIssuer, made api.BusConsumers) (http.Handler, *db.Pool) {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	if err := pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, CreatedBy: "admin"})
	}); err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(everything{who: "admin"}, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{
		Pool: pool, BusIssuer: issuer, BusConsumers: made,
	}); err != nil {
		t.Fatal(err)
	}
	return rt, pool
}

func TestARunnerAsksForABusCredentialAndCannotChooseItsPool(t *testing.T) {
	issuer, made := &minting{}, &consumers{}
	h, pool := withBus(t, issuer, made)

	token := issue(t, pool, nil)
	_, joined := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	credential, _ := joined["credential"].(string)
	runner, _ := joined["runner"].(string)

	w, answer := call(t, h, "POST", "/api/v1/bus/token", credential, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("asking for a bus credential answered %d: %s", w.Code, w.Body)
	}
	if answer["kind"] != bus.Kind || answer["jwt"] == nil || answer["seed"] == nil {
		t.Fatalf("the credential reads %v", answer)
	}
	if answer["consumer"] != "dmz" || answer["stream"] != bus.Stream {
		t.Errorf("the answer does not say where to pull from: %v", answer)
	}
	if answer["expires_at"] == nil {
		t.Error("the answer does not say when to come back for the next one")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("an answer carrying a credential says %q", w.Header().Get("Cache-Control"))
	}

	// The pool and the name came from the runner the credential opened.
	if len(issuer.pools) != 1 || issuer.pools[0] != "dmz" || issuer.names[0] != runner {
		t.Errorf("the credential was minted for %v as %v", issuer.pools, issuer.names)
	}
	// And its pool has somewhere to pull from, because a runner cannot make one itself.
	if len(made.made) != 1 || made.made[0] != "dmz" {
		t.Errorf("the consumers made are %v", made.made)
	}

	// A body naming another pool is not a way in: there is nothing in the request that
	// names one, and a field nobody knows is refused rather than half understood.
	w, _ = call(t, h, "POST", "/api/v1/bus/token", credential, map[string]any{"pool": "lan"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a body naming a pool answered %d", w.Code)
	}
	if w := streamed(t, h, "POST", "/api/v1/bus/token", credential, `{"pool":"lan"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a body naming a pool, declaring no length, answered %d", w.Code)
	}
	if len(issuer.pools) != 1 {
		t.Errorf("a refused request still minted something: %v", issuer.pools)
	}
}

func TestABusCredentialIsARunnerRouteAndNothingElse(t *testing.T) {
	h, _ := withBus(t, &minting{}, &consumers{})

	for _, as := range []string{"", "admin", "agkrunner_notarealcredentialbutlongenoughtolookright"} {
		w, _ := call(t, h, "POST", "/api/v1/bus/token", as, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("asking as %q answered %d", as, w.Code)
		}
	}
}

// An installation that mints none says so rather than answering an empty credential.
func TestAnInstallationThatMintsNoBusCredentials(t *testing.T) {
	h, pool := withBus(t, nil, nil)
	token := issue(t, pool, nil)
	_, joined := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	credential, _ := joined["credential"].(string)

	w, _ := call(t, h, "POST", "/api/v1/bus/token", credential, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("an installation with no issuer answered %d", w.Code)
	}
}
