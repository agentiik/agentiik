package api_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/password"
)

// agk login's hand-off and POST /api/v1/auth/exchange, through routers built as serve builds them,
// over a real PostgreSQL: a sign-in agk login started, with a password or a passkey, answers where
// the page sends the browser back to agk, with a one-time code agk trades, with its verifier, for an
// API token of whoever signed in; the code is taken once, lapses after a minute, holds to its
// verifier and to its account, and mints what the policy says when it is asked.

// redirectTo is openapi.json's redirectTo.
var redirectTo = regexp.MustCompile(`^http://(?:127\.0\.0\.1|\[::1\]):[0-9]+(?:/[A-Za-z0-9._~/-]*)?\?code=agkcode_[A-Za-z0-9_-]{43,}$`)

// loopback is where the tests' agk login listens.
const loopback = "http://127.0.0.1:53682/callback"

// pkce is a verifier as agk login makes one, 32 random bytes in base64url, and its S256 challenge.
func pkce(t *testing.T) (verifier, challenge string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// exchanging is passwordsOf serving the exchange too, where passkeys are optional.
func exchanging(t *testing.T) passwordsOf {
	t.Helper()
	in := exchangingAt(t, "https://agentiik.example.com")
	in.policy(t, "allowed", "optional")
	return in
}

// exchangingAt is passwordsOf on publicURL, serving the exchange too.
func exchangingAt(t *testing.T, publicURL string) passwordsOf {
	t.Helper()
	in := passwordsAt(t, publicURL, false)
	if _, err := api.NewExchange(in.h.(*api.Router), api.ExchangeOptions{
		Pool: in.pool, PublicURL: in.origin, Now: func() time.Time { return *in.clock },
	}); err != nil {
		t.Fatal(err)
	}
	return in
}

// terminalLogin signs login in with their password for agk login, whose page was opened with
// challenge, and answers what the route said.
func (in passwordsOf) terminalLogin(t *testing.T, login, challenge string) (int, api.SignedIn, string) {
	t.Helper()
	w := in.login(t, fmt.Sprintf(`{"login":%q,"password":%q,"terminal":{"redirect_uri":%q,"code_challenge":%q}}`,
		login, thePasswords[login], loopback, challenge), "")
	var answer api.SignedIn
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, answer, w.Body.String()
}

// handedOff signs login in for agk login and answers the code redirect_to carries, with the
// verifier it answers, failing the test where the route hands none.
func (in passwordsOf) handedOff(t *testing.T, login string) (code, verifier string) {
	t.Helper()
	verifier, challenge := pkce(t)
	status, answer, body := in.terminalLogin(t, login, challenge)
	if status != http.StatusOK || answer.Session != api.SessionFull {
		t.Fatalf("%s signing in for agk login answered %d %s", login, status, body)
	}
	if !redirectTo.MatchString(answer.RedirectTo) || !strings.HasPrefix(answer.RedirectTo, loopback+"?code=") {
		t.Fatalf("%s signing in for agk login was sent to %q", login, answer.RedirectTo)
	}
	return strings.TrimPrefix(answer.RedirectTo, loopback+"?code="), verifier
}

// exchange trades a code and a verifier, with a label, as agk login does: no credential, no Origin.
func exchange(t *testing.T, h http.Handler, code, verifier, label string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "device_label": label})
	if err != nil {
		t.Fatal(err)
	}
	return sent(t, h, "POST", "/api/v1/auth/exchange", "", string(body))
}

