package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/totp"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
	"github.com/agentiik/agentiik/version"
)

// Every act an installation serves is recorded in the audit log exactly once, with the action the
// page names, who did it, what it was done to, and the namespace it was done in or none: driven from
// the route table serve builds, so that a route changing something that no case here records fails
// this test until one does, and a route that changes nothing the log records says why here.

// publicOrigin is the origin of the public URL servingSettings names, which the sign-in page's
// requests and every request a session carries come from.
const publicOrigin = "https://agentiik.example.com"

// reads are the routes that change nothing: what they answer is read, and reading is no act.
var reads = []string{
	"GET /api/v1/runner-pools", "GET /api/v1/runners", "GET /api/v1/users", "GET /api/v1/users/{login}",
	"GET /api/v1/groups", "GET /api/v1/groups/{group}", "GET /api/v1/auth/tokens", "GET /api/v1/service-accounts",
	"GET /api/v1/runs", "GET /api/v1/{namespace}/runs", "GET /api/v1/runs/{run}", "GET /api/v1/{namespace}/runs/{run}",
	"GET /api/v1/runs/{run}/steps/{step}/logs", "GET /api/v1/runs/{run}/outputs/{name}",
	"GET /api/v1/runs/{run}/steps/{step}/outputs/{port}", "GET /api/v1/runs/{run}/steps/{step}/inputs/{port}",
	"GET /api/v1/artifacts/{uri}", "GET /api/v1/{namespace}/secrets", "GET /api/v1/{namespace}/secrets/{name}",
	"GET /api/v1/namespaces", "GET /api/v1/namespaces/{namespace}", "GET /api/v1/namespaces/{namespace}/quotas",
	"GET /api/v1/{namespace}/grants", "GET /api/v1/{namespace}/workflows/{workflow}/grants",
	"GET /api/v1/{namespace}/workflows/{workflow}/images",
	"GET /api/v1/me", "GET /api/v1/me/credentials", "GET /api/v1/auth/policy", "GET /api/v1/{namespace}/auth/policy",
	"GET /auth/sign-in", "GET /auth/enrol", "GET /auth/assets/{name}", "GET /objects/{key...}",
}

// recordsNothing are the routes that change something the audit log does not record, each with why.
var recordsNothing = map[string]string{
	"POST /api/v1/me/totp":                 "a TOTP generator started counts for nothing until POST /api/v1/me/totp/confirm enrols it, which is recorded",
	"POST /api/v1/auth/sign-out":           "a session is no credential: a sign-out ends one and gives nobody anything",
	"DELETE /api/v1/me/notifications/{id}": "a notification is its reader's copy of an act the log recorded, and dismissing it changes no access",
	"POST /api/v1/runners/heartbeat":       "a runner's own traffic under its credential, which no principal does",
	"POST /api/v1/runners/rotate":          "a runner's own traffic under its credential, which no principal does",
	"POST /api/v1/tasks/redeem":            "a runner's own traffic under its credential, which no principal does",
	"POST /api/v1/tasks/logs":              "a runner's own traffic under its credential, which no principal does",
	"POST /api/v1/bus/token":               "a runner's own traffic under its credential, which no principal does",
	"PUT /objects/{key...}":                "a task's output, stored under a URL its redemption signed, and the page's audit log names no upload",
	"POST /objects/{namespace}":            "a task's output, stored under a policy its redemption signed, and the page's audit log names no upload",
}

// actor is how a step asks: bearing a token, carrying a session from the public URL's origin, or
// neither, and from an address of its own where from is set.
type actor struct {
	bearer string
	cookie *http.Cookie
	from   string
}

// scenario is an installation serve built, asked one act at a time, each step holding the entries
// it appended to what it says they are.
type scenario struct {
	t   *testing.T
	in  *installation
	seq int64

	// record holds the routes a step held to the entries it appended.
	record map[string]bool
}

// act asks route and holds the entries it appended to want, as ask and holds say.
func (s *scenario) act(route, path string, who actor, body any, status int, want ...string) *httptest.ResponseRecorder {
	s.t.Helper()
	w := s.ask(route, path, who, body, status)
	s.holds(route, want...)
	return w
}

