package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/jackc/pgx/v5"
)

// What sharing does not expose, held against every route serve builds, as a principal holding
// nothing there asks and as the owner of everything there asks: "the existence of workflows they
// cannot read, in any listing, search result or error message", "secret values, at any role", and
// "runner internals".

// tenants is an installation serve built, holding one tenant's namespace, finance, with everything
// a route can name in it, and principals standing in every relation to it.
//
// alice owns finance. mallory owns hr and holds nothing in finance. oscar operates finance/payroll
// and holds nothing else there, so that finance is a namespace he sees and monthly-invoicing a
// workflow of it he does not. victor views finance, and walter edits monthly-invoicing alone. carol
// administers the installation and holds no grant.
type tenants struct {
	t  *testing.T
	in *installation

	// as are how each principal asks, bearing a token of theirs, how mallory asks with a token
	// narrowed to hr, and how her browser does, carrying her session from the public URL's origin.
	as map[string]asker

	// run is a run of finance/monthly-invoicing, and grant and workflowGrant a grant on finance
	// and one on monthly-invoicing, neither of them oscar's or mallory's.
	run, grant, workflowGrant string

	// token is the identifier of finance/nightly's API token, and notification and credential
	// the identifiers of one of alice's notifications and of her password.
	token, notification, credential string

	// runner is a runner of the pool dmz, credential its credential, and object the key of an object
	// finance holds.
	runner, runnerCredential, object string

	// admin is the database as the role that migrated it, which writes what no route does.
	admin config.Database

	// values are the secrets finance holds: billing's, sealed in the built-in store, and
	// ledger's, in the API's environment.
	values []string
}

// asker is how a request is sent: bearing a token, or carrying a session from the public URL's
// origin.
type asker struct {
	bearer  string
	session *http.Cookie
}

// tenantsOrigin is the origin of the public URL servingSettings names, which a session's requests
// come from.
const tenantsOrigin = "https://agentiik.example.com"

// runnerVersion is the agent version finance's runner joins with, which nobody but an administrator
// reads.
const runnerVersion = "0.3.0-build-host-7"

// ledgerVariable is the variable ledger's value is kept in, under the prefix finance is given.
const ledgerVariable = "AGK_DEV_FINANCE_LEDGER"

