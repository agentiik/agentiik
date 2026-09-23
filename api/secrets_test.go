package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// developing is an installation opted in to the env provider for both namespaces the tests make,
// each under a prefix of its own.
var developing = api.Environment{"finance": "AGENTIIK_SECRET_FINANCE_", "team-ops": "AGENTIIK_SECRET_TEAM_OPS_"}

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
	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing})

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
	w, _ = call(t, h, "PUT", "/api/v1/finance/secrets/ledger", "alice", api.Declare{Provider: "env", Path: "AGENTIIK_SECRET_FINANCE_GENERAL_LEDGER"})
	if w.Code != http.StatusOK {
		t.Fatalf("moving a declaration answered %d: %s", w.Code, w.Body)
	}
	w, one := call(t, h, "GET", "/api/v1/finance/secrets/ledger", "alice", nil)
	if w.Code != http.StatusOK || one["provider"] != "env" || one["path"] != "AGENTIIK_SECRET_FINANCE_GENERAL_LEDGER" {
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

// sealing is a built-in store that keeps what it is given where a test can look, by namespace and
// name, and refuses everything when it is told to.
type sealing struct {
	mu     sync.Mutex
	held   map[string][]byte
	refuse bool
}

func (s *sealing) Write(_ context.Context, ns *db.NS, name string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse {
		return errors.New("the store is not answering")
	}
	if s.held == nil {
		s.held = map[string][]byte{}
	}
	s.held[ns.Namespace()+"/"+name] = bytes.Clone(value)
	return nil
}

func (s *sealing) Forget(_ context.Context, ns *db.NS, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, ns.Namespace()+"/"+name)
	return nil
}

func (s *sealing) holds(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.held[key]
	return value, ok
}

// "A value reaches the built-in store write-only, on the declaration's PUT, for provider builtin:
// the request carries it once, no answer ever returns it, and rotating is writing again." The
// store is handed each value as sent, a PUT with none leaves the stored one where it is, and no
// answer of any route carries one.
func TestABuiltinValueIsWrittenAndNeverAnswered(t *testing.T) {
	store := &sealing{}
	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Values: store})
	const first, second = "sk_live_first", "sk_live_second"

	for _, c := range []struct {
		what, body string
		want       int
		holds      string
	}{
		{"writing a value", `{"provider":"builtin","value":"` + first + `"}`, http.StatusCreated, first},
		{"rotating it", `{"provider":"builtin","value":"` + second + `"}`, http.StatusOK, second},
		{"declaring again with no value", `{"provider":"builtin"}`, http.StatusOK, second},
	} {
		w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", c.body)
		if w.Code != c.want {
			t.Fatalf("%s answered %d: %s", c.what, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), first) || strings.Contains(w.Body.String(), second) || strings.Contains(w.Body.String(), `"value"`) {
			t.Errorf("%s was answered with a value: %s", c.what, w.Body)
		}
		if got, _ := store.holds("finance/billing"); string(got) != c.holds {
			t.Errorf("after %s the store holds %q, want %q", c.what, got, c.holds)
		}
	}
	for _, path := range []string{"/api/v1/finance/secrets/billing", "/api/v1/finance/secrets"} {
		w := sent(t, h, "GET", path, "alice", "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), second) || strings.Contains(w.Body.String(), `"value"`) {
			t.Errorf("GET %s answered %d %s", path, w.Code, w.Body)
		}
	}

	// A value that is not text travels as base64 and is kept as the bytes it names, not as the
	// text that named them.
	if w := sent(t, h, "PUT", "/api/v1/finance/secrets/keystore", "alice", `{"provider":"builtin","value":"//4A","encoding":"base64"}`); w.Code != http.StatusCreated {
		t.Fatalf("writing a value that is not text answered %d: %s", w.Code, w.Body)
	}
	if got, _ := store.holds("finance/keystore"); !bytes.Equal(got, []byte{0xff, 0xfe, 0x00}) {
		t.Errorf("a value written as base64 is kept as %x", got)
	}
}