// ask asks route, which is its method and pattern, at path as who with body, a string sent as it is
// and anything else as its JSON, and holds the answer to status and the route that answered it to
// route, so that no case is counted for a route it did not reach.
func (s *scenario) ask(route, path string, who actor, body any, status int) *httptest.ResponseRecorder {
	s.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			s.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	method, _, _ := strings.Cut(route, " ")
	r := httptest.NewRequestWithContext(s.t.Context(), method, path, reader)
	if reader != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if who.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+who.bearer)
	}
	if who.cookie != nil {
		r.AddCookie(who.cookie)
	}
	if who.bearer == "" {
		r.Header.Set("Origin", publicOrigin)
	}
	if who.from != "" {
		r.RemoteAddr = who.from + ":4000"
	}
	w := httptest.NewRecorder()
	s.in.router.ServeHTTP(w, r)
	if w.Code != status {
		s.t.Fatalf("%s %s answered %d, want %d: %s", method, path, w.Code, status, w.Body)
	}
	if r.Pattern != route {
		s.t.Fatalf("%s %s was answered by %q, and the case is for %s", method, path, r.Pattern, route)
	}
	return w
}

// holds holds the entries appended since the last step to want, each written action actor target
// namespace result, with - for no namespace and * for a target the answer does not name, and counts
// a case for route where it appended any.
func (s *scenario) holds(route string, want ...string) {
	s.t.Helper()
	if len(want) > 0 {
		s.record[route] = true
	}
	s.recorded(route, want...)
}