// someTenants builds the installation and everything in it through its routes, as the bootstrap
// token and alice would, save the principals and their tokens, which are written as a sign-in would
// leave them.
func someTenants(t *testing.T) *tenants {
	t.Helper()
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	bootstrapped(t, database.Application)
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	s := servingSettings(t, database.Application, dir, natsFrom(t, dir))
	s.EnvPrefixes = map[string]string{"finance": "AGK_DEV_FINANCE_"}
	ledger := "ledger-" + randomHex(t)
	t.Setenv(ledgerVariable, ledger)
	in, err := open(t.Context(), s, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.close)
	x := &tenants{t: t, in: in, as: map[string]asker{}, admin: database.Admin}

	// The principals, and a token for each.
	now := time.Now().UTC()
	x.credential = ulid.New()
	err = in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{
			{Login: "alice", DisplayName: "Alice"}, {Login: "mallory", DisplayName: "Mallory"},
			{Login: "oscar", DisplayName: "Oscar"}, {Login: "carol", DisplayName: "Carol", Admin: true},
			{Login: "victor", DisplayName: "Victor"}, {Login: "walter", DisplayName: "Walter"},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
			value := "agktoken_" + u.Login + "_" + randomHex(t)
			hash := sha256.Sum256([]byte(value))
			if err := w.MintToken(ctx, db.APIToken{
				ID: ulid.New(), Hash: hash[:], Principal: u.Login, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			}); err != nil {
				return err
			}
			x.as[u.Login] = asker{bearer: value}
		}
		// And mallory's token narrowed to hr, which reaches nothing in finance whatever she holds.
		narrowed := "agktoken_mallory_hr_" + randomHex(t)
		hash := sha256.Sum256([]byte(narrowed))
		if err := w.MintToken(ctx, db.APIToken{
			ID: ulid.New(), Hash: hash[:], Principal: "mallory", Within: []string{"hr"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			return err
		}
		x.as["mallory's token for hr"] = asker{bearer: narrowed}
		if err := w.CreateGroup(ctx, "auditors"); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{
			ID: x.credential, Login: "alice", Type: db.CredentialPassword, PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{
			ID: "mallory-passkey", Login: "mallory", Type: db.CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16),
		}); err != nil {
			return err
		}
		session, err := api.OpenSession(ctx, w, "mallory", api.OpenedBy{Credential: "mallory-passkey"}, now)
		x.as["mallory's browser"] = asker{session: session}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// finance, owned by alice, and hr, owned by mallory, so that she is somebody holding something.
	for _, ns := range []api.NamespaceRecord{{Name: "finance", Owner: "alice"}, {Name: "hr", Owner: "mallory"}} {
		x.must("POST", "/api/v1/namespaces", asker{bearer: theToken}, ns, http.StatusCreated)
	}

	// Two workflows, a run of one, and a secret of each provider.
	for _, workflow := range []string{"monthly-invoicing", "payroll"} {
		x.must("PUT", "/api/v1/finance/workflows/"+workflow+"/versions/"+theCommit, x.as["alice"], aPush(t), http.StatusOK)
	}
	started := x.answer(x.must("POST", "/api/v1/finance/workflows/monthly-invoicing/runs", x.as["alice"],
		api.Start{Commit: theCommit, Inputs: map[string]any{"orders": []any{}}}, http.StatusAccepted))
	x.run, _ = started["run"].(string)
	billing := "sk_live_" + randomHex(t)
	x.must("PUT", "/api/v1/finance/secrets/billing", x.as["alice"], api.Declare{Provider: "builtin", Value: &billing}, http.StatusCreated)
	x.must("PUT", "/api/v1/finance/secrets/ledger", x.as["alice"], api.Declare{Provider: "env", Path: ledgerVariable}, http.StatusCreated)
	x.values = []string{billing, ledger}

	// oscar operates payroll, victor views finance and walter edits monthly-invoicing; the auditors
	// view finance, by carol's power, which alice is told of, and edit monthly-invoicing.
	x.must("POST", "/api/v1/finance/workflows/payroll/grants", x.as["alice"], api.GrantRequest{Principal: "oscar", Role: "operator"}, http.StatusCreated)
	x.must("POST", "/api/v1/finance/grants", x.as["alice"], api.GrantRequest{Principal: "victor", Role: "viewer"}, http.StatusCreated)
	x.must("POST", "/api/v1/finance/workflows/monthly-invoicing/grants", x.as["alice"], api.GrantRequest{Principal: "walter", Role: "editor"}, http.StatusCreated)
	x.grant, _ = x.answer(x.must("POST", "/api/v1/finance/grants", x.as["carol"],
		api.GrantRequest{Principal: "group:auditors", Role: "viewer"}, http.StatusCreated))["id"].(string)
	x.workflowGrant, _ = x.answer(x.must("POST", "/api/v1/finance/workflows/monthly-invoicing/grants", x.as["alice"],
		api.GrantRequest{Principal: "group:auditors", Role: "editor"}, http.StatusCreated))["id"].(string)
	me := x.answer(x.must("GET", "/api/v1/me", x.as["alice"], nil, http.StatusOK))
	if told, _ := me["notifications"].([]any); len(told) == 1 {
		x.notification, _ = told[0].(map[string]any)["id"].(string)
	}

	// A service account of finance, and a token of it.
	x.must("POST", "/api/v1/service-accounts", x.as["alice"], api.NewServiceAccount{Namespace: "finance", Name: "nightly"}, http.StatusCreated)
	issued := x.answer(x.must("POST", "/api/v1/auth/tokens", x.as["alice"], api.TokenRequest{Principal: "finance/nightly"}, http.StatusCreated))
	x.token, _ = issued["api_token"].(map[string]any)["id"].(string)

	// A pool, and a machine joined to it.
	x.must("POST", "/api/v1/runner-pools", asker{bearer: theToken}, aPool(), http.StatusCreated)
	joinToken, _ := x.answer(x.must("POST", "/api/v1/runner-pools/dmz/join-tokens", asker{bearer: theToken},
		api.Issue{Labels: []string{"zone=dmz"}}, http.StatusCreated))["join_token"].(map[string]any)["token"].(string)
	machine := aMachine(joinToken)
	machine.AgentVersion, machine.Capacity = runnerVersion, &api.Capacity{VCPU: 7, Memory: "13Gi", Disk: "77Gi"}
	joined := x.answer(x.must("POST", "/api/v1/runners", asker{}, machine, http.StatusCreated))
	x.runner, _ = joined["runner"].(string)
	x.runnerCredential, _ = joined["credential"].(string)

	// The object the push stored for the workflow file, which finance holds.
	sum := sha256.Sum256([]byte(theWorkflow))
	x.object = "finance/sha256/" + hex.EncodeToString(sum[:])

	for name, value := range map[string]string{
		"a run": x.run, "a grant": x.grant, "a workflow's grant": x.workflowGrant, "a token": x.token,
		"a notification": x.notification, "a runner": x.runner,
	} {
		if value == "" {
			t.Fatalf("the installation holds no %s to ask about", name)
		}
	}
	return x
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ask sends one request to the installation's router as as, with body sent as it is where it is a
// string and as its JSON otherwise.
func (x *tenants) ask(ctx context.Context, method, path string, as asker, body any) *httptest.ResponseRecorder {
	x.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			x.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	r := httptest.NewRequestWithContext(ctx, method, path, reader)
	if reader != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if as.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+as.bearer)
	}
	if as.session != nil {
		r.AddCookie(as.session)
		r.Header.Set("Origin", tenantsOrigin)
	}
	w := httptest.NewRecorder()
	x.in.router.ServeHTTP(w, r)
	return w
}