// issued reads a token the exchange answered, failing the test unless it answered one.
func issued(t *testing.T, w *httptest.ResponseRecorder) api.IssuedToken {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("the exchange answered %d %s", w.Code, w.Body)
	}
	var got api.IssuedToken
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A password sign-in agk login started opens a full session and answers redirect_to, the loopback
// address with a one-time code; agk trades the code and its verifier for a token of the account
// that signed in, expiring in 90 days, labelled as agk asked and shown this once, which opens the
// installation at once and is recorded as api_token.create by that account, with the credential
// that signed in; and the code is spent.
func TestAPasswordSignInForAgkLoginHandsItACodeTradedForAToken(t *testing.T) {
	in := exchanging(t)
	code, verifier := in.handedOff(t, "alice")
	if n := in.count(t, `select count(*) from exchange_codes where login = 'alice' and credential = 'alice-password'`); n != 1 {
		t.Errorf("the sign-in kept %d codes for alice's password", n)
	}

	w := exchange(t, in.h, code, verifier, "agk on alice-laptop")
	got := issued(t, w)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the token is answered with Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	now := *in.clock
	if !regexp.MustCompile(`^agktoken_[A-Za-z0-9_-]{43}$`).MatchString(got.Token) || got.APIToken.Principal != "alice" ||
		got.APIToken.DeviceLabel != "agk on alice-laptop" || got.APIToken.Scope != nil ||
		!got.APIToken.ExpiresAt.Equal(now.AddDate(0, 0, 90)) {
		t.Errorf("the exchange answered %s", w.Body)
	}
	listed := sent(t, in.h, "GET", "/api/v1/auth/tokens", got.Token, "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), got.APIToken.ID) {
		t.Errorf("the token minted lists alice's tokens with %d %s", listed.Code, listed.Body)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'api_token.create' and actor = 'alice' and target = $1
		and detail::jsonb @> '{"principal":"alice","device_label":"agk on alice-laptop","credential":"alice-password"}'`, got.APIToken.ID); n != 1 {
		t.Errorf("the token is recorded %d times as alice's api_token.create", n)
	}

	again := exchange(t, in.h, code, verifier, "agk on alice-laptop")
	if again.Code != http.StatusUnauthorized || !strings.Contains(again.Body.String(), "that code opens nothing") {
		t.Errorf("the code traded a second time answered %d %s", again.Code, again.Body)
	}
	if n := in.count(t, `select count(*) from api_tokens where principal = 'alice'`); n != 1 {
		t.Errorf("alice holds %d tokens after one exchange and its replay", n)
	}
}

// A code is spent by the first presentation the schema accepts, whatever verifier it carries:
// presented with one that is not the one its challenge was made of, it opens nothing, and nor does
// it afterwards with the right one, so that whoever saw it on the way cannot try again, and agk,
// told so, signs in again.
func TestACodeIsSpentByAVerifierThatIsNotItsOwn(t *testing.T) {
	in := exchanging(t)
	code, verifier := in.handedOff(t, "alice")
	other, _ := pkce(t)
	if w := exchange(t, in.h, code, other, "x"); w.Code != http.StatusUnauthorized || w.Body.String() != `{"error":"that code opens nothing: it was used already, is older than a minute, was never issued, or does not answer this verifier. Run agk login again"}`+"\n" {
		t.Errorf("a code with another verifier answered %d %s", w.Code, w.Body)
	}
	if w := exchange(t, in.h, code, verifier, "x"); w.Code != http.StatusUnauthorized {
		t.Errorf("a code presented with another verifier first traded with its own answered %d %s", w.Code, w.Body)
	}
	// Nor is the challenge itself a verifier, which is what a verifier sent in the clear would be.
	_, challenge := pkce(t)
	_, answer, _ := in.terminalLogin(t, "alice", challenge)
	code = strings.TrimPrefix(answer.RedirectTo, loopback+"?code=")
	if w := exchange(t, in.h, code, challenge, "x"); w.Code != http.StatusUnauthorized {
		t.Errorf("a code traded with a challenge answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from api_tokens`); n != 0 {
		t.Errorf("%d tokens were minted by codes that opened nothing", n)
	}
}

// A code lapses a minute after the sign-in that minted it, and one never minted opens nothing, with
// the same sentence; a sign-in minting a code removes those whose minute has passed.
func TestACodeLapsesAfterAMinute(t *testing.T) {
	in := exchanging(t)
	code, verifier := in.handedOff(t, "alice")
	*in.clock = in.clock.Add(time.Minute)
	if w := exchange(t, in.h, code, verifier, "x"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "that code opens nothing") {
		t.Errorf("a code a minute old answered %d %s", w.Code, w.Body)
	}
	code, verifier = in.handedOff(t, "alice")
	*in.clock = in.clock.Add(59 * time.Second)
	issued(t, exchange(t, in.h, code, verifier, "x"))

	never := "agkcode_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if w := exchange(t, in.h, never, verifier, "x"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "that code opens nothing") {
		t.Errorf("a code never minted answered %d %s", w.Code, w.Body)
	}

	in.handedOff(t, "alice")
	*in.clock = in.clock.Add(2 * time.Minute)
	in.handedOff(t, "alice")
	if n := in.count(t, `select count(*) from exchange_codes`); n != 1 {
		t.Errorf("%d codes are kept once one past its minute was followed by another", n)
	}
}