// recorded holds the entries appended since the last step to want, as act says, and verifies the
// chain they extend.
func (s *scenario) recorded(what string, want ...string) {
	s.t.Helper()
	entries, err := s.in.pool.AuditTrail().After(s.t.Context(), 0, 10000)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := audit.Verify(entries); err != nil {
		s.t.Fatalf("after %s the audit log does not verify: %s", what, err)
	}
	var got []string
	for _, e := range entries {
		if e.Seq <= s.seq {
			continue
		}
		namespace := e.Namespace
		if namespace == "" {
			namespace = "-"
		}
		got = append(got, strings.Join([]string{e.Action, e.Actor, e.Target, namespace, e.Result}, " "))
		s.seq = e.Seq
	}
	matches := len(got) == len(want)
	for i := 0; matches && i < len(got); i++ {
		g, w := strings.Fields(got[i]), strings.Fields(want[i])
		for j := range w {
			if w[j] != "*" && w[j] != g[j] {
				matches = false
			}
		}
	}
	if !matches {
		s.t.Errorf("%s recorded\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// answer reads an answer's body as an object.
func (s *scenario) answer(w *httptest.ResponseRecorder) map[string]any {
	s.t.Helper()
	var a map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		s.t.Fatalf("the answer %s is not an object: %s", w.Body, err)
	}
	return a
}

// session is the session an answer opened.
func (s *scenario) session(w *httptest.ResponseRecorder) *http.Cookie {
	s.t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == api.SessionCookie {
			return c
		}
	}
	s.t.Fatalf("the answer %d %s opened no session", w.Code, w.Body)
	return nil
}

// options are the options a ceremony's first half answered.
func (s *scenario) options(w *httptest.ResponseRecorder) []byte {
	s.t.Helper()
	var o struct {
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		s.t.Fatal(err)
	}
	return o.Options
}

// codeIn is the code an enrolment link carries after its #.
var codeIn = regexp.MustCompile(`#(agkenrol_[A-Za-z0-9_-]+)$`)

func (s *scenario) code(link any) string {
	s.t.Helper()
	m := codeIn.FindStringSubmatch(fmt.Sprint(link))
	if m == nil {
		s.t.Fatalf("%v is not an enrolment link", link)
	}
	return m[1]
}

// Every route serve builds that changes something records its act once, as the page's audit log row
// names it, with who did it, what to and where; the host's own verbs too. The acts, in the order an
// installation lives them: the first administrator made with the bootstrap token and enrolled,
// which ends it; users, their links and recovery codes, and every way of signing in, agk login's
// exchange among them, refused as well as let in; credentials enrolled and removed; API tokens; the
// policy at both scopes, one forbidding passwords taking one; namespaces, their quotas and service
// accounts; groups and their members; grants and denies at both scopes; secrets, runs, pools, join
// tokens and runners' orders; and a user removed with their personal namespace. A run the
// controller cancels at its admission is the controller's act, which its own tests hold to its one
// entry.
func TestEveryRouteThatChangesSomethingRecordsItsActOnce(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	bootstrapped(t, database.Application)
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	in, err := open(t.Context(), servingSettings(t, database.Application, dir, natsFrom(t, dir)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	s := &scenario{t: t, in: in, record: map[string]bool{}}
	s.recorded("migrating")

	// On the host: a namespace created, as init creates the one its settings name.
	if err := namespace(t.Context(), database.Application, "create", "finance", io.Discard); err != nil {
		t.Fatal(err)
	}
	s.recorded("agentiik-api namespace create", "namespace.create installation finance - done")

	// The first administrator, created with the bootstrap token, handed finance, which no record
	// names an owner of, and given a fresh link, enrols a passkey from it, which ends the token; the
	// link it replaced opens nothing.
	boot := actor{bearer: theToken}
	w := s.act("POST /api/v1/users", "/api/v1/users", boot, `{"login":"carol","admin":true}`, http.StatusCreated,
		"user.create operator carol - done", "enrolment.issue operator carol - done", "grant.create operator * finance done")
	stale := s.code(s.answer(w)["enrolment"].(map[string]any)["link"])
	w = s.act("POST /api/v1/users/{login}/enrolment", "/api/v1/users/carol/enrolment", boot, nil, http.StatusCreated,
		"enrolment.issue operator carol - done")
	link := s.code(s.answer(w)["link"])
	s.act("POST /api/v1/auth/passkey/options", "/api/v1/auth/passkey/options", actor{},
		fmt.Sprintf(`{"ceremony":"registration","code":%q}`, stale), http.StatusUnauthorized,
		"signin.fail 192.0.2.1 carol - done")
	carolsKey := webauthntest.New(publicOrigin)
	w = s.act("POST /api/v1/auth/passkey/options", "/api/v1/auth/passkey/options", actor{},
		fmt.Sprintf(`{"ceremony":"registration","code":%q}`, link), http.StatusOK)
	made, _, err := carolsKey.Create(s.options(w))
	if err != nil {
		t.Fatal(err)
	}
	w = s.act("POST /api/v1/auth/passkey/verify", "/api/v1/auth/passkey/verify", actor{},
		map[string]any{"ceremony": "registration", "credential": made}, http.StatusOK,
		"credential.enrol carol "+made.ID+" - done", "enrolment.use carol carol - done", "bootstrap.end carol operator - done",
		"grant.create installation * carol done", "namespace.create installation carol - done", "signin.succeed carol carol - done")
	carol := actor{cookie: s.session(w)}
	s.act("POST /api/v1/users", "/api/v1/users", boot, `{"login":"mallory","admin":true}`, http.StatusUnauthorized)

	// On the host: the break-glass path, for the day no administrator can sign in.
	var printed bytes.Buffer
	if err := recoverAdministrator(t.Context(), config.Recovery{Database: database.Application, PublicURL: publicOrigin}, "carol", time.Now(), &printed); err != nil {
		t.Fatal(err)
	}
	s.recorded("agentiik-api recover", "enrolment.issue installation carol - done")

	// dave, created by carol, is given a recovery code and sets a password with it, which opens a
	// session that may only enrol, since a passkey is required; the code spent opens nothing again.
	w = s.act("POST /api/v1/users", "/api/v1/users", carol, `{"login":"dave"}`, http.StatusCreated,
		"user.create carol dave - done", "enrolment.issue carol dave - done")
	w = s.act("POST /api/v1/users/{login}/recovery", "/api/v1/users/dave/recovery", carol, nil, http.StatusCreated,
		"enrolment.issue carol dave - done")
	recovery := s.answer(w)["code"].(string)
	const davesPassword, davesNext = "correct horse battery staple", "a second passphrase for dave"
	w = s.ask("POST /api/v1/auth/password/enrol", "/api/v1/auth/password/enrol", actor{},
		map[string]string{"code": recovery, "password": davesPassword}, http.StatusOK)
	enrolling := actor{cookie: s.session(w)}
	password := s.answer(w)["credential"].(map[string]any)["id"].(string)
	s.holds("POST /api/v1/auth/password/enrol", "credential.enrol dave "+password+" - done", "enrolment.use dave dave - done",
		"grant.create installation * dave done", "namespace.create installation dave - done", "signin.succeed dave dave - done")
	s.act("POST /api/v1/auth/password/enrol", "/api/v1/auth/password/enrol", actor{},
		map[string]string{"code": recovery, "password": davesPassword}, http.StatusUnauthorized,
		"signin.fail 192.0.2.1 dave - done")

	// From that session dave registers a passkey, and signs in with it; the same assertion sent
	// again, its challenge answered already, signs nobody in.
	davesKeys := webauthntest.New(publicOrigin)
	w = s.act("POST /api/v1/auth/passkey/options", "/api/v1/auth/passkey/options", enrolling, `{"ceremony":"registration"}`, http.StatusOK)
	first, _, err := davesKeys.Create(s.options(w))
	if err != nil {
		t.Fatal(err)
	}
	s.act("POST /api/v1/auth/passkey/verify", "/api/v1/auth/passkey/verify", enrolling,
		map[string]any{"ceremony": "registration", "credential": first}, http.StatusOK,
		"credential.enrol dave "+first.ID+" - done")
	w = s.act("POST /api/v1/auth/passkey/options", "/api/v1/auth/passkey/options", actor{}, `{"ceremony":"assertion"}`, http.StatusOK)
	asserted, err := davesKeys.Get(s.options(w))
	if err != nil {
		t.Fatal(err)
	}
	w = s.act("POST /api/v1/auth/passkey/verify", "/api/v1/auth/passkey/verify", actor{},
		map[string]any{"ceremony": "assertion", "credential": asserted}, http.StatusOK,
		"signin.succeed dave dave - done")
	dave := actor{cookie: s.session(w)}
	s.act("POST /api/v1/auth/passkey/verify", "/api/v1/auth/passkey/verify", actor{from: "198.51.100.7"},
		map[string]any{"ceremony": "assertion", "credential": asserted}, http.StatusUnauthorized,
		"signin.fail 198.51.100.7 "+asserted.ID+" - done")

	// agk login: an assertion carrying its loopback address and the SHA-256 of its verifier hands
	// it a code, which it trades once for an API token of dave's; the code presented again opens
	// nothing, is answered as a bearer token that opens nothing is, and is recorded as a sign-in
	// refused, about no account, since the code taken names none any more.
	verifier := strings.Repeat("v", 43)
	challenge := sha256.Sum256([]byte(verifier))
	w = s.act("POST /api/v1/auth/passkey/options", "/api/v1/auth/passkey/options", actor{}, `{"ceremony":"assertion"}`, http.StatusOK)
	terminal, err := davesKeys.Get(s.options(w))
	if err != nil {
		t.Fatal(err)
	}
	w = s.act("POST /api/v1/auth/passkey/verify", "/api/v1/auth/passkey/verify", actor{},
		map[string]any{"ceremony": "assertion", "credential": terminal, "terminal": map[string]string{
			"redirect_uri": "http://127.0.0.1:4711/", "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]),
		}}, http.StatusOK, "signin.succeed dave dave - done")
	handed, err := url.Parse(s.answer(w)["redirect_to"].(string))
	if err != nil {
		t.Fatal(err)
	}
	exchange := map[string]string{"code": handed.Query().Get("code"), "code_verifier": verifier}
	w = s.ask("POST /api/v1/auth/exchange", "/api/v1/auth/exchange", actor{}, exchange, http.StatusCreated)
	s.holds("POST /api/v1/auth/exchange", "api_token.create dave "+s.answer(w)["api_token"].(map[string]any)["id"].(string)+" - done")
	s.act("POST /api/v1/auth/exchange", "/api/v1/auth/exchange", actor{}, exchange, http.StatusUnauthorized,
		"signin.fail 192.0.2.1 an unknown exchange code - done")

	// dave's password signs in, and a wrong one does not.
	s.act("POST /api/v1/auth/login", "/api/v1/auth/login", actor{},
		map[string]string{"login": "dave", "password": davesPassword}, http.StatusOK,
		"signin.succeed dave dave - done")
	s.act("POST /api/v1/auth/login", "/api/v1/auth/login", actor{from: "198.51.100.8"},
		map[string]string{"login": "dave", "password": "not dave's password"}, http.StatusUnauthorized,
		"signin.fail 198.51.100.8 dave - done")

	// dave changes his password, enrols a TOTP generator beside it and removes it.
	s.act("PUT /api/v1/me/password", "/api/v1/me/password", dave,
		map[string]string{"current_password": davesPassword, "password": davesNext}, http.StatusOK,
		"credential.enrol dave "+password+" - done")
	w = s.act("POST /api/v1/me/totp", "/api/v1/me/totp", dave, nil, http.StatusOK)
	started := s.answer(w)
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(started["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	step := totp.StepAt(time.Now())
	s.act("POST /api/v1/me/totp/confirm", "/api/v1/me/totp/confirm", dave,
		map[string]string{"totp": totp.Code(secret, step)}, http.StatusOK,
		"credential.enrol dave "+started["id"].(string)+" - done")
	s.act("DELETE /api/v1/me/totp", "/api/v1/me/totp", dave,
		map[string]string{"totp": totp.Code(secret, step+1)}, http.StatusNoContent,
		"credential.remove dave "+started["id"].(string)+" - done")

	// A second passkey, on another device, brings dave to min_passkeys where a passkey is
	// required, which takes his password. With passkeys optional and min_passkeys at one, he
	// removes that passkey, sets a password again and removes it.
	w = s.act("POST /api/v1/auth/passkey/options", "/api/v1/auth/passkey/options", dave, `{"ceremony":"registration"}`, http.StatusOK)
	second, _, err := webauthntest.New(publicOrigin).Create(s.options(w))
	if err != nil {
		t.Fatal(err)
	}
	s.act("POST /api/v1/auth/passkey/verify", "/api/v1/auth/passkey/verify", dave,
		map[string]any{"ceremony": "registration", "credential": second}, http.StatusOK,
		"credential.enrol dave "+second.ID+" - done", "credential.remove dave "+password+" - done")
	s.act("PUT /api/v1/auth/policy", "/api/v1/auth/policy", carol, `{"passkey":"optional","min_passkeys":1}`, http.StatusOK,
		"policy.change carol installation - done")
	s.act("DELETE /api/v1/me/credentials/{id}", "/api/v1/me/credentials/"+second.ID, dave, nil, http.StatusNoContent,
		"credential.remove dave "+second.ID+" - done")
	w = s.ask("PUT /api/v1/me/password", "/api/v1/me/password", dave, map[string]string{"password": davesPassword}, http.StatusOK)
	again := s.answer(w)["id"].(string)
	s.holds("PUT /api/v1/me/password", "credential.enrol dave "+again+" - done")
	s.act("DELETE /api/v1/me/password", "/api/v1/me/password", dave, nil, http.StatusNoContent,
		"credential.remove dave "+again+" - done")

	// carol's own API token, minted and revoked.
	w = s.ask("POST /api/v1/auth/tokens", "/api/v1/auth/tokens", carol, `{"device_label":"laptop"}`, http.StatusCreated)
	minted := s.answer(w)["api_token"].(map[string]any)["id"].(string)
	s.holds("POST /api/v1/auth/tokens", "api_token.create carol "+minted+" - done")
	s.act("DELETE /api/v1/auth/tokens/{id}", "/api/v1/auth/tokens/"+minted, carol, nil, http.StatusNoContent,
		"api_token.revoke carol "+minted+" - done")

	// A namespace carol owns, its quotas, and a service account of it with a token, removed with it.
	s.act("POST /api/v1/namespaces", "/api/v1/namespaces", carol, `{"name":"ops","owner":"carol"}`, http.StatusCreated,
		"grant.create carol * ops done", "namespace.create carol ops - done")
	s.act("PUT /api/v1/namespaces/{namespace}/quotas", "/api/v1/namespaces/ops/quotas", carol, `{"max_concurrent_tasks":5}`, http.StatusOK,
		"namespace.update carol ops - done")
	s.act("POST /api/v1/service-accounts", "/api/v1/service-accounts", carol, `{"namespace":"ops","name":"deployer"}`, http.StatusCreated,
		"service_account.create carol ops/deployer ops done")
	w = s.ask("POST /api/v1/auth/tokens", "/api/v1/auth/tokens", carol, `{"principal":"ops/deployer"}`, http.StatusCreated)
	s.holds("POST /api/v1/auth/tokens", "api_token.create carol "+s.answer(w)["api_token"].(map[string]any)["id"].(string)+" ops done")
	s.act("DELETE /api/v1/service-accounts/{ns}/{name}", "/api/v1/service-accounts/ops/deployer", carol, nil, http.StatusNoContent,
		"service_account.delete carol ops/deployer ops done")
	s.act("DELETE /api/v1/namespaces/{namespace}", "/api/v1/namespaces/ops", carol, nil, http.StatusNoContent,
		"namespace.delete carol ops - done")

	// A group, a member put in and taken out, and the group removed.
	s.act("POST /api/v1/groups", "/api/v1/groups", carol, `{"name":"auditors"}`, http.StatusCreated,
		"group.create carol group:auditors - done")
	s.act("PUT /api/v1/groups/{group}/members/{login}", "/api/v1/groups/auditors/members/dave", carol, nil, http.StatusOK,
		"group_member.add carol group:auditors - done")
	s.act("DELETE /api/v1/groups/{group}/members/{login}", "/api/v1/groups/auditors/members/dave", carol, nil, http.StatusOK,
		"group_member.remove carol group:auditors - done")
	s.act("DELETE /api/v1/groups/{group}", "/api/v1/groups/auditors", carol, nil, http.StatusNoContent,
		"group.delete carol group:auditors - done")

	// carol gives herself the owner role in finance by the installation's power, which tells its
	// owners, none but her; then she shares it, pushes, keeps a secret, and starts and cancels a run.
	w = s.ask("POST /api/v1/{namespace}/grants", "/api/v1/finance/grants", carol, `{"principal":"carol","role":"owner"}`, http.StatusCreated)
	s.holds("POST /api/v1/{namespace}/grants", "grant.create carol "+s.answer(w)["id"].(string)+" finance done")
	// A version is a commit, its row keeping who pushed it as its author, and the page's audit log
	// names no push; what it records is the pin its tag was moved to, as recording one does.
	s.act("PUT /api/v1/{namespace}/workflows/{workflow}/versions/{commit}", "/api/v1/finance/workflows/monthly-invoicing/versions/"+theCommit, carol, aTaggedPush(t), http.StatusOK,
		"image.pin carol monthly-invoicing finance done")
	s.act("POST /api/v1/{namespace}/workflows/{workflow}/images", "/api/v1/finance/workflows/monthly-invoicing/images", carol,
		api.RecordImages{Pins: map[string]string{"ghcr.io/acme/agk-invoice:1.5.0": theImage}}, http.StatusOK,
		"image.pin carol monthly-invoicing finance done")
	w = s.ask("POST /api/v1/{namespace}/grants", "/api/v1/finance/grants", carol, `{"principal":"dave","deny":"run:read_data"}`, http.StatusCreated)
	deny := s.answer(w)["id"].(string)
	s.holds("POST /api/v1/{namespace}/grants", "grant.create carol "+deny+" finance done")
	s.act("DELETE /api/v1/{namespace}/grants/{id}", "/api/v1/finance/grants/"+deny, carol, nil, http.StatusNoContent,
		"grant.delete carol "+deny+" finance done")
	w = s.ask("POST /api/v1/{namespace}/workflows/{workflow}/grants", "/api/v1/finance/workflows/monthly-invoicing/grants", carol,
		`{"principal":"dave","role":"viewer"}`, http.StatusCreated)
	viewer := s.answer(w)["id"].(string)
	s.holds("POST /api/v1/{namespace}/workflows/{workflow}/grants", "grant.create carol "+viewer+" finance done")
	s.act("DELETE /api/v1/{namespace}/workflows/{workflow}/grants/{id}", "/api/v1/finance/workflows/monthly-invoicing/grants/"+viewer, carol, nil,
		http.StatusNoContent, "grant.delete carol "+viewer+" finance done")
	value := "hunter2-but-longer"
	s.act("PUT /api/v1/{namespace}/secrets/{name}", "/api/v1/finance/secrets/billing", carol, api.Declare{Provider: "builtin", Value: &value}, http.StatusCreated,
		"secret.write carol billing finance done")
	s.act("DELETE /api/v1/{namespace}/secrets/{name}", "/api/v1/finance/secrets/billing", carol, nil, http.StatusNoContent,
		"secret.delete carol billing finance done")
	w = s.ask("POST /api/v1/{namespace}/workflows/{workflow}/runs", "/api/v1/finance/workflows/monthly-invoicing/runs", carol,
		api.Start{Commit: theCommit, Inputs: map[string]any{"orders": []any{}}}, http.StatusAccepted)
	run := s.answer(w)["run"].(string)
	s.holds("POST /api/v1/{namespace}/workflows/{workflow}/runs", "run.trigger carol "+run+" finance done")
	s.act("POST /api/v1/runs/{run}/cancel", "/api/v1/runs/"+run+"/cancel", carol, nil, http.StatusAccepted,
		"run.cancel carol "+run+" finance done")

	// finance comes to forbid passwords, which takes erin's, erin holding a role there.
	w = s.act("POST /api/v1/users", "/api/v1/users", carol, `{"login":"erin"}`, http.StatusCreated,
		"user.create carol erin - done", "enrolment.issue carol erin - done")
	w = s.ask("POST /api/v1/auth/password/enrol", "/api/v1/auth/password/enrol", actor{},
		map[string]string{"code": s.code(s.answer(w)["enrolment"].(map[string]any)["link"]), "password": "erin's own passphrase"}, http.StatusOK)
	erinsPassword := s.answer(w)["credential"].(map[string]any)["id"].(string)
	s.holds("POST /api/v1/auth/password/enrol", "credential.enrol erin "+erinsPassword+" - done", "enrolment.use erin erin - done",
		"grant.create installation * erin done", "namespace.create installation erin - done", "signin.succeed erin erin - done")
	w = s.ask("POST /api/v1/{namespace}/grants", "/api/v1/finance/grants", carol, `{"principal":"erin","role":"viewer"}`, http.StatusCreated)
	s.holds("POST /api/v1/{namespace}/grants", "grant.create carol "+s.answer(w)["id"].(string)+" finance done")
	s.act("PUT /api/v1/{namespace}/auth/policy", "/api/v1/finance/auth/policy", carol, `{"password":"forbidden"}`, http.StatusOK,
		"policy.change carol finance finance done", "credential.remove carol "+erinsPassword+" - done")
	s.act("DELETE /api/v1/users/{login}", "/api/v1/users/erin", carol, nil, http.StatusNoContent,
		"namespace.delete carol erin - done", "user.delete carol erin - done")

	// A pool, a join token, a machine joining with it, and the runner drained and revoked.
	s.act("POST /api/v1/runner-pools", "/api/v1/runner-pools", carol, aPool(), http.StatusCreated,
		"runner_pool.create carol dmz - done")
	w = s.ask("POST /api/v1/runner-pools/{pool}/join-tokens", "/api/v1/runner-pools/dmz/join-tokens", carol, api.Issue{Labels: []string{"zone=dmz"}}, http.StatusCreated)
	issued := s.answer(w)["join_token"].(map[string]any)
	s.holds("POST /api/v1/runner-pools/{pool}/join-tokens", "join_token.issue carol "+issued["id"].(string)+" - done")
	joined := s.ask("POST /api/v1/runners", "/api/v1/runners", actor{}, aMachine(issued["token"].(string)), http.StatusCreated)
	runner := s.answer(joined)["runner"].(string)
	s.holds("POST /api/v1/runners", "runner.join carol "+runner+" - done")
	s.act("POST /api/v1/runners/{runner}/drain", "/api/v1/runners/"+runner+"/drain", carol, `{"reason":"moving racks"}`, http.StatusOK,
		"runner.drain carol "+runner+" - done")
	s.act("POST /api/v1/runners/{runner}/revoke", "/api/v1/runners/"+runner+"/revoke", carol, `{"reason":"moved"}`, http.StatusOK,
		"runner.revoke carol "+runner+" - done")

	// carol widening her own access in dave's namespace tells dave, who dismisses it; and dave
	// signs out. Neither is an act the log records.
	w = s.ask("POST /api/v1/{namespace}/grants", "/api/v1/dave/grants", carol, `{"principal":"carol","role":"viewer"}`, http.StatusCreated)
	s.holds("POST /api/v1/{namespace}/grants", "grant.create carol "+s.answer(w)["id"].(string)+" dave done")
	w = s.act("GET /api/v1/me", "/api/v1/me", dave, nil, http.StatusOK)
	told, _ := s.answer(w)["notifications"].([]any)
	if len(told) != 1 {
		t.Fatalf("dave is told %v", told)
	}
	s.act("DELETE /api/v1/me/notifications/{id}", "/api/v1/me/notifications/"+told[0].(map[string]any)["id"].(string), dave, nil, http.StatusNoContent)
	s.act("POST /api/v1/auth/sign-out", "/api/v1/auth/sign-out", enrolling, nil, http.StatusNoContent)

	// Every route serve builds is here: one that changes something has a case recording its act,
	// or says why it records none, and one that reads is listed as one.
	served := map[string]bool{}
	for _, r := range in.router.Routes() {
		route := r.Method + " " + r.Pattern
		served[route] = true
		_, silent := recordsNothing[route]
		switch {
		case slices.Contains(reads, route):
			if r.Method != "GET" {
				t.Errorf("%s is listed as a read", route)
			}
		case silent && s.record[route]:
			t.Errorf("%s records an act, and is listed as recording none", route)
		case silent:
		case !s.record[route]:
			t.Errorf("%s changes something, and no case here holds the act it records", route)
		}
	}
	for _, route := range append(slices.Clone(reads), slices.Collect(func(yield func(string) bool) {
		for route := range recordsNothing {
			if !yield(route) {
				return
			}
		}
	})...) {
		if !served[route] {
			t.Errorf("%s is listed here, and serve builds no such route", route)
		}
	}
}

// aTaggedPush is the ordinary push naming its image by a tag, with the digest agk push resolved it
// to, which pins the tag in the repository.
func aTaggedPush(t *testing.T) api.Push {
	t.Helper()
	const tag = "ghcr.io/acme/agk-invoice:1.4.0"
	document := strings.ReplaceAll(theWorkflow, theImage, tag)
	m, err := brick.ParseManifest([]byte(theManifest))
	if err != nil {
		t.Fatal(err)
	}
	v, err := version.Capture(fstest.MapFS{"agentiik.yaml": &fstest.MapFile{Data: []byte(document)}}, "agentiik.yaml", map[string]brick.Manifest{tag: m})
	if err != nil {
		t.Fatal(err)
	}
	return api.Push{
		Entry: v.Entry, Document: v.Document, Includes: v.Includes, Manifests: v.Manifests,
		Images: map[string]string{tag: theImage}, Branch: "main",
		Tree: map[string]api.PushFile{"agentiik.yaml": {Content: []byte(document), Mode: "0644"}},
	}
}