// must is ask, failing the test on any status but status.
func (x *tenants) must(method, path string, as asker, body any, status int) *httptest.ResponseRecorder {
	x.t.Helper()
	w := x.ask(x.t.Context(), method, path, as, body)
	if w.Code != status {
		x.t.Fatalf("%s %s answered %d, want %d: %s", method, path, w.Code, status, w.Body)
	}
	return w
}

func (x *tenants) answer(w *httptest.ResponseRecorder) map[string]any {
	x.t.Helper()
	var a map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		x.t.Fatalf("the answer %s is not an object: %s", w.Body, err)
	}
	return a
}

// parameter is a path parameter of a pattern, {name} or {name...}.
var parameter = regexp.MustCompile(`\{([a-z]+)(\.\.\.)?\}`)

// staticAssets are the routes whose parameters name nothing a principal holds: the sign-in page's
// own files, the same for everybody.
var staticAssets = map[string]bool{"GET /auth/assets/{name}": true}

// present is what each parameter of a route names where it names something the installation holds,
// and absent where it names nothing: a namespace, a workflow, a run, a secret, a grant, a token or
// anything else no row holds, under a name its grammar takes, so that it is the absence of the thing
// and not the refusal of a name that is asked about.
func (x *tenants) present(route string) map[string]string {
	named := map[string]string{
		"namespace": "finance", "ns": "finance", "workflow": "monthly-invoicing", "run": x.run,
		"step": "normalize", "port": "ok", "commit": theCommit, "login": "alice", "group": "auditors",
		"runner": x.runner, "pool": "dmz", "key": x.object,
		"uri": url.PathEscape("agk://run/" + x.run + "/normalize/ok/invoice.pdf"),
	}
	switch {
	case strings.Contains(route, "/auth/assets/"):
		named["name"] = "page.js"
	case strings.Contains(route, "/secrets/"):
		named["name"] = "billing"
	case strings.Contains(route, "/outputs/"):
		named["name"] = "invoices"
	case strings.Contains(route, "/service-accounts/"):
		named["name"] = "nightly"
	}
	switch {
	case strings.Contains(route, "/workflows/{workflow}/grants/"):
		named["id"] = x.workflowGrant
	case strings.Contains(route, "/grants/"):
		named["id"] = x.grant
	case strings.Contains(route, "/auth/tokens/"):
		named["id"] = x.token
	case strings.Contains(route, "/me/notifications/"):
		named["id"] = x.notification
	case strings.Contains(route, "/me/credentials/"):
		named["id"] = x.credential
	}
	return named
}