// A value is refused wherever it would not be kept as sent: for a store that is not the built-in
// one, after the declaration as a second document, which a decoder reading one document would
// never see, empty, in an encoding nobody knows or not in the one it names, or larger than the
// store seals. Nothing is written, the refusal does not repeat the value, and the store is never
// handed it.
func TestAValueWithNowhereToGoIsRefused(t *testing.T) {
	store := &sealing{}
	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing, Values: store})

	const value = "sk_live_notreal"
	for what, c := range map[string]struct {
		body string
		want int
	}{
		"a value for a variable":                 {`{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_BILLING","value":"` + value + `"}`, http.StatusBadRequest},
		"a value after the declaration":          {`{"provider":"builtin"} {"value":"` + value + `"}`, http.StatusBadRequest},
		"an empty value":                         {`{"provider":"builtin","value":""}`, http.StatusBadRequest},
		"an encoding and no value":               {`{"provider":"builtin","encoding":"base64"}`, http.StatusBadRequest},
		"an encoding nobody knows":               {`{"provider":"builtin","value":"` + value + `","encoding":"rot13"}`, http.StatusBadRequest},
		"a value that is not the base64 it says": {`{"provider":"builtin","value":"` + value + `!","encoding":"base64"}`, http.StatusBadRequest},
		"a value larger than the store seals":    {`{"provider":"builtin","value":"` + value + strings.Repeat("s", 1<<20) + `"}`, http.StatusRequestEntityTooLarge},
		"a body larger than any value":           {`{"provider":"builtin","value":"` + value + strings.Repeat("s", 3<<20) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", c.body)
		if w.Code != c.want {
			t.Errorf("%s answered %d, want %d: %s", what, w.Code, c.want, w.Body)
		}
		if strings.Contains(w.Body.String(), value) {
			t.Errorf("the refusal of %s repeats the value: %s", what, w.Body)
		}
		if w, _ := call(t, h, "GET", "/api/v1/finance/secrets/billing", "alice", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s was refused and written anyway, and reads %d", what, w.Code)
		}
	}
	if _, ok := store.holds("finance/billing"); ok {
		t.Error("the store was handed a value every request carrying it was refused for")
	}
}

// A value the installation cannot keep leaves the namespace as it was: with no built-in store
// attached it is a 503 that says so, and with one that fails it is a 500, and in both the
// declaration written in the same transaction is undone rather than left with no value.
func TestAValueTheInstallationCannotKeepWritesNothing(t *testing.T) {
	h, _ := withDeclarations(t, everything{who: "alice"})
	if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", `{"provider":"builtin","value":"sk_live_notreal"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("a value sent to an installation with no built-in store answered %d: %s", w.Code, w.Body)
	}
	if w, _ := call(t, h, "GET", "/api/v1/finance/secrets/billing", "alice", nil); w.Code != http.StatusNotFound {
		t.Errorf("a declaration whose value had nowhere to go was written anyway, and reads %d", w.Code)
	}
	// With no value, the same installation takes the declaration: the value can follow once a
	// store is attached.
	if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", `{"provider":"builtin"}`); w.Code != http.StatusCreated {
		t.Errorf("a declaration with no value answered %d: %s", w.Code, w.Body)
	}

	store := &sealing{}
	h, _ = declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing, Values: store})
	if w, _ := call(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", api.Declare{Provider: "env", Path: "AGENTIIK_SECRET_FINANCE_BILLING"}); w.Code != http.StatusCreated {
		t.Fatalf("declaring answered %d", w.Code)
	}
	store.refuse = true
	if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", `{"provider":"builtin","value":"sk_live_notreal"}`); w.Code != http.StatusInternalServerError {
		t.Errorf("a value the store refused answered %d: %s", w.Code, w.Body)
	}
	if w, one := call(t, h, "GET", "/api/v1/finance/secrets/billing", "alice", nil); w.Code != http.StatusOK || one["provider"] != "env" {
		t.Errorf("a declaration whose value the store refused reads %d %v, and it was env before", w.Code, one)
	}
}

// A secret removed, or moved out of the built-in store, takes its value with it, so that the name
// declared in the built-in store again later does not bring back a credential somebody meant gone.
func TestARemovedSecretTakesItsValueWithIt(t *testing.T) {
	store := &sealing{}
	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing, Values: store})
	written := `{"provider":"builtin","value":"sk_live_notreal"}`

	for _, c := range []struct {
		what, method, body string
		want               int
	}{
		{"removed", "DELETE", "", http.StatusNoContent},
		{"moved to a variable", "PUT", `{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_BILLING"}`, http.StatusOK},
	} {
		if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", written); w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("writing the value answered %d: %s", w.Code, w.Body)
		}
		if w := sent(t, h, c.method, "/api/v1/finance/secrets/billing", "alice", c.body); w.Code != c.want {
			t.Fatalf("the secret %s answered %d: %s", c.what, w.Code, w.Body)
		}
		if _, ok := store.holds("finance/billing"); ok {
			t.Errorf("the store still holds the value of a secret %s", c.what)
		}
	}
}

