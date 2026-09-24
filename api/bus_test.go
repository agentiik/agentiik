package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
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

	// revoked is each runner minted the narrower credential a revoked runner finishes its grace
	// with, and until when.
	revoked []string
	until   []time.Time

	// full is until when each ForRunner credential was minted.
	full []time.Time
}

func (m *minting) ForRunner(name, pool string, until time.Time) (bus.Credentials, error) {
	m.names = append(m.names, name)
	m.pools = append(m.pools, pool)
	m.full = append(m.full, until)
	if m.fail {
		return bus.Credentials{}, context.DeadlineExceeded
	}
	return bus.Credentials{
		Kind: bus.Kind, URL: "nats://bus.example.com:4222",
		JWT: "a.signed.credential", Seed: "SUAFAKESEED",
		ExpiresAt: until.UTC().Truncate(time.Second),
	}, nil
}

func (m *minting) ForRevokedRunner(name string, until time.Time) (bus.Credentials, error) {
	m.revoked = append(m.revoked, name)
	m.until = append(m.until, until)
	return bus.Credentials{
		Kind: bus.Kind, URL: "nats://bus.example.com:4222",
		JWT: "a.narrower.credential", Seed: "SUAFAKESEED",
		ExpiresAt: until.UTC().Truncate(time.Second),
	}, nil
}

// consumers records each pool whose consumer it was asked to make ready, and refuses every one
// with refuse where it is set, as a bus refuses a control plane whose credential has expired.
type consumers struct {
	made   []string
	refuse error
}

func (c *consumers) Consumer(_ context.Context, pool string) error {
	if c.refuse != nil {
		return c.refuse
	}
	c.made = append(c.made, pool)
	return nil
}

// errExpired is what the bus answers a control plane whose credential has expired.
var errExpired = errors.New("nats: authorization violation: user authentication expired")

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
	// And nothing was asked of the bus: the pool's consumer was made ready when the pool was
	// created, and asking again would put the route behind the control plane's credential.
	if len(made.made) != 0 {
		t.Errorf("minting a runner's credential made the consumers of %v ready", made.made)
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

// The route mints with the account seed and asks nothing of the bus, so a runner is still given its
// credential once the control plane's own has expired and the bus refuses the API's connection.
// Before, every runner lost the bus within the hour of that expiry.
func TestARunnerIsGivenItsBusCredentialOnceTheControlPlanesHasExpired(t *testing.T) {
	issuer, refused := &minting{}, &consumers{refuse: errExpired}
	h, pool := withBus(t, issuer, refused)
	token := issue(t, pool, nil)
	_, joined := call(t, h, "POST", "/api/v1/runners", "", aMachine(token.Clear))
	credential, _ := joined["credential"].(string)

	w, answer := call(t, h, "POST", "/api/v1/bus/token", credential, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("with the control plane's credential expired, asking for a bus credential answered %d: %s", w.Code, w.Body)
	}
	if answer["jwt"] != "a.signed.credential" || answer["consumer"] != "dmz" {
		t.Errorf("the credential reads %v", answer)
	}
}

// A pool's consumer is made ready as the pool is created, in the same transaction: a bus that
// refuses it leaves no pool behind, for runners to join and find nothing to pull from, and the
// same pool is created once the bus takes it.
func TestAPoolsQueueIsMadeReadyAsThePoolIsCreated(t *testing.T) {
	made := &consumers{}
	h, _ := withBus(t, &minting{}, made)
	ask := api.RunnerPool{Pool: api.Pool{Name: "lan", Labels: []string{"zone=lan"}, Namespaces: []string{}, Ceilings: &api.Ceilings{}}}

	made.refuse = errExpired
	w, _ := call(t, h, "POST", "/api/v1/runner-pools", "admin", ask)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "queue") {
		t.Errorf("a pool whose queue the bus refused answered %d: %s", w.Code, w.Body)
	}
	_, listing := call(t, h, "GET", "/api/v1/runner-pools", "admin", nil)
	if strings.Contains(fmt.Sprint(listing), "lan") {
		t.Errorf("a pool whose queue the bus refused was created all the same: %v", listing)
	}

	made.refuse = nil
	if w, _ := call(t, h, "POST", "/api/v1/runner-pools", "admin", ask); w.Code != http.StatusCreated {
		t.Fatalf("creating the pool once the bus takes it answered %d: %s", w.Code, w.Body)
	}
	if !slices.Equal(made.made, []string{"lan"}) {
		t.Errorf("creating the pool lan made the consumers of %v ready", made.made)
	}
}

// As the API starts, every pool's consumer is made ready, the pool default the installation was
// migrated with among them, since no request creates that one. A bus that refuses one refuses the
// start, naming the pool.
func TestEveryPoolsQueueIsMadeReadyAsTheAPIStarts(t *testing.T) {
	made := &consumers{}
	_, pool := withBus(t, &minting{}, made)
	if err := api.ReadyQueues(t.Context(), pool, made); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(slices.Values(made.made))
	if !slices.Equal(got, []string{"default", "dmz"}) {
		t.Errorf("starting made the consumers of %v ready", got)
	}

	made.refuse = errExpired
	if err := api.ReadyQueues(t.Context(), pool, made); err == nil || !errors.Is(err, errExpired) || !strings.Contains(err.Error(), "pool") {
		t.Errorf("a bus refusing every consumer answered %v", err)
	}
}