func absent() map[string]string {
	nowhere := strings.ToLower(ulid.New())
	named := map[string]string{
		"namespace": "nowhere", "ns": "nowhere", "workflow": "nothing", "run": ulid.New(),
		"step": "normalize", "port": "ok", "commit": theCommit, "login": "nobody", "group": "nobody",
		"runner": nowhere, "pool": "nowhere", "key": "nowhere/sha256/" + strings.Repeat("0", 64),
		"uri":  url.PathEscape("agk://run/" + ulid.New() + "/normalize/ok/invoice.pdf"),
		"name": "nothing", "id": ulid.New(),
	}
	return named
}

// identities are the parameters that say which thing a route is about, in the order a path names
// them, as against the step and the port of a run, which say which part of it.
func identities(pattern string) []string {
	var out []string
	for _, m := range parameter.FindAllStringSubmatch(pattern, -1) {
		switch m[1] {
		case "step", "port", "commit":
		default:
			out = append(out, m[1])
		}
	}
	return out
}

func fill(pattern string, named map[string]string) string {
	return parameter.ReplaceAllStringFunc(pattern, func(p string) string {
		m := parameter.FindStringSubmatch(p)
		return named[m[1]]
	})
}

// sameAnswer says how an answer differs from the one absence got, and is empty where it does not:
// status, headers and body.
func sameAnswer(absence, w *httptest.ResponseRecorder) string {
	switch {
	case absence.Code != w.Code:
		return fmt.Sprintf("%d, and absence %d", w.Code, absence.Code)
	case !maps.EqualFunc(absence.Header(), w.Header(), slices.Equal):
		return fmt.Sprintf("the headers %v, and absence %v", w.Header(), absence.Header())
	case !bytes.Equal(absence.Body.Bytes(), w.Body.Bytes()):
		return fmt.Sprintf("%q, and absence %q", w.Body, absence.Body)
	}
	return ""
}