// A code is bound to the account that signed in and the credential it signed in with: once the
// account is suspended, removed and created again under the same login, or has lost that credential
// or set its password anew, the code opens nothing, with the sentence every other code that opens
// nothing gets, and no token is minted for anybody.
func TestACodeOpensNothingOnceItsAccountOrCredentialIsGone(t *testing.T) {
	again := func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "alice", DisplayName: "Another Alice"}); err != nil {
			return err
		}
		hash, err := password.Hash(thePasswords["alice"])
		if err != nil {
			return err
		}
		return w.AddCredential(ctx, db.Credential{ID: "alice-password", Login: "alice", Type: db.CredentialPassword, PasswordHash: hash})
	}
	for what, change := range map[string]func(context.Context, *db.Wide) error{
		"suspended": func(ctx context.Context, w *db.Wide) error {
			return w.UpdateUser(ctx, db.User{Login: "alice", DisplayName: "Alice", Suspended: true})
		},
		"removed and created again": func(ctx context.Context, w *db.Wide) error {
			if err := w.RemoveNamespace(ctx, "alice"); err != nil {
				return err
			}
			if err := w.RemovePrincipal(ctx, "alice"); err != nil {
				return err
			}
			return again(ctx, w)
		},
		"its password set anew": func(ctx context.Context, w *db.Wide) error {
			hash, err := password.Hash("alice's next passphrase")
			if err != nil {
				return err
			}
			_, _, err = w.SetPassword(ctx, "alice", "unused", hash, time.Now().UTC())
			return err
		},
		"its password removed and set again": func(ctx context.Context, w *db.Wide) error {
			if err := w.RemoveCredential(ctx, "alice", "alice-password"); err != nil {
				return err
			}
			hash, err := password.Hash(thePasswords["alice"])
			if err != nil {
				return err
			}
			return w.AddCredential(ctx, db.Credential{ID: "alice-password", Login: "alice", Type: db.CredentialPassword, PasswordHash: hash})
		},
	} {
		t.Run(what, func(t *testing.T) {
			in := exchanging(t)
			code, verifier := in.handedOff(t, "alice")
			if err := in.pool.Installation(t.Context(), db.Identity, change); err != nil {
				t.Fatal(err)
			}
			if w := exchange(t, in.h, code, verifier, "x"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "that code opens nothing") {
				t.Errorf("the code answered %d %s", w.Code, w.Body)
			}
			if n := in.count(t, `select count(*) from api_tokens`); n != 0 {
				t.Errorf("%d tokens were minted", n)
			}
		})
	}
}

// A code mints a token of the account that signed in and nobody else, whatever credential the
// exchange's request carries beside it: the route reads none, the code and the verifier being what
// authenticate it.
func TestACodeMintsForItsOwnAccountWhateverTheRequestCarries(t *testing.T) {
	in := exchanging(t)
	code, verifier := in.handedOff(t, "alice")
	carolsCode, carolsVerifier := in.handedOff(t, "carol")
	carol := issued(t, exchange(t, in.h, carolsCode, carolsVerifier, "carol"))
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	got := issued(t, sent(t, in.h, "POST", "/api/v1/auth/exchange", carol.Token, string(body)))
	if got.APIToken.Principal != "alice" || got.APIToken.DeviceLabel != "" {
		t.Errorf("alice's code traded with carol's token beside it minted %+v", got.APIToken)
	}
}

