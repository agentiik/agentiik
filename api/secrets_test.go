package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/version"
)

// A namespace's secret declarations: where each value lives, answered to whoever reads the
// namespace's workflows, written by whoever holds secret:write, and never the value.

func withDeclarations(t *testing.T, auth api.Authorizer) (http.Handler, *db.Pool) {
	t.Helper()
	return declaring(t, auth, api.DeclarationOptions{})
}

// declaring is withDeclarations with options of the test's own, the database filled in.
func declaring(t *testing.T, auth api.Authorizer, o api.DeclarationOptions) (http.Handler, *db.Pool) {
	t.Helper()
	pool, super := dbtest.Open(t)
	conn := dbtest.Superuser(t, super)
	if _, err := conn.Exec(t.Context(), `insert into namespaces (name) values ('finance'), ('team-ops')`); err != nil {
		t.Fatal(err)
	}
	rt := router(t, auth)
	o.Pool = pool
	if _, err := api.NewDeclarations(rt, o); err != nil {
		t.Fatal(err)
	}
	return rt, pool
}

// sent is call with a body written by hand, for a body no Go type in this package would produce.
func sent(t *testing.T, h http.Handler, method, path, as, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if as != "" {
		r.Header.Set("Authorization", "Bearer "+as)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The whole of what a namespace does with a declaration: writes it, reads it, moves it and takes
// it away, one secret at a time.
func TestADeclarationIsWrittenAndReadBack(t *testing.T) {
	h, _ := withDeclarations(t, everything{who: "alice"})

	w, answer := call(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", api.Declare{Provider: "builtin"})
	if w.Code != http.StatusCreated {
		t.Fatalf("declaring answered %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Location"); got != "/api/v1/finance/secrets/billing" {
		t.Errorf("the declaration is at %q", got)
	}
	if answer["mount"] != "/agk/secrets/billing" || answer["declared_by"] != "alice" {
		t.Errorf("the declaration was answered as %v", answer)
	}
	if _, ok := answer["path"]; ok {
		t.Errorf("a declaration in the built-in store was answered with a path: %v", answer)
	}

	w, _ = call(t, h, "PUT", "/api/v1/finance/secrets/ledger", "alice", api.Declare{Provider: "env", Path: "AGENTIIK_SECRET_FINANCE_LEDGER"})
	if w.Code != http.StatusCreated {
		t.Fatalf("declaring answered %d: %s", w.Code, w.Body)
	}

	w, listing := call(t, h, "GET", "/api/v1/finance/secrets", "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the listing answered %d: %s", w.Code, w.Body)
	}
	secrets, _ := listing["secrets"].([]any)
	if len(secrets) != 2 {
		t.Fatalf("the listing holds %v", listing)
	}
	first, _ := secrets[0].(map[string]any)
	second, _ := secrets[1].(map[string]any)
	if first["name"] != "billing" || first["provider"] != "builtin" {
		t.Errorf("the listing begins with %v", first)
	}
	if second["name"] != "ledger" || second["provider"] != "env" || second["path"] != "AGENTIIK_SECRET_FINANCE_LEDGER" || second["mount"] != "/agk/secrets/ledger" {
		t.Errorf("the listing ends with %v", second)
	}

	// Declared again elsewhere is the same secret moved, answered 200 rather than 201.
	w, _ = call(t, h, "PUT", "/api/v1/finance/secrets/ledger", "alice", api.Declare{Provider: "vault", Path: "kv/data/finance/ledger"})
	if w.Code != http.StatusOK {
		t.Fatalf("moving a declaration answered %d: %s", w.Code, w.Body)
	}
	w, one := call(t, h, "GET", "/api/v1/finance/secrets/ledger", "alice", nil)
	if w.Code != http.StatusOK || one["provider"] != "vault" || one["path"] != "kv/data/finance/ledger" {
		t.Fatalf("the moved declaration reads %d %v", w.Code, one)
	}

	w, _ = call(t, h, "DELETE", "/api/v1/finance/secrets/ledger", "alice", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("removing a declaration answered %d: %s", w.Code, w.Body)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if w, _ := call(t, h, method, "/api/v1/finance/secrets/ledger", "alice", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s of a removed declaration answered %d", method, w.Code)
		}
	}
}

// A declaration reads the same the moment it is written as every time after, to the microsecond
// and in UTC, whatever the clock that wrote it gave: a client comparing what a PUT answered with
// what it reads next, as Terraform does after every apply, sees nothing move.
func TestADeclarationReadsAsItWasWritten(t *testing.T) {
	paris := time.FixedZone("CEST", 2*60*60)
	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{
		Now: func() time.Time { return time.Date(2026, 9, 23, 15, 32, 45, 945646123, paris) },
	})

	w, written := call(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", api.Declare{Provider: "builtin"})
	if w.Code != http.StatusCreated {
		t.Fatalf("declaring answered %d: %s", w.Code, w.Body)
	}
	_, one := call(t, h, "GET", "/api/v1/finance/secrets/billing", "alice", nil)
	_, listing := call(t, h, "GET", "/api/v1/finance/secrets", "alice", nil)
	secrets, _ := listing["secrets"].([]any)
	if len(secrets) != 1 {
		t.Fatalf("the listing holds %v", listing)
	}
	listed, _ := secrets[0].(map[string]any)

	const want = "2026-09-23T13:32:45.945646Z"
	for what, got := range map[string]any{"the PUT": written["declared_at"], "the GET": one["declared_at"], "the listing": listed["declared_at"]} {
		if got != want {
			t.Errorf("%s answered the declaration's time as %v, and it was stored as %s", what, got, want)
		}
	}
}

// A value has nowhere to go here: a body carrying one is refused rather than accepted and dropped,
// which would tell its author the value had been kept, and nothing is written. That holds for a
// value sent after the declaration as a second document, which a decoder reading one document and
// stopping would never see.
func TestADeclarationWithAValueIsRefused(t *testing.T) {
	h, _ := withDeclarations(t, everything{who: "alice"})

	const value = "sk_live_notreal"
	for what, body := range map[string]string{
		"a value in the declaration":    `{"provider":"builtin","value":"` + value + `"}`,
		"a value after the declaration": `{"provider":"builtin"} {"value":"` + value + `"}`,
	} {
		w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s answered %d: %s", what, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), value) {
			t.Errorf("the refusal of %s repeats the value: %s", what, w.Body)
		}
		if w, _ := call(t, h, "GET", "/api/v1/finance/secrets/billing", "alice", nil); w.Code != http.StatusNotFound {
			t.Errorf("a declaration refused for %s was written anyway, and reads %d", what, w.Code)
		}
	}
}

// A declaration says where a store keeps a value, and one that names no store, or says where in a
// way its store cannot read, is refused before anything is written.
func TestADeclarationTheStoreCannotReadIsRefused(t *testing.T) {
	h, _ := withDeclarations(t, everything{who: "alice"})

	for what, c := range map[string]struct {
		name string
		body string
		want int
	}{
		"a store nobody knows":                     {"billing", `{"provider":"keychain","path":"billing"}`, http.StatusBadRequest},
		"no store at all":                          {"billing", `{"path":"kv/data/finance/billing"}`, http.StatusBadRequest},
		"the built-in store with a path":           {"billing", `{"provider":"builtin","path":"finance/billing"}`, http.StatusBadRequest},
		"a variable with no name":                  {"billing", `{"provider":"env"}`, http.StatusBadRequest},
		"a path carrying an escape sequence":       {"billing", `{"provider":"vault","path":"kv/\u001b[2Jbilling"}`, http.StatusBadRequest},
		"a name the workflow file cannot write":    {"bill.ing", `{"provider":"builtin"}`, http.StatusBadRequest},
		"a name no file could be named after":      {strings.Repeat("a", 256), `{"provider":"builtin"}`, http.StatusBadRequest},
		"a name longer than an index row can hold": {strings.Repeat("q7-Z", 1500), `{"provider":"builtin"}`, http.StatusBadRequest},
		"a body far larger than a declaration is":  {"billing", `{"provider":"vault","path":"` + strings.Repeat("a", 64<<10) + `"}`, http.StatusRequestEntityTooLarge},
		"a body that is not a declaration at all":  {"billing", `["builtin"]`, http.StatusBadRequest},
		"a body that says nothing about the store": {"billing", ``, http.StatusBadRequest},
	} {
		w := sent(t, h, "PUT", "/api/v1/finance/secrets/"+c.name, "alice", c.body)
		if w.Code != c.want {
			t.Errorf("%s answered %d, want %d: %s", what, w.Code, c.want, w.Body)
		}
	}
	w, listing := call(t, h, "GET", "/api/v1/finance/secrets", "alice", nil)
	if secrets, _ := listing["secrets"].([]any); w.Code != http.StatusOK || len(secrets) != 0 {
		t.Errorf("after nothing but refusals the namespace declares %v", listing)
	}
}

// A declaration another namespace holds is answered exactly as one nobody holds, in a namespace
// that exists or one that does not, so that probing names yields nothing; and it cannot be moved
// or removed from outside.
func TestAnotherNamespacesDeclarationsAreNotFound(t *testing.T) {
	h, _ := withDeclarations(t, everything{who: "alice"})
	if w, _ := call(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", api.Declare{Provider: "vault", Path: "kv/data/finance/billing"}); w.Code != http.StatusCreated {
		t.Fatalf("declaring answered %d", w.Code)
	}

	absent := sent(t, h, "GET", "/api/v1/nowhere/secrets/billing", "alice", "")
	if absent.Code != http.StatusNotFound {
		t.Fatalf("a declaration in a namespace nobody created answered %d", absent.Code)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/team-ops/secrets/billing"},
		{"GET", "/api/v1/finance/secrets/nobody"},
		{"DELETE", "/api/v1/team-ops/secrets/billing"},
		{"DELETE", "/api/v1/nowhere/secrets/billing"},
	} {
		w := sent(t, h, c.method, c.path, "alice", "")
		if w.Code != absent.Code || w.Body.String() != absent.Body.String() {
			t.Errorf("%s %s answered %d %s, and absence answers %d %s", c.method, c.path, w.Code, w.Body, absent.Code, absent.Body)
		}
	}
	if w := sent(t, h, "PUT", "/api/v1/nowhere/secrets/billing", "alice", `{"provider":"builtin"}`); w.Code != absent.Code || w.Body.String() != absent.Body.String() {
		t.Errorf("declaring into a namespace nobody created answered %d %s", w.Code, w.Body)
	}

	if _, listing := call(t, h, "GET", "/api/v1/team-ops/secrets", "alice", nil); len(listing["secrets"].([]any)) != 0 {
		t.Errorf("team-ops lists %v, which finance declared", listing)
	}
	if w, one := call(t, h, "GET", "/api/v1/finance/secrets/billing", "alice", nil); w.Code != http.StatusOK || one["path"] != "kv/data/finance/billing" {
		t.Errorf("finance's declaration reads %d %v after another namespace tried to remove it", w.Code, one)
	}
}

// Reading a declaration takes workflow:read and writing one takes secret:write, each at the
// namespace, and until v0.3.0 gives anybody either, every one of the four is refused.
func TestTheDeclarationsNeedWhatTheirRoutesSay(t *testing.T) {
	finance := api.Target{Namespace: "finance"}

	h, _ := withDeclarations(t, api.DenyAll{})
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/finance/secrets", ""},
		{"GET", "/api/v1/finance/secrets/billing", ""},
		{"PUT", "/api/v1/finance/secrets/billing", `{"provider":"builtin"}`},
		{"DELETE", "/api/v1/finance/secrets/billing", ""},
	} {
		if w := sent(t, h, c.method, c.path, "alice", c.body); w.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d with nothing granted", c.method, c.path, w.Code)
		}
	}

	h, _ = withDeclarations(t, holder{who: "bob", what: api.WorkflowRead, over: finance})
	if w := sent(t, h, "GET", "/api/v1/finance/secrets", "bob", ""); w.Code != http.StatusOK {
		t.Errorf("reading the declarations with workflow:read answered %d", w.Code)
	}
	if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "bob", `{"provider":"builtin"}`); w.Code != http.StatusNotFound {
		t.Errorf("declaring with workflow:read alone answered %d", w.Code)
	}

	h, _ = withDeclarations(t, holder{who: "carol", what: api.SecretWrite, over: finance})
	if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "carol", `{"provider":"builtin"}`); w.Code != http.StatusCreated {
		t.Errorf("declaring with secret:write answered %d: %s", w.Code, w.Body)
	}
	if w := sent(t, h, "DELETE", "/api/v1/finance/secrets/billing", "carol", ""); w.Code != http.StatusNoContent {
		t.Errorf("removing with secret:write answered %d", w.Code)
	}
}