// "No matching grant means refusal: the same 404 whether the workflow is absent or merely invisible,
// so listing cannot discover names", and the same for a namespace, a run, a secret, a grant and a
// token. Every route serve builds that names something in its path is asked about it as a principal
// holding nothing there, once naming what does not exist and once naming what does, and has to answer
// the two alike, byte for byte and header for header: status, error sentence, Content-Type and
// Cache-Control. A route naming several things is asked with each in turn named as it is and the rest
// as nothing, so that an absent workflow in a namespace that exists answers as one in a namespace
// that does not. mallory holds nothing in finance, and asks with her token, with one narrowed to hr
// and from her browser, and asks as well about finance's runs, grants, workflows and service account
// under hr, which she owns; oscar holds a role on one of finance's workflows alone, which makes
// finance a namespace he sees and every other workflow of it one he does not.
func TestAnAbsentNameAndAnInvisibleOneAreAnsweredAlike(t *testing.T) {
	x := someTenants(t)
	asked := 0
	for _, route := range x.in.router.Routes() {
		name := route.Method + " " + route.Pattern
		ids := identities(route.Pattern)
		if len(ids) == 0 || staticAssets[name] {
			continue
		}
		for _, c := range []struct {
			// within fixes the namespace a path names to one the caller holds something in, where
			// it is not empty.
			who, within string
		}{{"mallory", ""}, {"mallory's browser", ""}, {"mallory's token for hr", ""}, {"oscar", "finance"}, {"mallory", "hr"}} {
			named := x.present(route.Pattern)
			nothing := absent()
			varying := ids
			if c.within != "" {
				named["namespace"], named["ns"] = c.within, c.within
				nothing["namespace"], nothing["ns"] = c.within, c.within
				varying = slices.DeleteFunc(slices.Clone(ids), func(p string) bool { return p == "namespace" || p == "ns" })
				if len(varying) == 0 {
					continue
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			none := x.ask(ctx, route.Method, fill(route.Pattern, nothing), x.as[c.who], "{}")
			// Each identity named as it is in turn, those before it too, and the rest as nothing.
			for i := range varying {
				mixed := maps.Clone(nothing)
				for _, p := range varying[:i+1] {
					mixed[p] = named[p]
				}
				path := fill(route.Pattern, mixed)
				w := x.ask(ctx, route.Method, path, x.as[c.who], "{}")
				asked++
				if diff := sameAnswer(none, w); diff != "" {
					t.Errorf("%s: as %s, %s answered %s", name, c.who, path, diff)
				}
			}
			cancel()
		}
	}
	if asked < 40 {
		t.Fatalf("%d questions were asked of the routes, and serve builds more routes naming something than that", asked)
	}

	// And what a request names in its body or its query rather than its path, asked by mallory where
	// she may ask it, in hr, which she owns: a service account made, a token minted and a grant
	// written for somebody of a namespace she does not see, and a listing narrowed to one.
	for _, c := range []struct {
		method, absent, present string
		nothing, something      any
	}{
		{"POST", "/api/v1/service-accounts", "/api/v1/service-accounts",
			api.NewServiceAccount{Namespace: "nowhere", Name: "deploy"}, api.NewServiceAccount{Namespace: "finance", Name: "deploy"}},
		{"POST", "/api/v1/service-accounts", "/api/v1/service-accounts",
			api.NewServiceAccount{Namespace: "nowhere", Name: "nightly"}, api.NewServiceAccount{Namespace: "finance", Name: "nightly"}},
		{"POST", "/api/v1/auth/tokens", "/api/v1/auth/tokens",
			api.TokenRequest{Principal: "nowhere/nightly"}, api.TokenRequest{Principal: "finance/nightly"}},
		{"POST", "/api/v1/hr/grants", "/api/v1/hr/grants",
			api.GrantRequest{Principal: "nowhere/nightly", Role: "viewer"}, api.GrantRequest{Principal: "finance/nightly", Role: "viewer"}},
		{"POST", "/api/v1/hr/grants", "/api/v1/hr/grants",
			api.GrantRequest{Principal: "nowhere/agentiik", Role: "viewer"}, api.GrantRequest{Principal: "finance/agentiik", Role: "viewer"}},
		{"GET", "/api/v1/runs?namespace=nowhere", "/api/v1/runs?namespace=finance", nil, nil},
		{"GET", "/api/v1/runs?namespace=finance&workflow=nothing", "/api/v1/runs?namespace=finance&workflow=monthly-invoicing", nil, nil},
		{"GET", "/api/v1/finance/runs?workflow=nothing", "/api/v1/finance/runs?workflow=monthly-invoicing", nil, nil},
	} {
		none := x.ask(t.Context(), c.method, c.absent, x.as["mallory"], c.nothing)
		w := x.ask(t.Context(), c.method, c.present, x.as["mallory"], c.something)
		// A refusal says back what it was asked about, which is the caller's own words.
		echoed := httptest.NewRecorder()
		maps.Copy(echoed.Header(), w.Header())
		echoed.Code = w.Code
		echoed.Body.WriteString(strings.ReplaceAll(w.Body.String(), "finance", "nowhere"))
		if diff := sameAnswer(none, echoed); diff != "" {
			t.Errorf("%s %s naming %v answered %s", c.method, c.present, c.something, diff)
		}
	}
}

// secretSpellings are the ways a value could be written in an answer: as it is, and in the encodings
// an answer carries bytes in, base64 of either alphabet and hexadecimal.
func secretSpellings(value string) []string {
	return []string{
		value,
		base64.StdEncoding.EncodeToString([]byte(value)),
		base64.RawURLEncoding.EncodeToString([]byte(value)),
		hex.EncodeToString([]byte(value)),
	}
}

// "Secret values, at any role": "No role reads a secret value through the API." Every route serve
// builds is asked by finance's owner about everything finance holds, its two secrets included, with
// an empty body and with the one the route takes where it takes one, and by an administrator of the
// installation where it reads; no answer, body or header, carries either value in any spelling. The
// routes that read are asked first and the ones that change something after, those removing
// something last, so that each is asked while what it names is there, and a secret's own routes are
// asked last of all, about ledger, so that billing is there for every other.
func TestNoAnswerOfAnyRouteCarriesASecretsValue(t *testing.T) {
	x := someTenants(t)
	routes := x.in.router.Routes()
	order := func(r api.Route) int {
		n := 1
		switch r.Method {
		case "GET", "HEAD":
			n = 0
		case "DELETE":
			n = 2
		}
		if strings.Contains(r.Pattern, "/secrets/{name}") && r.Method != "GET" {
			n += 3
		}
		return n
	}
	slices.SortStableFunc(routes, func(a, b api.Route) int { return order(a) - order(b) })

	// What each route that takes a body is sent besides an empty one, as its owner would send it.
	pushed := aPush(t)
	taken := map[string]any{
		"PUT /api/v1/{namespace}/workflows/{workflow}/versions/{commit}": pushed,
		"POST /api/v1/{namespace}/workflows/{workflow}/runs":             api.Start{Commit: theCommit, Inputs: map[string]any{"orders": []any{}}},
		"PUT /api/v1/{namespace}/secrets/{name}":                         api.Declare{Provider: "env", Path: ledgerVariable},
		"POST /api/v1/{namespace}/grants":                                api.GrantRequest{Principal: "group:auditors", Role: "viewer"},
		"POST /api/v1/{namespace}/workflows/{workflow}/grants":           api.GrantRequest{Principal: "group:auditors", Role: "viewer"},
		"POST /api/v1/service-accounts":                                  api.NewServiceAccount{Namespace: "finance", Name: "deploy"},
		"POST /api/v1/auth/tokens":                                       api.TokenRequest{Principal: "finance/nightly"},
		"PUT /api/v1/{namespace}/auth/policy":                            map[string]any{},
	}
	asked, succeeded := 0, 0
	for _, who := range []string{"alice", "carol"} {
		for _, route := range routes {
			if who == "carol" && order(route) != 0 {
				// An administrator changing things would change what alice is asked about.
				continue
			}
			name := route.Method + " " + route.Pattern
			named := x.present(route.Pattern)
			if strings.Contains(route.Pattern, "/secrets/{name}") && route.Method != "GET" {
				named["name"] = "ledger"
			}
			path := fill(route.Pattern, named)
			bodies := []any{"{}"}
			if body, ok := taken[name]; ok {
				bodies = append(bodies, body)
			}
			for _, body := range bodies {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				w := x.ask(ctx, route.Method, path, x.as[who], body)
				cancel()
				asked++
				if w.Code < 300 {
					succeeded++
				}
				answer := w.Body.String() + fmt.Sprint(w.Header())
				for _, value := range x.values {
					for _, spelled := range secretSpellings(value) {
						if strings.Contains(answer, spelled) {
							t.Errorf("%s %s, asked by %s, answered %d carrying a secret's value: %s", route.Method, path, who, w.Code, answer)
						}
					}
				}
			}
		}
	}
	if asked < 100 || succeeded < 40 {
		t.Fatalf("%d questions were asked of the routes and %d answered as done, and serve builds more than that", asked, succeeded)
	}
}

// timingVariable asks TestHowLongAbsenceAndInvisibilityTake to run: it measures, and a machine
// under other load measures noise, so it runs when somebody asks for numbers.
const timingVariable = "AGENTIIK_TEST_TIMING"

// How long a principal holding nothing in finance waits for the answer about something that does
// not exist and about something finance holds, on the routes a prober would ask first, interleaved
// so that the two meet the same machine: the medians and the ninetieth percentiles are logged.
// finance holds twenty workflows here, since a listing asking about each of them is where a namespace
// that exists would take longer than one that does not.
func TestHowLongAbsenceAndInvisibilityTake(t *testing.T) {
	if os.Getenv(timingVariable) == "" {
		t.Skipf("set %s to measure", timingVariable)
	}
	x := someTenants(t)
	for i := range 18 {
		x.must("PUT", fmt.Sprintf("/api/v1/finance/workflows/extra-%02d/versions/%s", i, theCommit), x.as["alice"], aPush(t), http.StatusOK)
	}
	for _, c := range []struct{ who, method, absent, present string }{
		{"mallory", "GET", "/api/v1/runs/" + ulid.New(), "/api/v1/runs/" + x.run},
		{"mallory's token for hr", "GET", "/api/v1/runs/" + ulid.New(), "/api/v1/runs/" + x.run},
		{"oscar", "GET", "/api/v1/runs/" + ulid.New(), "/api/v1/runs/" + x.run},
		{"oscar", "GET", "/api/v1/finance/runs?workflow=nothing", "/api/v1/finance/runs?workflow=monthly-invoicing"},
		{"mallory", "GET", "/api/v1/nowhere/runs", "/api/v1/finance/runs"},
		{"mallory", "GET", "/api/v1/runs?namespace=nowhere", "/api/v1/runs?namespace=finance"},
		{"mallory", "GET", "/api/v1/nowhere/secrets", "/api/v1/finance/secrets"},
		{"mallory", "GET", "/api/v1/namespaces/nowhere", "/api/v1/namespaces/finance"},
		{"mallory", "POST", "/api/v1/nowhere/workflows/nothing/runs", "/api/v1/finance/workflows/monthly-invoicing/runs"},
		{"mallory", "DELETE", "/api/v1/auth/tokens/" + ulid.New(), "/api/v1/auth/tokens/" + x.token},
	} {
		const rounds = 300
		took := [2][]time.Duration{}
		for i := range rounds + 20 {
			for j, path := range []string{c.absent, c.present} {
				began := time.Now()
				x.ask(t.Context(), c.method, path, x.as[c.who], "{}")
				if i >= 20 {
					took[j] = append(took[j], time.Since(began))
				}
			}
		}
		for j := range took {
			slices.Sort(took[j])
		}
		t.Logf("%-22s %-6s %-48s absent %6.0fµs p90 %6.0fµs | present %6.0fµs p90 %6.0fµs", c.who, c.method, c.present,
			micro(took[0][rounds/2]), micro(took[0][rounds*9/10]), micro(took[1][rounds/2]), micro(took[1][rounds*9/10]))
	}
}

func micro(d time.Duration) float64 { return float64(d) / float64(time.Microsecond) }

// "Runner internals. A user never learns which host executed a task beyond its runner name and
// labels." A task of alice's run ran on finance's runner, which joined with its version and capacity
// and reported its state since, and the run as its owner reads it, by either route, in the listing and
// in its step's log stream, names the runner by its identifier and says nothing else of it: no
// version, no architecture, no capacity, no key, no address, and each task carries the fields the
// page's run view names and no other.
func TestARunViewNamesItsRunnerAndNothingOfItsHost(t *testing.T) {
	x := someTenants(t)
	beat := api.Beat{Runner: x.runner, AgentVersion: runnerVersion, State: "ready", Concurrency: 3, Tasks: []agk.TaskID{}, SentAt: time.Now()}
	x.must("POST", "/api/v1/runners/heartbeat", asker{bearer: x.runnerCredential}, beat, http.StatusOK)
	conn, err := pgx.Connect(t.Context(), x.admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `insert into tasks (namespace, id, run_id, step, attempt, state, runner, exit_code, started_at, finished_at)
		values ('finance', $1, $2, 'normalize', 1, 'succeeded', $3, 0, now(), now())`, ulid.New(), x.run, x.runner); err != nil {
		t.Fatal(err)
	}

	// What the inventory says of a runner besides its name and labels, by the names it says it under
	// and by the values this one joined and reported with.
	host := []string{runnerVersion, "amd64", "13958643712", "82678120448", "MCowBQYDK2VwAyEAXiy2zvWwTpj67NwwKIgCbjFcQdrNAboeffNXm",
		`"agent_version"`, `"architecture"`, `"cpu"`, `"memory_bytes"`, `"disk_bytes"`, `"public_key"`, `"containment"`,
		`"reported_state"`, `"concurrency"`, `"joined_at"`, `"last_seen_at"`, `"rotate_by"`, `"pool"`, `"address"`, `"host"`}
	fields := []string{"task", "step", "state", "attempt", "shard", "runner", "exit_code", "started_at", "finished_at", "inputs"}
	for _, path := range []string{"/api/v1/runs/" + x.run, "/api/v1/finance/runs/" + x.run} {
		w := x.must("GET", path, x.as["alice"], nil, http.StatusOK)
		var detail struct {
			Tasks []map[string]any `json:"tasks"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if len(detail.Tasks) != 1 || detail.Tasks[0]["runner"] != x.runner {
			t.Fatalf("%s names the tasks %v, and its one task ran on %s", path, detail.Tasks, x.runner)
		}
		for name := range detail.Tasks[0] {
			if !slices.Contains(fields, name) {
				t.Errorf("%s answers a task's %s, which the run view does not name", path, name)
			}
		}
		for _, detail := range host {
			if strings.Contains(w.Body.String(), detail) {
				t.Errorf("%s says %q of the runner: %s", path, detail, w.Body)
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, path := range []string{"/api/v1/runs", "/api/v1/finance/runs", "/api/v1/runs/" + x.run + "/steps/normalize/logs"} {
		w := x.ask(ctx, "GET", path, x.as["alice"], nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", path, w.Code, w.Body)
		}
		for _, detail := range append(host, x.runner) {
			if strings.Contains(w.Body.String(), detail) {
				t.Errorf("%s says %q of the runner: %s", path, detail, w.Body)
			}
		}
	}
}

// pushNaming is aPush of a workflow document of the test's own.
func pushNaming(t *testing.T, document string) api.Push {
	t.Helper()
	p := aPush(t)
	p.Document = []byte(document)
	p.Tree = map[string]api.PushFile{"agentiik.yaml": {Content: []byte(document), Mode: "0644"}}
	return p
}

// The secret routes, as the grants of real principals decide them: "GET /api/v1/{ns}/secrets:
// requires workflow:read at namespace scope", "PUT, DELETE: requires secret:write at namespace scope",
// a version naming a secret pushed only by someone holding secret:use in the namespace, and "a grant on
// one workflow shows none", although the editor role it gives holds all three. victor views finance and
// reads its declarations, and writes none; walter edits monthly-invoicing and neither reads them nor
// writes one, nor pushes a version naming one, since secret:use and secret:write come from namespace
// grants alone; alice owns finance and does all of it. Every refusal is the 404 of a namespace with no
// such declaration, and a push's the 403 the page names.
func TestTheSecretRoutesTakeWhatTheirGrantsGiveInTheNamespace(t *testing.T) {
	x := someTenants(t)
	naming := strings.Replace(strings.Replace(theWorkflow, "steps:\n", "secrets: [billing]\nsteps:\n", 1),
		"    outputs: [ok, rejected]\n", "    outputs: [ok, rejected]\n    secrets: [billing]\n", 1)
	if naming == theWorkflow {
		t.Fatal("the workflow names no secret")
	}
	value := "sk_rotated_" + randomHex(t)
	for _, c := range []struct {
		who                                  string
		list, read, write, remove, pushNamed int
	}{
		{"walter", http.StatusNotFound, http.StatusNotFound, http.StatusNotFound, http.StatusNotFound, http.StatusForbidden},
		{"victor", http.StatusOK, http.StatusOK, http.StatusNotFound, http.StatusNotFound, http.StatusNotFound},
		{"alice", http.StatusOK, http.StatusOK, http.StatusOK, http.StatusNoContent, http.StatusOK},
	} {
		as := x.as[c.who]
		for _, q := range []struct {
			method, path string
			body         any
			want         int
		}{
			{"GET", "/api/v1/finance/secrets", nil, c.list},
			{"GET", "/api/v1/finance/secrets/billing", nil, c.read},
			{"PUT", "/api/v1/finance/secrets/billing", api.Declare{Provider: "builtin", Value: &value}, c.write},
			{"PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/" + strings.Repeat("b", 40), pushNaming(t, naming), c.pushNamed},
			{"DELETE", "/api/v1/finance/secrets/ledger", nil, c.remove},
		} {
			w := x.ask(t.Context(), q.method, q.path, as, q.body)
			if w.Code != q.want {
				t.Errorf("%s: %s %s answered %d, want %d: %s", c.who, q.method, q.path, w.Code, q.want, w.Body)
			}
			if w.Code == http.StatusNotFound && w.Body.String() != "{\"error\":\"no such thing, or not yours\"}\n" {
				t.Errorf("%s: %s %s was refused with %s, which is not what absence is answered", c.who, q.method, q.path, w.Body)
			}
		}
	}
}