// With no built-in store attached a value is still forgotten. Forgetting needs no key, and the
// value is a row another process of the installation, attached with its keyring, may have written:
// a secret removed or moved to a variable through these routes leaves nothing for the name
// declared in the built-in store again to bring back.
func TestASecretRemovedWithNoStoreAttachedStillTakesItsValue(t *testing.T) {
	h, pool := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing})
	kept := func() error {
		return pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			return ns.WriteSealed(ctx, "billing", func(version int) (db.SealedValue, error) {
				return db.SealedValue{
					Version: version, Master: "2026-09", Salt: []byte("salt"), WrappedKey: []byte("key"),
					WrapNonce: []byte("wrap"), Ciphertext: []byte("sealed"), Nonce: []byte("nonce"),
				}, nil
			})
		})
	}

	for _, c := range []struct {
		what, method, body string
		want               int
	}{
		{"removed", "DELETE", "", http.StatusNoContent},
		{"moved to a variable", "PUT", `{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_BILLING"}`, http.StatusOK},
	} {
		if w := sent(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", `{"provider":"builtin"}`); w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("declaring billing answered %d: %s", w.Code, w.Body)
		}
		if err := kept(); err != nil {
			t.Fatal(err)
		}
		if w := sent(t, h, c.method, "/api/v1/finance/secrets/billing", "alice", c.body); w.Code != c.want {
			t.Fatalf("the secret %s answered %d: %s", c.what, w.Code, w.Body)
		}
		err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			_, err := ns.SealedValue(ctx, "billing")
			return err
		})
		if !errors.Is(err, db.ErrNoValue) {
			t.Errorf("a secret %s with no store attached left its value, which reads as %v", c.what, err)
		}
	}
}

// A declaration says where a store keeps a value, and one that names no store, or says where in a
// way its store cannot read, is refused before anything is written.
func TestADeclarationTheStoreCannotReadIsRefused(t *testing.T) {
	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing})

	for what, c := range map[string]struct {
		name string
		body string
		want int
	}{
		"a store nobody knows":                     {"billing", `{"provider":"keychain","path":"billing"}`, http.StatusBadRequest},
		"no store at all":                          {"billing", `{"path":"kv/data/finance/billing"}`, http.StatusBadRequest},
		"the built-in store with a path":           {"billing", `{"provider":"builtin","path":"finance/billing"}`, http.StatusBadRequest},
		"a variable with no name":                  {"billing", `{"provider":"env"}`, http.StatusBadRequest},
		"a variable carrying an escape sequence":   {"billing", `{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_\u001b[2J"}`, http.StatusBadRequest},
		"a name the workflow file cannot write":    {"bill.ing", `{"provider":"builtin"}`, http.StatusBadRequest},
		"a name no file could be named after":      {strings.Repeat("a", 256), `{"provider":"builtin"}`, http.StatusBadRequest},
		"a name longer than an index row can hold": {strings.Repeat("q7-Z", 1500), `{"provider":"builtin"}`, http.StatusBadRequest},
		"a path longer than any store's":           {"billing", `{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_` + strings.Repeat("A", 64<<10) + `"}`, http.StatusBadRequest},
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

// "A namespace is confined to its own paths." A declaration is refused wherever the path it gives
// could not be held to its namespace when it is written: env where the installation has not opted
// in, a variable outside the namespace's own prefix, which is where another namespace's and the
// API's own are, and vault anywhere, since nothing gives a namespace its prefix there yet. Only
// what could be confined is written.
func TestADeclarationIsConfinedToItsNamespace(t *testing.T) {
	for _, c := range []struct {
		what string
		env  api.Environment
		ns   string
		body string
	}{
		{"env with no opting in", nil, "finance", `{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_LEDGER"}`},
		{"env in a namespace the installation left out", api.Environment{"finance": "AGENTIIK_SECRET_FINANCE_"}, "team-ops", `{"provider":"env","path":"AGENTIIK_SECRET_TEAM_OPS_LEDGER"}`},
		{"the API's database", developing, "finance", `{"provider":"env","path":"AGENTIIK_DATABASE_URL"}`},
		{"the API's master key", developing, "finance", `{"provider":"env","path":"AGENTIIK_MASTER_KEY"}`},
		{"another namespace's variable", developing, "finance", `{"provider":"env","path":"AGENTIIK_SECRET_TEAM_OPS_LEDGER"}`},
		{"the namespace's prefix in another case", developing, "finance", `{"provider":"env","path":"agentiik_secret_finance_ledger"}`},
		{"a variable no environment can hold", developing, "finance", `{"provider":"env","path":"AGENTIIK_SECRET_FINANCE_../x"}`},
		{"another namespace's Vault path", developing, "finance", `{"provider":"vault","path":"kv/data/team-ops/root-token"}`},
		{"a Vault path climbing out of the namespace", developing, "finance", `{"provider":"vault","path":"kv/data/finance/../team-ops/x"}`},
		{"the namespace's own Vault path", developing, "finance", `{"provider":"vault","path":"kv/data/finance/ledger"}`},
	} {
		h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: c.env})
		if w := sent(t, h, "PUT", "/api/v1/"+c.ns+"/secrets/ledger", "alice", c.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d: %s", c.what, w.Code, w.Body)
		}
		if w, _ := call(t, h, "GET", "/api/v1/"+c.ns+"/secrets/ledger", "alice", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s was refused and written anyway, and reads %d", c.what, w.Code)
		}
	}

	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing})
	if w := sent(t, h, "PUT", "/api/v1/team-ops/secrets/ledger", "alice", `{"provider":"env","path":"AGENTIIK_SECRET_TEAM_OPS_LEDGER"}`); w.Code != http.StatusCreated {
		t.Errorf("a variable under the namespace's own prefix answered %d: %s", w.Code, w.Body)
	}
}