// What a code mints is the policy's to say when it is traded, as what a session may do is at each
// request: a code a password minted where the policy has since come to require a passkey the
// account does not hold is a 403 saying it may only enrol, one where passwords have since been
// forbidden a 403 naming the setting, and neither mints a token.
func TestAPolicyChangedSinceTheSignInDecidesWhatTheCodeMints(t *testing.T) {
	in := exchanging(t)
	code, verifier := in.handedOff(t, "alice")
	in.policy(t, "allowed", "required")
	if w := exchange(t, in.h, code, verifier, "x"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "enrols passkeys and nothing else: it mints no token") {
		t.Errorf("a code traded once a passkey is required answered %d %s", w.Code, w.Body)
	}
	in.policy(t, "allowed", "optional")
	code, verifier = in.handedOff(t, "alice")
	in.policy(t, "forbidden", "required")
	if w := exchange(t, in.h, code, verifier, "x"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Errorf("a code traded once passwords are forbidden answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from api_tokens`); n != 0 {
		t.Errorf("%d tokens were minted", n)
	}
}

// On an installation addressed by an IP address, where no passkey can be used, a password is how
// agk login signs in: the policy is applied with passwords allowed and no passkey required, as the
// sign-in applied it, whatever the stored policy says, and the code mints a token.
func TestAnInstallationAddressedByAnIPAddressTradesAPasswordsCode(t *testing.T) {
	in := exchangingAt(t, "https://192.0.2.10")
	in.policy(t, "allowed", "required")
	code, verifier := in.handedOff(t, "alice")
	if got := issued(t, exchange(t, in.h, code, verifier, "x")); got.APIToken.Principal != "alice" {
		t.Errorf("the code minted %+v", got.APIToken)
	}
}

// A password sign-in whose session may only enrol hands agk login no code: its answer carries no
// redirect_to and nothing is kept, since such a session "cannot mint a token".
func TestASessionThatMayOnlyEnrolIsHandedNoCode(t *testing.T) {
	in := exchanging(t)
	in.policy(t, "allowed", "required")
	_, challenge := pkce(t)
	status, answer, body := in.terminalLogin(t, "alice", challenge)
	if status != http.StatusOK || answer.Session != api.SessionEnrolment || answer.RedirectTo != "" || strings.Contains(body, "redirect_to") {
		t.Errorf("a sign-in that may only enrol answered %d %s", status, body)
	}
	if n := in.count(t, `select count(*) from exchange_codes`); n != 0 {
		t.Errorf("%d codes were kept for a session that may only enrol", n)
	}
}

// A principal holding as many live tokens as one may is minted none by a code either.
func TestACodeMintsNoTokenPastThePrincipalsBound(t *testing.T) {
	in := exchanging(t)
	in.exec(t, `insert into api_tokens (id, hash, principal, principal_kind, created_at, expires_at)
		select lpad(i::text, 26, '0'), sha256(i::text::bytea), 'alice', 'user', now(), now() + interval '1 day'
		  from generate_series(1, 100) i`)
	code, verifier := in.handedOff(t, "alice")
	if w := exchange(t, in.h, code, verifier, "x"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "100 live tokens") {
		t.Errorf("a code of a principal holding 100 tokens answered %d %s", w.Code, w.Body)
	}
}

// What agk login hands the sign-in routes, terminal, and what it trades, are held to their schemas
// before anything is checked or spent: both halves of terminal, a loopback address by number and
// never another host, a challenge of 43 base64url characters, no member of another name; a code and
// a verifier on their grammar, a label a token can carry. A terminal left null is none.
func TestWhatAgkLoginSendsIsHeldToItsSchema(t *testing.T) {
	in := exchanging(t)
	_, challenge := pkce(t)
	for _, terminal := range []string{
		`{"redirect_uri":"http://127.0.0.1:53682/callback"}`,
		`{"code_challenge":"` + challenge + `"}`,
	} {
		w := in.login(t, fmt.Sprintf(`{"login":"alice","password":%q,"terminal":%s}`, thePasswords["alice"], terminal), "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "carries both redirect_uri and code_challenge") {
			t.Errorf("a password sign-in with terminal %s answered %d %s", terminal, w.Code, w.Body)
		}
	}
	for _, terminal := range []string{
		`{"redirect_uri":"http://localhost:53682/callback","code_challenge":"` + challenge + `"}`,
		`{"redirect_uri":"https://127.0.0.1:53682/callback","code_challenge":"` + challenge + `"}`,
		`{"redirect_uri":"http://evil.example/callback","code_challenge":"` + challenge + `"}`,
		`{"redirect_uri":"http://127.0.0.1:53682/callback?x=1","code_challenge":"` + challenge + `"}`,
		`{"redirect_uri":"http://127.0.0.1:53682/callback","code_challenge":"short"}`,
		`{"redirect_uri":"http://127.0.0.1:53682/callback","code_challenge":"` + challenge + `","code_challenge_method":"S256"}`,
		`"http://127.0.0.1:53682/callback"`,
	} {
		w := in.login(t, fmt.Sprintf(`{"login":"alice","password":%q,"terminal":%s}`, thePasswords["alice"], terminal), "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("a password sign-in with terminal %s answered %d %s", terminal, w.Code, w.Body)
		}
	}
	if w := in.login(t, fmt.Sprintf(`{"login":"alice","password":%q,"terminal":null}`, thePasswords["alice"]), ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "redirect_to") {
		t.Errorf("a password sign-in with terminal null answered %d %s", w.Code, w.Body)
	}

	code, verifier := in.handedOff(t, "alice")
	for what, body := range map[string]string{
		"a code of another kind":   fmt.Sprintf(`{"code":"agkenrol_%s","code_verifier":%q}`, strings.TrimPrefix(code, "agkcode_"), verifier),
		"a short code":             fmt.Sprintf(`{"code":"agkcode_short","code_verifier":%q}`, verifier),
		"no code":                  fmt.Sprintf(`{"code_verifier":%q}`, verifier),
		"a short verifier":         fmt.Sprintf(`{"code":%q,"code_verifier":"short"}`, code),
		"a verifier past 128":      fmt.Sprintf(`{"code":%q,"code_verifier":%q}`, code, strings.Repeat("a", 129)),
		"a verifier out of range":  fmt.Sprintf(`{"code":%q,"code_verifier":%q}`, code, verifier[:42]+"+"),
		"an empty label":           fmt.Sprintf(`{"code":%q,"code_verifier":%q,"device_label":""}`, code, verifier),
		"a label past 256":         fmt.Sprintf(`{"code":%q,"code_verifier":%q,"device_label":%q}`, code, verifier, strings.Repeat("é", 257)),
		"a member of another name": fmt.Sprintf(`{"code":%q,"code_verifier":%q,"principal":"bob"}`, code, verifier),
	} {
		if w := sent(t, in.h, "POST", "/api/v1/auth/exchange", "", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d %s", what, w.Code, w.Body)
		}
	}
	// None of those spent the code, which the schema refused before it was read.
	issued(t, exchange(t, in.h, code, verifier, "agk on alice-laptop"))
}

// A passkey sign-in agk login started answers redirect_to beside its session, a code bound to the
// passkey that signed in, which agk trades for a token; a registration carrying terminal is refused
// before anything is verified, since agk login signs in and enrols nothing.
func TestAPasskeySignInForAgkLoginHandsItACode(t *testing.T) {
	in := someCeremonies(t)
	if _, err := api.NewExchange(in.h.(*api.Router), api.ExchangeOptions{Pool: in.pool, PublicURL: publicOrigin, Now: func() time.Time { return *in.clock }}); err != nil {
		t.Fatal(err)
	}
	browser := newBrowser()
	if w := in.enrol(t, browser, in.user(t, "alice", true), "laptop"); w.Code != http.StatusOK {
		t.Fatalf("enrolling answered %d %s", w.Code, w.Body)
	}

	made, _, err := newBrowser().Create(in.options(t, `{"ceremony":"registration"}`, session(t, in.signIn(t, browser))))
	if err != nil {
		t.Fatal(err)
	}
	_, challenge := pkce(t)
	terminal := map[string]string{"redirect_uri": loopback, "code_challenge": challenge}
	registering, _ := json.Marshal(map[string]any{"ceremony": "registration", "credential": made, "terminal": terminal})
	if w := in.call(t, "POST", "/api/v1/auth/passkey/verify", string(registering), ""); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "terminal") {
		t.Errorf("a registration carrying terminal answered %d %s", w.Code, w.Body)
	}

	verifier, challenge := pkce(t)
	got, err := browser.Get(in.options(t, `{"ceremony":"assertion"}`))
	if err != nil {
		t.Fatal(err)
	}
	asserting, _ := json.Marshal(map[string]any{"ceremony": "assertion", "credential": got,
		"terminal": map[string]string{"redirect_uri": loopback, "code_challenge": challenge}})
	w := in.call(t, "POST", "/api/v1/auth/passkey/verify", string(asserting), "")
	var verified api.Verified
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &verified) != nil || verified.Login != "alice" || !redirectTo.MatchString(verified.RedirectTo) ||
		!strings.HasPrefix(verified.RedirectTo, loopback+"?code=") {
		t.Fatalf("a passkey sign-in for agk login answered %d %s", w.Code, w.Body)
	}
	session(t, w)
	if n := in.count(t, fmt.Sprintf(`select count(*) from exchange_codes where login = 'alice' and credential = %s`, quoted(got.ID))); n != 1 {
		t.Errorf("the sign-in kept %d codes for alice's passkey", n)
	}
	// Signed in with no terminal, the answer carries no redirect_to.
	if w := in.signIn(t, browser); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "redirect_to") {
		t.Errorf("a passkey sign-in of the page's own answered %d %s", w.Code, w.Body)
	}

	body, _ := json.Marshal(map[string]string{"code": strings.TrimPrefix(verified.RedirectTo, loopback+"?code="), "code_verifier": verifier, "device_label": "agk"})
	token := issued(t, sent(t, in.h, "POST", "/api/v1/auth/exchange", "", string(body)))
	if token.APIToken.Principal != "alice" {
		t.Errorf("the code minted %+v", token.APIToken)
	}

	// terminal is held to its grammar here as on the password route, before anything is verified
	// or kept.
	for _, handedOn := range []map[string]string{
		{"redirect_uri": "http://localhost:53682/callback", "code_challenge": challenge},
		{"redirect_uri": "http://evil.example/callback", "code_challenge": challenge},
		{"redirect_uri": loopback, "code_challenge": "short"},
	} {
		got, err := browser.Get(in.options(t, `{"ceremony":"assertion"}`))
		if err != nil {
			t.Fatal(err)
		}
		asserting, _ := json.Marshal(map[string]any{"ceremony": "assertion", "credential": got, "terminal": handedOn})
		if w := in.call(t, "POST", "/api/v1/auth/passkey/verify", string(asserting), ""); w.Code != http.StatusBadRequest {
			t.Errorf("a passkey sign-in with terminal %v answered %d %s", handedOn, w.Code, w.Body)
		}
	}
	if n := in.count(t, `select count(*) from exchange_codes`); n != 0 {
		t.Errorf("%d codes are kept once the one minted was traded and the others refused", n)
	}
}

