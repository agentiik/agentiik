package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/password"
)

// The installation's own authorisation server, against a real PostgreSQL: the authorisation page
// naming what the client asks and refusing what OAuth 2.1 refuses, on a page where the client or its
// redirect URI does not hold and at a link back to it otherwise; the sign-ins that allow a client,
// and those that mint it no code; and the token endpoint's refusals, a refresh that spends nothing it
// refuses, and one asking for less narrowing the grant. The end-to-end flow, over HTTP and through
// what serve builds, is cmd/agentiik-api's conformance test.

const (
	issuer         = "https://agentiik.example.com"
	clientRedirect = "http://127.0.0.1:4711/callback"
	alicesPassword = "alice's own passphrase"
)

// authorisation is an installation serving the authorisation server, the password sign-in, agk
// login's exchange, the tokens and the sign-in page, with alice holding a password, and a client
// publishing its document at clientID.
type authorisation struct {
	t        *testing.T
	h        http.Handler
	pool     *db.Pool
	clientID string
	verifier string
}

// anAuthorisationServer is that installation, its policy requiring a passkey where required is set
// and letting a password sign in fully otherwise.
func anAuthorisationServer(t *testing.T, required bool) *authorisation {
	t.Helper()
	var id string
	document := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client.json" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"client_id": id, "client_name": "Example <client>", "redirect_uris": []string{clientRedirect}})
	}))
	t.Cleanup(document.Close)
	id = document.URL + "/client.json"

	pool, _ := dbtest.Open(t)
	passkey := "optional"
	if required {
		passkey = "required"
	}
	if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "alice", Profile: db.Profile{GivenName: "Alice"}}); err != nil {
			return err
		}
		hash, err := password.Hash(alicesPassword)
		if err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "alice-password", Login: "alice", Type: db.CredentialPassword, PasswordHash: hash}); err != nil {
			return err
		}
		bound := false
		return w.SetInstallationPolicy(ctx, db.AuthPolicy{Password: "allowed", Passkey: passkey, UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	principals, err := api.NewPrincipals(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := principals.AcceptSessions(issuer); err != nil {
		t.Fatal(err)
	}
	if err := principals.AcceptOAuth(issuer); err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(principals, principals.Identify)
	if err != nil {
		t.Fatal(err)
	}
	oauth, err := api.NewOAuth(rt, api.OAuthOptions{Pool: pool, PublicURL: issuer, Fetch: document.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewPasswords(rt, api.PasswordOptions{Pool: pool, PublicURL: issuer, Identify: principals.Identify, OAuth: oauth}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewExchange(rt, api.ExchangeOptions{Pool: pool, PublicURL: issuer}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewTokens(rt, api.TokenOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewSignIn(rt, api.SignInOptions{Pool: pool, PublicURL: issuer, Sessions: principals, Passwords: true, OAuth: oauth}); err != nil {
		t.Fatal(err)
	}
	return &authorisation{t: t, h: rt, pool: pool, clientID: id, verifier: strings.Repeat("v", 43)}
}

// challenge is the SHA-256 of the client's verifier, as its code_challenge.
func (a *authorisation) challenge() string {
	sum := sha256.Sum256([]byte(a.verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// asked is an authorisation request for /mcp, with what changes changes.
func (a *authorisation) asked(changes map[string]string) url.Values {
	q := url.Values{
		"response_type": {"code"}, "client_id": {a.clientID}, "redirect_uri": {clientRedirect},
		"code_challenge": {a.challenge()}, "code_challenge_method": {"S256"},
		"resource": {issuer + "/mcp"}, "scope": {"workflow:read run:read"}, "state": {"the client's state"},
	}
	for k, v := range changes {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	return q
}

// signIn signs alice in with her password from the page, carrying body's members besides, and
// answers the status and the answer.
func (a *authorisation) signIn(body map[string]any) (int, map[string]any) {
	a.t.Helper()
	body["login"], body["password"] = "alice", alicesPassword
	return a.post("/api/v1/auth/login", "application/json", must(json.Marshal(body)))
}

func (a *authorisation) post(path, kind string, body []byte) (int, map[string]any) {
	a.t.Helper()
	r := httptest.NewRequestWithContext(a.t.Context(), "POST", path, bytes.NewReader(body))
	r.Header.Set("Content-Type", kind)
	r.Header.Set("Origin", issuer)
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)
	var answer map[string]any
	json.Unmarshal(w.Body.Bytes(), &answer)
	return w.Code, answer
}

// code is a code alice's sign-in mints for the client, the request changed by changes.
func (a *authorisation) code(changes map[string]string) string {
	a.t.Helper()
	q := a.asked(changes)
	status, answer := a.signIn(map[string]any{"authorization": map[string]string{
		"client_id": q.Get("client_id"), "redirect_uri": q.Get("redirect_uri"), "code_challenge": q.Get("code_challenge"),
		"resource": q.Get("resource"), "scope": q.Get("scope"), "state": q.Get("state"),
	}})
	back, _ := answer["redirect_to"].(string)
	u, err := url.Parse(back)
	if status != http.StatusOK || err != nil || !strings.HasPrefix(back, clientRedirect+"?") {
		a.t.Fatalf("the sign-in allowing the client answered %d %v", status, answer)
	}
	if u.Query().Get("iss") != issuer || u.Query().Get("state") != q.Get("state") {
		a.t.Errorf("the browser is sent back to %s", back)
	}
	return u.Query().Get("code")
}

// token asks the token endpoint with form.
func (a *authorisation) token(form url.Values) (int, map[string]any) {
	a.t.Helper()
	return a.post("/api/v1/auth/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()))
}

func (a *authorisation) redeemed(code string, changes map[string]string) url.Values {
	f := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {a.verifier},
		"client_id": {a.clientID}, "redirect_uri": {clientRedirect},
	}
	for k, v := range changes {
		f.Set(k, v)
	}
	return f
}

// The page names the client by its document's name and its host beside it, where the browser goes
// back to, flagged as a program on this machine, and each permission asked, and offers the person a
// sign-in and a way to deny; every element its script names is on it. A client or a redirect URI that
// does not hold is a page sending the browser nowhere, and every other refusal a page with a link back
// to the client carrying OAuth's error, the client's state and iss.
func TestTheAuthorisationPageNamesTheClientAndRefusesWhatOAuthRefuses(t *testing.T) {
	a := anAuthorisationServer(t, false)
	w := fetched(t, a.h, "/auth/authorize?"+a.asked(nil).Encode())
	page := w.Body.String()
	host := strings.TrimPrefix(strings.TrimSuffix(a.clientID, "/client.json"), "https://")
	for _, want := range []string{"Example &lt;client&gt;", "<code>" + host + "</code>", "<code>127.0.0.1:4711</code>", "a program on this machine",
		"<code>workflow:read</code>", "<code>run:read</code>", "/mcp, everything your account reaches there", `data-code-challenge="` + a.challenge() + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page holds no %q:\n%s", want, page)
		}
	}
	if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != pagePolicy || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the page answered %d with %v", w.Code, w.Header())
	}
	deny := html.UnescapeString(regexp.MustCompile(`id="deny" href="([^"]*)"`).FindStringSubmatch(page)[1])
	if denied, err := url.Parse(deny); err != nil || !strings.HasPrefix(deny, clientRedirect+"?") || denied.Query().Get("error") != "access_denied" ||
		denied.Query().Get("iss") != issuer || denied.Query().Get("state") != "the client's state" {
		t.Errorf("Deny sends the browser to %s", deny)
	}
	// Its part of the script, and what the helpers it calls name, the page itself, the status and
	// the problem: the sections the other pages draw, a sign-in again, the credentials and the
	// password, are none it calls.
	script := fetched(t, a.h, "/auth/assets/page.js").Body.String()
	_, own, ok := strings.Cut(script, "// The authorisation page.")
	if !ok {
		t.Fatal("page.js no longer says where the authorisation page's part of it starts")
	}
	named := []string{"page", "status", "problem"}
	for _, m := range regexp.MustCompile(`(?:\$|getElementById|show)\("([^"]+)"`).FindAllStringSubmatch(own, -1) {
		named = append(named, m[1])
	}
	for _, id := range named {
		if n := strings.Count(page, ` id="`+id+`"`); n != 1 {
			t.Errorf("the page holds %d elements of id %q, which page.js names", n, id)
		}
	}

	for name, changes := range map[string]map[string]string{
		"a redirect URI the document does not name": {"redirect_uri": "http://127.0.0.1:4711/other"},
		"a client whose document is not there":      {"client_id": strings.Replace(a.clientID, "client.json", "gone.json", 1)},
		"a client_id that is no https URL":          {"client_id": "http://client.example.com/oauth.json"},
		"no client_id":                              {"client_id": ""},
	} {
		w := fetched(t, a.h, "/auth/authorize?"+a.asked(changes).Encode())
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "Return to") || strings.Contains(w.Body.String(), "4711") {
			t.Errorf("%s answered %d:\n%s", name, w.Code, w.Body)
		}
	}
	twice := a.asked(nil)
	twice.Add("client_id", a.clientID)
	if w := fetched(t, a.h, "/auth/authorize?"+twice.Encode()); w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "Return to") {
		t.Errorf("a client_id written twice answered %d", w.Code)
	}

	for want, changes := range map[string]map[string]string{
		"invalid_scope":             {"scope": "workflow:read admin"},
		"unsupported_response_type": {"response_type": "token"},
		"invalid_request":           {"code_challenge_method": "plain"},
		"invalid_target":            {"resource": "https://elsewhere.example.com/mcp"},
	} {
		w := fetched(t, a.h, "/auth/authorize?"+a.asked(changes).Encode())
		m := regexp.MustCompile(`<a href="([^"]*)">Return to 127\.0\.0\.1:4711</a>`).FindStringSubmatch(w.Body.String())
		if w.Code != http.StatusBadRequest || m == nil {
			t.Errorf("a request refused with %s answered %d:\n%s", want, w.Code, w.Body)
			continue
		}
		back, err := url.Parse(html.UnescapeString(m[1]))
		if err != nil || back.Query().Get("error") != want || back.Query().Get("iss") != issuer || back.Query().Get("state") != "the client's state" || back.Query().Get("error_description") == "" {
			t.Errorf("a request refused with %s links back to %s", want, m[1])
		}
	}
}

// A sign-in hands the client a code only from a full session: a password's where the policy requires a
// passkey only enrols, and mints nothing. A sign-in naming agk login and a client both, or a client
// whose document no longer holds, is refused before anything is checked.
func TestASignInMintsAClientACodeOnlyWhereItsSessionIsFull(t *testing.T) {
	a := anAuthorisationServer(t, true)
	authorization := map[string]string{"client_id": a.clientID, "redirect_uri": clientRedirect, "code_challenge": a.challenge(), "resource": issuer + "/mcp"}
	status, answer := a.signIn(map[string]any{"authorization": authorization})
	if status != http.StatusOK || answer["session"] != "enrolment" || answer["redirect_to"] != nil {
		t.Errorf("a password where a passkey is required answered %d %v", status, answer)
	}
	status, answer = a.signIn(map[string]any{"authorization": authorization, "terminal": map[string]string{"redirect_uri": "http://127.0.0.1:53682/", "code_challenge": a.challenge()}})
	if status != http.StatusBadRequest {
		t.Errorf("a sign-in for agk login and a client answered %d %v", status, answer)
	}
	gone := map[string]string{"client_id": strings.Replace(a.clientID, "client.json", "gone.json", 1), "redirect_uri": clientRedirect, "code_challenge": a.challenge(), "resource": issuer + "/mcp"}
	if status, answer := a.signIn(map[string]any{"authorization": gone}); status != http.StatusBadRequest || !strings.HasPrefix(answer["error"].(string), "authorization: ") {
		t.Errorf("a sign-in for a client whose document is not there answered %d %v", status, answer)
	}
}

// The token endpoint takes a form naming each parameter once and a grant type it serves; a code is
// spent by a request naming another client, another redirect URI or another resource, and answers
// nothing then; agk login's code opens nothing here, nor a client's at the exchange. A refresh naming
// another client spends nothing, one asking for a scope the grant does not keep is refused, and one
// asking for less narrows the grant.
func TestTheTokenEndpointRefusesAsOAuthDoes(t *testing.T) {
	a := anAuthorisationServer(t, false)
	refused := func(what string, status int, answer map[string]any, want string) {
		t.Helper()
		if status != http.StatusBadRequest || answer["error"] != want || answer["error_description"] == "" {
			t.Errorf("%s answered %d %v, want %s", what, status, answer, want)
		}
	}
	code := a.code(nil)
	status, answer := a.post("/api/v1/auth/oauth/token", "application/json", must(json.Marshal(map[string]string{"grant_type": "authorization_code", "code": code})))
	refused("a JSON body", status, answer, "invalid_request")
	twice := a.redeemed(code, nil)
	twice.Add("grant_type", "authorization_code")
	status, answer = a.token(twice)
	refused("a grant_type written twice", status, answer, "invalid_request")
	status, answer = a.token(url.Values{"grant_type": {"password"}, "username": {"alice"}, "password": {alicesPassword}})
	refused("the password grant", status, answer, "unsupported_grant_type")
	status, answer = a.token(a.redeemed(code, map[string]string{"code_verifier": ""}))
	refused("no verifier", status, answer, "invalid_request")

	for name, changes := range map[string]map[string]string{
		"another client":       {"client_id": "https://other.example.com/client.json"},
		"another redirect URI": {"redirect_uri": "http://127.0.0.1:4711/other"},
	} {
		code := a.code(nil)
		status, answer := a.token(a.redeemed(code, changes))
		refused(name, status, answer, "invalid_grant")
		status, answer = a.token(a.redeemed(code, nil))
		refused("the code after "+name+" spent it", status, answer, "invalid_grant")
	}
	status, answer = a.token(a.redeemed(a.code(nil), map[string]string{"resource": issuer + "/mcp/collections/01JR8Q2W6H3V0X9K4M7N5P1T2C"}))
	refused("another resource", status, answer, "invalid_target")

	// agk login's code at the token endpoint, and a client's at the exchange.
	status, answer = a.signIn(map[string]any{"terminal": map[string]string{"redirect_uri": "http://127.0.0.1:53682/", "code_challenge": a.challenge()}})
	terminal, _ := url.Parse(answer["redirect_to"].(string))
	status, answer = a.token(a.redeemed(terminal.Query().Get("code"), nil))
	refused("agk login's code", status, answer, "invalid_grant")
	if status, answer := a.post("/api/v1/auth/exchange", "application/json", must(json.Marshal(map[string]string{"code": a.code(nil), "code_verifier": a.verifier}))); status != http.StatusUnauthorized {
		t.Errorf("a client's code at agk login's exchange answered %d %v", status, answer)
	}

	status, pair := a.token(a.redeemed(a.code(nil), nil))
	if status != http.StatusOK || pair["scope"] != "workflow:read run:read" {
		t.Fatalf("the code answered %d %v", status, pair)
	}
	refresh := func(token string, changes map[string]string) (int, map[string]any) {
		f := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}, "client_id": {a.clientID}}
		for k, v := range changes {
			f.Set(k, v)
		}
		return a.token(f)
	}
	status, answer = refresh(pair["refresh_token"].(string), map[string]string{"client_id": "https://other.example.com/client.json"})
	refused("a refresh naming another client", status, answer, "invalid_grant")
	status, answer = refresh(pair["refresh_token"].(string), map[string]string{"scope": "workflow:read grant:manage"})
	refused("a refresh asking for more", status, answer, "invalid_scope")
	status, narrowed := refresh(pair["refresh_token"].(string), map[string]string{"scope": "run:read"})
	if status != http.StatusOK || narrowed["scope"] != "run:read" {
		t.Fatalf("a refresh asking for less, after two refused, answered %d %v", status, narrowed)
	}
	status, again := refresh(narrowed["refresh_token"].(string), nil)
	if status != http.StatusOK || again["scope"] != "run:read" {
		t.Errorf("the grant a refresh narrowed refreshes as %d %v", status, again)
	}
}