// An installation opting in to env names prefixes that confine, or the routes are not built: no
// prefix begins another namespace's, which would hand the first the second's variables, and each
// is the beginning of a name a variable can have.
func TestAnEnvironmentThatCannotConfineIsRefused(t *testing.T) {
	for what, env := range map[string]api.Environment{
		"a prefix beginning another's": {"team": "AGENTIIK_SECRET_TEAM_", "team-ops": "AGENTIIK_SECRET_TEAM_OPS_"},
		"one prefix for two":           {"finance": "AGENTIIK_SECRET_", "team-ops": "AGENTIIK_SECRET_"},
		"a prefix of nothing":          {"finance": ""},
		"a prefix no variable has":     {"finance": "AGENTIIK-SECRET-FINANCE-"},
	} {
		if _, err := api.NewDeclarations(router(t, api.DenyAll{}), api.DeclarationOptions{Pool: &db.Pool{}, Environment: env}); err == nil {
			t.Errorf("an environment with %s was taken", what)
		}
	}
	if _, err := api.NewDeclarations(router(t, api.DenyAll{}), api.DeclarationOptions{Pool: &db.Pool{}, Environment: developing}); err != nil {
		t.Errorf("an environment giving each namespace a prefix of its own was refused: %s", err)
	}
}

// A declaration another namespace holds is answered exactly as one nobody holds, in a namespace
// that exists or one that does not, so that probing names yields nothing; and it cannot be moved
// or removed from outside.
func TestAnotherNamespacesDeclarationsAreNotFound(t *testing.T) {
	h, _ := declaring(t, everything{who: "alice"}, api.DeclarationOptions{Environment: developing})
	if w, _ := call(t, h, "PUT", "/api/v1/finance/secrets/billing", "alice", api.Declare{Provider: "env", Path: "AGENTIIK_SECRET_FINANCE_BILLING"}); w.Code != http.StatusCreated {
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
	if w, one := call(t, h, "GET", "/api/v1/finance/secrets/billing", "alice", nil); w.Code != http.StatusOK || one["path"] != "AGENTIIK_SECRET_FINANCE_BILLING" {
		t.Errorf("finance's declaration reads %d %v after another namespace tried to remove it", w.Code, one)
	}
}

// Reading a declaration takes workflow:read and writing one takes secret:write, each at the
// namespace, and until v0.3.0 gives anybody either, every one of the four is refused.
//
// Each principal is sent the four against a declaration that exists, because a refusal answers
// 404 and so does an absence: against an empty namespace a route guarded by nothing at all would
// pass for one guarded by the right permission.
func TestTheDeclarationsNeedWhatTheirRoutesSay(t *testing.T) {
	finance := api.Target{Namespace: "finance"}
	const (
		list   = "GET /api/v1/finance/secrets"
		read   = "GET /api/v1/finance/secrets/billing"
		write  = "PUT /api/v1/finance/secrets/billing"
		remove = "DELETE /api/v1/finance/secrets/billing"
	)
	for _, c := range []struct {
		who  string
		auth api.Authorizer
		want map[string]int
	}{
		{"nobody", api.DenyAll{}, map[string]int{list: 404, read: 404, write: 404, remove: 404}},
		{"bob", holder{who: "bob", what: api.WorkflowRead, over: finance}, map[string]int{list: 200, read: 200, write: 404, remove: 404}},
		{"carol", holder{who: "carol", what: api.SecretWrite, over: finance}, map[string]int{list: 404, read: 404, write: 200, remove: 204}},
	} {
		h, pool := withDeclarations(t, c.auth)
		if err := pool.In(t.Context(), "finance", func(ctx context.Context, ns *db.NS) error {
			_, _, err := ns.Declare(ctx, db.Declaration{Name: "billing", Provider: "builtin", DeclaredBy: "alice"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// In this order, so that the removal, when it is allowed, comes after everything that
		// needs the declaration to be there.
		for _, route := range []string{list, read, write, remove} {
			method, path, _ := strings.Cut(route, " ")
			body := ""
			if method == "PUT" {
				body = `{"provider":"builtin"}`
			}
			if w := sent(t, h, method, path, c.who, body); w.Code != c.want[route] {
				t.Errorf("%s: %s answered %d, want %d", c.who, route, w.Code, c.want[route])
			}
		}
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