// guarded is a secret store that fails the test when anything reads it while it is not expecting
// to be read, and counts the reads it was expecting.
type guarded struct {
	t        *testing.T
	expected atomic.Bool
	read     atomic.Int32
}

func (g *guarded) Value(_ context.Context, namespace, name string) ([]byte, error) {
	if !g.expected.Load() {
		g.t.Errorf("the secret %s/%s was read by a request that is not a redemption", namespace, name)
		return nil, api.ErrNoSecret
	}
	g.read.Add(1)
	return []byte("sk_live_notreal"), nil
}

// "The API is the only component that reads the store", and inside the API one route does: the
// redemption, which a runner reaches with a task's grant. Every other route of the whole surface,
// the declarations above all, is sent a request with a store that fails the test if it is read.
func TestNoRouteButRedemptionReadsASecretValue(t *testing.T) {
	store := &guarded{t: t}
	g := withGrants(t, store)

	rt := router(t, everything{who: "admin"})
	versions, err := version.New(g.pool, version.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewServer(rt, api.ServerOptions{Pool: g.pool, Versions: versions, Objects: g.objects}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewRunners(rt, api.RunnerOptions{Pool: g.pool, Objects: g.objects, URLs: g.signed, Secrets: store}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewObjects(rt, g.signed); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewDeclarations(rt, api.DeclarationOptions{Pool: g.pool}); err != nil {
		t.Fatal(err)
	}
	g.handler = rt

	credential := g.joined(t)
	clear, _, _ := g.dispatched(t, []string{"stripe"})
	if w, _ := call(t, rt, "PUT", "/api/v1/finance/secrets/stripe", "admin", api.Declare{Provider: "builtin"}); w.Code != http.StatusCreated {
		t.Fatalf("declaring the task's secret answered %d: %s", w.Code, w.Body)
	}

	filled := strings.NewReplacer(
		"{namespace}", "finance", "{workflow}", "monthly-invoicing", "{commit}", aCommit,
		"{run}", grantRun, "{name}", "stripe", "{pool}", "dmz",
		"{key...}", "finance/sha256/"+strings.Repeat("0", 64),
	)
	redemption, err := json.Marshal(asking(clear))
	if err != nil {
		t.Fatal(err)
	}
	var redeem *api.Route
	routes := rt.Routes()
	for i, route := range routes {
		if route.Method == "POST" && route.Pattern == "/api/v1/tasks/redeem" {
			redeem = &routes[i]
			continue
		}
		as := "admin"
		switch {
		case route.Runner:
			as = credential
		case route.Public:
			as = ""
		}
		// Twice: once saying nothing, and once carrying everything a redemption carries, so
		// that a route which would turn a grant into values if it were handed one is handed one.
		for _, body := range []string{"{}", string(redemption)} {
			sent(t, rt, route.Method, filled.Replace(route.Pattern), as, body)
		}
	}
	if redeem == nil {
		t.Fatal("the surface has no redemption")
	}
	if !redeem.Runner {
		t.Fatalf("the redemption is guarded as %+v, and only a runner holding a task's grant reaches it", *redeem)
	}
	if len(routes) < 15 {
		t.Fatalf("the walk reached %d routes, and the four constructors register more than that", len(routes))
	}

	// And the store that was refused to everything else is the one the redemption reads, so that
	// its silence above is the other routes not asking rather than a store nothing was wired to.
	store.expected.Store(true)
	if w, _ := call(t, rt, "POST", "/api/v1/tasks/redeem", credential, asking(clear)); w.Code != http.StatusOK {
		t.Fatalf("the redemption answered %d: %s", w.Code, w.Body)
	}
	if store.read.Load() != 1 {
		t.Fatalf("the redemption read the store %d times for one secret", store.read.Load())
	}
}