// A code a synced passkey minted mints no token once device_bound_only applies to its account, as
// the session beside it opens nothing from then on: a 403 naming the setting, as the sign-in would
// be answered.
func TestACodeASyncedPasskeyMintedIsHeldToDeviceBoundOnly(t *testing.T) {
	in := someCeremonies(t)
	if _, err := api.NewExchange(in.h.(*api.Router), api.ExchangeOptions{Pool: in.pool, PublicURL: publicOrigin, Now: func() time.Time { return *in.clock }}); err != nil {
		t.Fatal(err)
	}
	synced := newBrowser()
	synced.BackupEligible, synced.BackedUp = true, true
	if w := in.enrol(t, synced, in.user(t, "bob", false), ""); w.Code != http.StatusOK {
		t.Fatalf("the registration answered %d %s", w.Code, w.Body)
	}
	// signedIn signs bob in for agk login, and answers what agk would trade.
	signedIn := func() string {
		t.Helper()
		verifier, challenge := pkce(t)
		got, err := synced.Get(in.options(t, `{"ceremony":"assertion"}`))
		if err != nil {
			t.Fatal(err)
		}
		asserting, _ := json.Marshal(map[string]any{"ceremony": "assertion", "credential": got,
			"terminal": map[string]string{"redirect_uri": loopback, "code_challenge": challenge}})
		w := in.call(t, "POST", "/api/v1/auth/passkey/verify", string(asserting), "")
		var verified api.Verified
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &verified) != nil {
			t.Fatalf("the sign-in answered %d %s", w.Code, w.Body)
		}
		body, _ := json.Marshal(map[string]string{"code": strings.TrimPrefix(verified.RedirectTo, loopback+"?code="), "code_verifier": verifier})
		return string(body)
	}
	issued(t, sent(t, in.h, "POST", "/api/v1/auth/exchange", "", signedIn()))

	traded := signedIn()
	in.exec(t, `update auth_policy set device_bound_only = true where namespace is null`)
	if w := sent(t, in.h, "POST", "/api/v1/auth/exchange", "", traded); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"device_bound_only"`) {
		t.Errorf("a synced passkey's code once device_bound_only applies answered %d %s", w.Code, w.Body)
	}
}

// quoted is a string as SQL writes it.
func quoted(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
