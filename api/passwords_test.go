package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/password"
	"github.com/agentiik/agentiik/internal/totp"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/secret"
)

// The password sign-in, through a router built as serve builds it, over a real PostgreSQL: a
// password opens a full session where the policy is met and one that only enrols where a passkey is
// required and none is held, which a passkey registered from it lifts at the next request, as a
// policy changed applies at the next; passwords forbidden are refused outright, before anything is
// hashed; every other refusal is one sentence; attempts are counted for each login and each
// address; hashing waits its turn; and a TOTP code is accepted one step either side and never twice.

// passwordsOf is an installation serving the password sign-in, the passkey ceremonies, GET
// /api/v1/me, the API tokens, the authentication policy, the caller's credentials, the users and the
// grants on https://agentiik.example.com, on a clock the test moves. alice holds a password; bob a password
// and a TOTP generator; carol a password and a passkey; dave, who is suspended, a password; and
// erin nothing. alice holds a grant in finance.
type passwordsOf struct {
	origin    string
	pool      *db.Pool
	super     string
	clock     *time.Time
	h         http.Handler
	passwords *api.PasswordAPI
	passkeys  *api.PasskeyAPI
	policies  *api.PolicyAPI
	p         *api.Principals
	totp      *secret.TOTP
	trouble   *[]error
}

// thePasswords are the passwords the users of passwordsOf hold.
var thePasswords = map[string]string{
	"alice": "correct horse battery staple", "bob": "bob's own", "carol": "carol's own", "dave": "dave's own",
}

// bobsSecret is bob's TOTP generator's secret.
var bobsSecret = []byte("bob's twenty byte ke")

func somePasswords(t *testing.T) passwordsOf {
	return passwordsAt(t, "https://agentiik.example.com", false)
}

// passwordsAt is passwordsOf on publicURL, behind the proxy AGK_PROXY_URL names where proxied is set.
func passwordsAt(t *testing.T, publicURL string, proxied bool) passwordsOf {
	t.Helper()
	pool, super := dbtest.Open(t)
	now := time.Now().UTC().Truncate(time.Second)
	var troubles []error
	var mu sync.Mutex
	in := passwordsOf{origin: strings.TrimSuffix(publicURL, "/"), pool: pool, super: super, clock: &now, trouble: &troubles}
	clock := func() time.Time { return *in.clock }
	master, err := secret.NewMaster("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secret.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	if in.totp, err = secret.NewTOTP(keys); err != nil {
		t.Fatal(err)
	}
	if in.p, err = api.NewPrincipals(pool, clock); err != nil {
		t.Fatal(err)
	}
	if err := in.p.AcceptSessions(publicURL); err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	signIns := api.NewSignIns(proxied)
	if in.passkeys, err = api.NewPasskeys(rt, api.PasskeyOptions{
		Pool: pool, PublicURL: publicURL, Identify: in.p.Identify, Now: clock, SignIns: signIns,
	}); err != nil {
		t.Fatal(err)
	}
	if in.passwords, err = api.NewPasswords(rt, api.PasswordOptions{
		Pool: pool, PublicURL: publicURL, TOTP: in.totp, SignIns: signIns, Now: clock, Identify: in.p.Identify,
		Trouble: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			troubles = append(troubles, err)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMe(rt, api.MeOptions{Pool: pool, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewTokens(rt, api.TokenOptions{Pool: pool, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if in.policies, err = api.NewPolicies(rt, api.PolicyOptions{Pool: pool, PublicURL: publicURL, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewCredentials(rt, api.CredentialOptions{Pool: pool, PublicURL: publicURL, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: pool, PublicURL: publicURL, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewSharing(rt, api.SharingOptions{Pool: pool, PublicURL: publicURL, Now: clock}); err != nil {
		t.Fatal(err)
	}
	in.h = rt

	in.exec(t, `insert into namespaces (name) values ('finance')`)
	err = pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{
			{Login: "alice", DisplayName: "Alice"}, {Login: "bob", DisplayName: "Bob"},
			{Login: "carol", DisplayName: "Carol"}, {Login: "dave", DisplayName: "Dave", Suspended: true},
			{Login: "erin", DisplayName: "Erin"},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		for login, clear := range thePasswords {
			hash, err := password.Hash(clear)
			if err != nil {
				return err
			}
			if err := w.AddCredential(ctx, db.Credential{ID: login + "-password", Login: login, Type: db.CredentialPassword, PasswordHash: hash}); err != nil {
				return err
			}
		}
		sealed, err := in.totp.SealTOTP("bob", "bob-totp", bobsSecret)
		if err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "bob-totp", Login: "bob", Type: db.CredentialTOTP, TOTPSealed: sealed}); err != nil {
			return err
		}
		return w.AddCredential(ctx, db.Credential{ID: "carol-passkey", Login: "carol", Type: db.CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "alice", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "carol"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// exec runs statements behind every policy.
func (in passwordsOf) exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
}

// count is one number the database answers, behind every policy.
func (in passwordsOf) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %s", query, err)
	}
	return n
}

// policy writes the installation's policy, passwords and passkeys as given and the rest at the
// defaults.
func (in passwordsOf) policy(t *testing.T, passwords, passkeys string) {
	t.Helper()
	bound := false
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.SetInstallationPolicy(ctx, db.AuthPolicy{
			Password: passwords, Passkey: passkeys, UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2,
		}, *in.clock)
	}); err != nil {
		t.Fatal(err)
	}
}

// tighten writes what finance's policy tightens.
func (in passwordsOf) tighten(t *testing.T, p db.AuthPolicy) {
	t.Helper()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.SetNamespacePolicy(ctx, "finance", p, *in.clock)
	}); err != nil {
		t.Fatal(err)
	}
}

// login signs in with body, from the sign-in page, from the address from where it is not empty.
func (in passwordsOf) login(t *testing.T, body, from string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", in.origin)
	if from != "" {
		r.RemoteAddr = from
	}
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	return w
}

// as signs login in with their password, and a TOTP code where code is not empty.
func (in passwordsOf) as(t *testing.T, login, code string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"login":%q,"password":%q}`, login, thePasswords[login])
	if code != "" {
		body = fmt.Sprintf(`{"login":%q,"password":%q,"totp":%q}`, login, thePasswords[login], code)
	}
	return in.login(t, body, "")
}

// signedIn signs login in, failing the test unless the session opened is of kind, and answers it.
func (in passwordsOf) signedIn(t *testing.T, login, kind string) *http.Cookie {
	t.Helper()
	w := in.as(t, login, "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s signing in answered %d %s", login, w.Code, w.Body)
	}
	var answer api.SignedIn
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Login != login || answer.Session != kind {
		t.Fatalf("%s signing in answered %s, and the session is %s", login, w.Body, kind)
	}
	return session(t, w)
}

// me asks GET /api/v1/me with the session c carries, and answers the status.
func (in passwordsOf) me(t *testing.T, c *http.Cookie) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, request(t, "GET", "/api/v1/me", "", c))
	return w.Code, w.Body.String()
}

// failures are the signin.fail entries of the audit log, as actor target reason, in order.
func (in passwordsOf) failures(t *testing.T) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(),
		`select actor || ' ' || target || ' ' || (detail::jsonb->>'reason') from audit_log where action = 'signin.fail' order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		all = append(all, s)
	}
	return all
}

// A password signs in where passkeys are optional: a full session opened by the password, which reads
// what the user's grants allow; the password's last use, the sign-in and the user's personal
// namespace recorded; and signin.succeed, saying what the session may do.
func TestAPasswordOpensAFullSessionWhereThePolicyIsMet(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")

	w := in.as(t, "alice", "")
	if w.Code != http.StatusOK || w.Body.String() != "{\"login\":\"alice\",\"session\":\"full\"}\n" {
		t.Fatalf("alice signing in answered %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the sign-in is answered with Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	c := session(t, w)
	if code, body := in.me(t, c); code != http.StatusOK || !strings.Contains(body, `"finance"`) {
		t.Errorf("GET /api/v1/me with the session answered %d %s", code, body)
	}
	if as, err := in.p.Identify(request(t, "GET", "/api/v1/me", "", c)); err != nil || as.Principal != "alice" || as.Enrolling || as.OpenedByCode {
		t.Errorf("the session identified %+v: %v", as, err)
	}
	if n := in.count(t, `select count(*) from sessions where login = 'alice' and credential = 'alice-password' and enrolment_code is null`); n != 1 {
		t.Errorf("%d sessions of alice were opened by her password", n)
	}
	if n := in.count(t, `select count(*) from credentials where id = 'alice-password' and last_used_at = $1`, *in.clock); n != 1 {
		t.Error("the password's last use is not the sign-in")
	}
	if n := in.count(t, `select count(*) from users where login = 'alice' and last_sign_in_at = $1`, *in.clock); n != 1 {
		t.Error("the sign-in is not recorded on the user")
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'alice' and kind = 'personal' and owner = 'alice'`); n != 1 {
		t.Error("alice's first sign-in gave her no personal namespace")
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.succeed' and actor = 'alice' and target = 'alice'
	                       and detail::jsonb->>'credential' = 'alice-password' and detail::jsonb->>'session' = 'full'`); n != 1 {
		t.Errorf("%d sign-ins of alice by her password, with a full session, are recorded", n)
	}
}

// Where a passkey is required, the defaults, a password opens a session that enrols passkeys and
// nothing else until its account holds min_passkeys the policy accepts: it reads nothing and mints
// no token, as a session an enrolment link opened, and is told apart from one by not being opened by
// a code. A first passkey registered from it leaves it confined, one of the two min_passkeys asks
// for; the second takes the password, recorded as credential.remove, and the session it opened goes
// with it, since the account signs in with its passkeys from then on. An account holding one passkey
// of two signs in to a session that only enrols, and so does one holding one of one, while a passkey
// is required; where passkeys are optional, to a full one.
func TestAPasswordOpensASessionThatOnlyEnrolsUntilMinPasskeysAreHeld(t *testing.T) {
	in := somePasswords(t)
	c := in.signedIn(t, "alice", api.SessionEnrolment)

	if as, err := in.p.Identify(request(t, "GET", "/api/v1/me", "", c)); err != nil || as.Principal != "alice" || !as.Enrolling || as.OpenedByCode {
		t.Errorf("the session identified %+v: %v", as, err)
	}
	if code, body := in.me(t, c); code != http.StatusForbidden || !strings.Contains(body, "this session enrols passkeys and nothing else") {
		t.Errorf("GET /api/v1/me with a session that only enrols answered %d %s", code, body)
	}
	r := request(t, "POST", "/api/v1/auth/tokens", publicOrigin, c)
	r.Body = http.NoBody
	w := httptest.NewRecorder()
	if in.h.ServeHTTP(w, r); w.Code != http.StatusForbidden {
		t.Errorf("minting a token with a session that only enrols answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.succeed' and detail::jsonb->>'session' = 'enrolment'`); n != 1 {
		t.Errorf("%d sign-ins to a session that only enrols are recorded", n)
	}

	// The registration ceremony, from that session, with no code, on two devices.
	ceremonies := ceremonies{pool: in.pool, super: in.super, clock: in.clock, h: in.h}
	for i, label := range []string{"phone", "laptop"} {
		made, _, err := newBrowser().Create(ceremonies.options(t, `{"ceremony":"registration"}`, c))
		if err != nil {
			t.Fatal(err)
		}
		if w := ceremonies.verify(t, "registration", made, label, "", c); w.Code != http.StatusOK {
			t.Fatalf("registering a passkey from the session answered %d %s", w.Code, w.Body)
		}
		code, body := in.me(t, c)
		if i == 0 && code != http.StatusForbidden {
			t.Errorf("once alice holds one passkey of two, GET /api/v1/me with the same session answered %d %s", code, body)
		}
		if i == 1 && code != http.StatusUnauthorized {
			t.Errorf("once alice holds two passkeys of two, the session her password opened answered %d %s", code, body)
		}
	}
	if n := in.count(t, `select count(*) from credentials where login = 'alice' and type = 'password'`); n != 0 {
		t.Errorf("alice still holds her password beside two passkeys")
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.remove' and actor = 'alice' and target = 'alice-password'
	                       and detail::jsonb->>'reason' like 'the account holds min_passkeys%'`); n != 1 {
		t.Errorf("%d removals of alice's password are recorded", n)
	}

	in.signedIn(t, "carol", api.SessionEnrolment)
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "required", UserVerification: "required", MinPasskeys: 1})
	in.signedIn(t, "carol", api.SessionEnrolment)
	in.policy(t, "allowed", "optional")
	in.signedIn(t, "carol", api.SessionFull)
}

// What a session a password opened may do is read at every request, so a policy changed applies at
// the next one: a passkey required confines it, a namespace alice holds a grant in requiring one
// does too, and passwords forbidden, by the installation or by that namespace, end it. A session a
// passkey opened is not touched.
func TestAPolicyChangedAppliesToAPasswordsSessionAtTheNextRequest(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c := in.signedIn(t, "alice", api.SessionFull)
	passkeys, err := in.opening(t, "carol", api.OpenedBy{Credential: "carol-passkey"})
	if err != nil {
		t.Fatal(err)
	}

	in.policy(t, "allowed", "required")
	if code, _ := in.me(t, c); code != http.StatusForbidden {
		t.Errorf("with a passkey required, alice's session read her record: %d", code)
	}
	in.policy(t, "allowed", "optional")
	if code, _ := in.me(t, c); code != http.StatusOK {
		t.Errorf("with passkeys optional again, alice's session answered %d", code)
	}
	in.tighten(t, db.AuthPolicy{Passkey: "required"})
	if code, _ := in.me(t, c); code != http.StatusForbidden {
		t.Errorf("with finance requiring a passkey, alice's session read her record: %d", code)
	}
	in.tighten(t, db.AuthPolicy{Password: "forbidden"})
	if code, body := in.me(t, c); code != http.StatusUnauthorized || !strings.Contains(body, "that session opens nothing") {
		t.Errorf("with finance forbidding passwords, alice's session answered %d %s", code, body)
	}
	in.tighten(t, db.AuthPolicy{})
	in.policy(t, "forbidden", "required")
	if code, _ := in.me(t, c); code != http.StatusUnauthorized {
		t.Errorf("with passwords forbidden, alice's session answered %d", code)
	}
	if code, body := in.me(t, passkeys); code != http.StatusOK {
		t.Errorf("with passwords forbidden, carol's passkey's session answered %d %s", code, body)
	}
}

// hold gives login back the password whose hash is held, where a test took it and needs it again.
func (in passwordsOf) hold(t *testing.T, login, held string) {
	t.Helper()
	in.exec(t, `insert into credentials (id, login, type, password_hash) values ('`+login+`-password', '`+login+`', 'password', '`+held+`')
	            on conflict (id) do update set password_hash = excluded.password_hash`)
}

// opening opens a session of login as the sign-in routes do, at the test's clock.
func (in passwordsOf) opening(t *testing.T, login string, by api.OpenedBy) (*http.Cookie, error) {
	t.Helper()
	var c *http.Cookie
	err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		var err error
		c, err = api.OpenSession(ctx, w, login, by, *in.clock)
		return err
	})
	return c, err
}

// On an installation addressed by an IP address, where a browser runs no passkey ceremony, the
// policy is applied with passwords allowed and no passkey required, whatever the stored policy says:
// a password opens a full session there.
func TestAnInstallationAddressedByAnIPAddressTakesPasswords(t *testing.T) {
	in := passwordsAt(t, "https://192.0.2.10:8443", false)
	in.policy(t, "forbidden", "required")
	in.tighten(t, db.AuthPolicy{Password: "forbidden"})
	c := in.signedIn(t, "alice", api.SessionFull)
	if code, body := in.me(t, c); code != http.StatusOK {
		t.Errorf("alice's session on an installation addressed by an IP address answered %d %s", code, body)
	}
}

// Passwords forbidden are refused outright, 403 naming the setting and never a wrong password's 401,
// for a right password, a wrong one and a login nobody holds alike, and before any password is
// compared: with every turn to hash held, the refusal comes at once rather than after the wait. A
// namespace forbidding them refuses those holding a grant in it. Each refusal is recorded.
func TestPasswordsForbiddenAreRefusedBeforeAnyIsCompared(t *testing.T) {
	in := somePasswords(t)
	hold := api.Hashing(in.passwords, 1, 50*time.Millisecond)
	in.policy(t, "forbidden", "required")
	done := hold()
	for _, body := range []string{
		`{"login":"alice","password":"correct horse battery staple"}`,
		`{"login":"alice","password":"wrong"}`,
		`{"login":"nobody-at-all","password":"wrong"}`,
	} {
		w := in.login(t, body, "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) || !strings.Contains(w.Body.String(), "passwords are forbidden") {
			t.Errorf("%s with passwords forbidden answered %d %s", body, w.Code, w.Body)
		}
		if w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s with passwords forbidden set a cookie", body)
		}
	}
	done()

	in.policy(t, "allowed", "optional")
	in.tighten(t, db.AuthPolicy{Password: "forbidden"})
	if w := in.as(t, "alice", ""); w.Code != http.StatusForbidden {
		t.Errorf("alice, holding a grant in finance which forbids passwords, answered %d %s", w.Code, w.Body)
	}
	if w := in.as(t, "carol", ""); w.Code != http.StatusOK {
		t.Errorf("carol, holding no grant in finance, answered %d %s", w.Code, w.Body)
	}
	got := in.failures(t)
	if len(got) != 4 || !strings.HasPrefix(got[2], "192.0.2.1 nobody-at-all passwords are forbidden") {
		t.Errorf("the refusals are recorded as %q", got)
	}
}

// Every other refusal is one 401 with one sentence, whatever the reason, so that no login can be
// learnt by asking; each is recorded with its reason, by the address it came from.
func TestEveryOtherRefusalIsOneSentence(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	code := totp.Code(bobsSecret, totp.StepAt(*in.clock))
	var said string
	for _, c := range []struct {
		body, reason string
	}{
		{`{"login":"alice","password":"correct horse battery stapler"}`, "the password does not match"},
		{`{"login":"nobody-at-all","password":"correct horse battery staple"}`, "no user holds that login"},
		{`{"login":"erin","password":"anything"}`, "the account holds no password"},
		{`{"login":"dave","password":"dave's own"}`, "the account is suspended"},
		{`{"login":"bob","password":"bob's own"}`, "the account holds a TOTP generator, and no code was sent"},
		{`{"login":"bob","password":"bob's own","totp":"000000"}`, "the TOTP code does not match"},
		{`{"login":"bob","password":"wrong","totp":"` + code + `"}`, "the password does not match"},
		{`{"login":"alice","password":"correct horse battery staple","totp":"123456"}`, "a TOTP code was sent, and the account holds no TOTP generator"},
	} {
		w := in.login(t, c.body, "198.51.100.4:5000")
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s answered %d %s", c.body, w.Code, w.Body)
		}
		if said == "" {
			said = w.Body.String()
		}
		if w.Body.String() != said {
			t.Errorf("%s answered %s, and another refusal %s", c.body, w.Body, said)
		}
	}
	got := in.failures(t)
	if len(got) != 8 {
		t.Fatalf("the refusals are recorded as %q", got)
	}
	for i, want := range []string{
		"198.51.100.4 alice the password does not match", "198.51.100.4 nobody-at-all no user holds that login",
		"198.51.100.4 erin the account holds no password", "198.51.100.4 dave the account is suspended",
		"198.51.100.4 bob the account holds a TOTP generator", "198.51.100.4 bob the TOTP code does not match",
		"198.51.100.4 bob the password does not match", "198.51.100.4 alice a TOTP code was sent",
	} {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("refusal %d is recorded as %q", i, got[i])
		}
	}
	if n := in.count(t, `select count(*) from sessions`); n != 0 {
		t.Errorf("the refusals opened %d sessions", n)
	}
	if n := in.count(t, `select count(*) from credentials where totp_step is not null`); n != 0 {
		t.Error("a TOTP code refused with a wrong password spent its step")
	}
}

// A request that is not the sign-in page's, a body the schema refuses, and a service account, which
// never signs in, are refused before anything is counted or read.
func TestWhatTheSchemaRefusesIsRefusedFirst(t *testing.T) {
	in := somePasswords(t)
	for name, body := range map[string]string{
		"a service account":      `{"login":"finance/agentiik","password":"x"}`,
		"a group":                `{"login":"group:team","password":"x"}`,
		"a reserved login":       `{"login":"operator","password":"x"}`,
		"no password":            `{"login":"alice","password":""}`,
		"a code of five":         `{"login":"bob","password":"bob's own","totp":"12345"}`,
		"a code of letters":      `{"login":"bob","password":"bob's own","totp":"12345a"}`,
		"agk login's member":     `{"login":"alice","password":"x","terminal":{}}`,
		"a member written twice": `{"login":"alice","login":"bob","password":"x"}`,
	} {
		if w := in.login(t, body, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d %s", name, w.Code, w.Body)
		}
	}
	for _, origin := range []string{"", "null", "https://evil.example.com"} {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/login", strings.NewReader(`{"login":"alice","password":"correct horse battery staple"}`))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		if in.h.ServeHTTP(w, r); w.Code != http.StatusForbidden || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("a sign-in from the origin %q answered %d %s", origin, w.Code, w.Body)
		}
	}
	if got := in.failures(t); len(got) != 0 {
		t.Errorf("requests refused before any sign-in were recorded as %q", got)
	}
	// Past alice's count of ten, and she still signs in.
	in.policy(t, "allowed", "optional")
	for range 12 {
		in.login(t, `{"login":"alice","password":""}`, "")
	}
	if w := in.as(t, "alice", ""); w.Code != http.StatusOK {
		t.Errorf("after twelve refusals of the schema alice signing in answered %d %s", w.Code, w.Body)
	}
}

// Attempts are counted for each login from anywhere and for each address at any login, and one past
// either count is 429 with Retry-After, the right password included, until the first of those
// counted leaves the window; a sign-in starts its login's count again. Behind the proxy AGK_PROXY_URL
// names, the address is the last entry of X-Forwarded-For, the one the proxy wrote.
func TestAttemptsAreCountedForEachLoginAndEachAddress(t *testing.T) {
	in := passwordsAt(t, "https://agentiik.example.com", true)
	in.policy(t, "allowed", "optional")
	wrong := `{"login":"alice","password":"wrong"}`
	proxied := func(body, forwarded string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/login", strings.NewReader(body))
		r.Header.Set("Origin", publicOrigin)
		r.RemoteAddr = "127.0.0.1:40000"
		if forwarded != "" {
			r.Header.Set("X-Forwarded-For", forwarded)
		}
		w := httptest.NewRecorder()
		in.h.ServeHTTP(w, r)
		return w
	}

	// Nine wrong, from nine addresses, and one right: the count starts again.
	for i := range 9 {
		if w := proxied(wrong, fmt.Sprintf("203.0.113.%d", i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d answered %d %s", i, w.Code, w.Body)
		}
	}
	if w := proxied(`{"login":"alice","password":"correct horse battery staple"}`, "203.0.113.50"); w.Code != http.StatusOK {
		t.Fatalf("alice's right password after nine wrong answered %d %s", w.Code, w.Body)
	}
	// Ten wrong from ten addresses, a client's own X-Forwarded-For entries before the proxy's
	// changing nothing: the eleventh is refused whoever sends it, the right password among them.
	start := *in.clock
	for i := range 10 {
		*in.clock = start.Add(time.Duration(i) * time.Second)
		if w := proxied(wrong, fmt.Sprintf("192.0.2.200, 198.51.100.%d", i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d answered %d %s", i, w.Code, w.Body)
		}
	}
	// Retry-After rounds up, so that one more fits once the seconds it gives have passed.
	*in.clock = start.Add(time.Minute + 500*time.Millisecond)
	for _, body := range []string{wrong, `{"login":"alice","password":"correct horse battery staple"}`} {
		w := proxied(body, "198.51.100.99")
		wait, _ := strconv.Atoi(w.Header().Get("Retry-After"))
		if w.Code != http.StatusTooManyRequests || wait != 15*60-60 || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("an eleventh attempt for alice answered %d %s, Retry-After %q", w.Code, w.Body, w.Header().Get("Retry-After"))
		}
	}
	// The window moves: the first of the ten leaves it, and one more fits.
	*in.clock = start.Add(15 * time.Minute)
	if w := proxied(`{"login":"alice","password":"correct horse battery staple"}`, "198.51.100.99"); w.Code != http.StatusOK {
		t.Errorf("once the first attempt left the window, alice answered %d %s", w.Code, w.Body)
	}

	// Thirty from one address at logins nobody holds, and the thirty-first refused, for alice too.
	*in.clock = start.Add(time.Hour)
	for i := range 30 {
		if w := proxied(fmt.Sprintf(`{"login":"nobody-%d","password":"x"}`, i), "2001:db8:1:2::1"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d from one address answered %d %s", i, w.Code, w.Body)
		}
	}
	// Another address of the same /64 is the same machine.
	if w := proxied(`{"login":"alice","password":"correct horse battery staple"}`, "2001:db8:1:2::ffff"); w.Code != http.StatusTooManyRequests {
		t.Errorf("a thirty-first attempt from one /64 answered %d %s", w.Code, w.Body)
	}
	if w := proxied(`{"login":"alice","password":"correct horse battery staple"}`, "2001:db8:1:3::1"); w.Code != http.StatusOK {
		t.Errorf("an attempt from another /64 answered %d %s", w.Code, w.Body)
	}
	// The address recorded is the proxy's last entry.
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.fail' and actor = '198.51.100.3'`); n != 1 {
		t.Errorf("%d failures are recorded as from 198.51.100.3", n)
	}
	if n := in.count(t, `select count(*) from audit_log where actor in ('192.0.2.200', '127.0.0.1')`); n != 0 {
		t.Errorf("%d entries are recorded as from the client's own entry or the proxy", n)
	}
}

// An attempt refused before any password was compared is given back to its login: passwords
// forbidden, whose address keeps it, and a hash that waited too long for its turn, which is 503 and
// not a wrong password, and is given back to its address as well.
func TestAnAttemptThatComparedNothingIsGivenBack(t *testing.T) {
	in := somePasswords(t)
	held := in.hashHeld(t, "alice")
	in.policy(t, "forbidden", "optional")
	for range 12 {
		if w := in.as(t, "alice", ""); w.Code != http.StatusForbidden {
			t.Fatalf("with passwords forbidden alice answered %d %s", w.Code, w.Body)
		}
	}
	in.policy(t, "allowed", "optional")
	// The first refusal took the password it found forbidden, which is put back: what this test
	// counts is the attempts.
	in.hold(t, "alice", held)
	hold := api.Hashing(in.passwords, 1, 20*time.Millisecond)
	done := hold()
	for range 12 {
		if w := in.as(t, "alice", ""); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "too many passwords at once") {
			t.Fatalf("with every turn to hash held alice answered %d %s", w.Code, w.Body)
		}
	}
	done()
	if w := in.as(t, "alice", ""); w.Code != http.StatusOK {
		t.Errorf("after twenty-four attempts that compared nothing alice answered %d %s", w.Code, w.Body)
	}
}

// Hashing is bounded: with its one turn held, a sign-in waits for it, and signs in once it is given
// back within the wait; a login nobody holds, and an account holding no password, wait their turn as
// well, since each is hashed as any other.
func TestHashingWaitsItsTurn(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	hold := api.Hashing(in.passwords, 1, 5*time.Second)
	done := hold()
	answered := make(chan *httptest.ResponseRecorder, 3)
	go func() { answered <- in.as(t, "alice", "") }()
	go func() { answered <- in.login(t, `{"login":"nobody-at-all","password":"x"}`, "") }()
	go func() { answered <- in.login(t, `{"login":"erin","password":"x"}`, "") }()
	select {
	case w := <-answered:
		t.Fatalf("a sign-in was answered while the one turn to hash was held: %d %s", w.Code, w.Body)
	case <-time.After(300 * time.Millisecond):
	}
	done()
	var codes []int
	for range 3 {
		codes = append(codes, (<-answered).Code)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusOK, http.StatusUnauthorized, http.StatusUnauthorized}) {
		t.Errorf("once the turn was given back the two sign-ins answered %v", codes)
	}
}

// A TOTP code is accepted at its step and one either side of the API's clock, and never twice: the
// same code again, or one of an earlier step, is refused once a code was accepted, and the next
// step's is accepted. The step is recorded on the generator.
func TestATOTPCodeIsAcceptedOneStepEitherSideAndNeverTwice(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	start := *in.clock
	at := totp.StepAt(start)
	for _, far := range []int64{-2, 2} {
		if w := in.as(t, "bob", totp.Code(bobsSecret, at+far)); w.Code != http.StatusUnauthorized {
			t.Errorf("the code of step %+d answered %d %s", far, w.Code, w.Body)
		}
	}
	if w := in.as(t, "bob", totp.Code(bobsSecret, at-1)); w.Code != http.StatusOK {
		t.Fatalf("the code of the step before answered %d %s", w.Code, w.Body)
	}
	if w := in.as(t, "bob", totp.Code(bobsSecret, at-1)); w.Code != http.StatusUnauthorized {
		t.Errorf("the same code again answered %d %s", w.Code, w.Body)
	}
	if w := in.as(t, "bob", totp.Code(bobsSecret, at+1)); w.Code != http.StatusOK {
		t.Fatalf("the code of the step after answered %d %s", w.Code, w.Body)
	}
	for _, step := range []int64{at - 1, at, at + 1} {
		if w := in.as(t, "bob", totp.Code(bobsSecret, step)); w.Code != http.StatusUnauthorized {
			t.Errorf("the code of step %+d after that of step +1 answered %d %s", step-at, w.Code, w.Body)
		}
	}
	*in.clock = start.Add(2 * totp.Step)
	if w := in.as(t, "bob", totp.Code(bobsSecret, at+2)); w.Code != http.StatusOK {
		t.Errorf("a minute later, the code of the step then answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where id = 'bob-totp' and totp_step = $1 and last_used_at = $2`, at+2, *in.clock); n != 1 {
		t.Error("the step of the code accepted last is not recorded on bob's generator")
	}
	if got := in.failures(t); len(got) != 6 || !strings.HasSuffix(got[2], "the TOTP code does not match, or a code of its step or a later one was accepted already") {
		t.Errorf("the refusals are recorded as %q", got)
	}
}

// Two sign-ins with one code, checked at once, open one session: the second finds its step spent.
func TestOneTOTPCodeSignsInOnceWhenSentTwiceAtOnce(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	code := totp.Code(bobsSecret, totp.StepAt(*in.clock))
	answered := make(chan int, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { answered <- in.as(t, "bob", code).Code })
	}
	wg.Wait()
	close(answered)
	var codes []int
	for c := range answered {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusOK, http.StatusUnauthorized, http.StatusUnauthorized, http.StatusUnauthorized}) {
		t.Errorf("four sign-ins with one code answered %v", codes)
	}
	if n := in.count(t, `select count(*) from sessions where login = 'bob'`); n != 1 {
		t.Errorf("one code opened %d sessions", n)
	}
}

// A hash the API does not read, one asking for more memory than a turn to hash counts on, is the
// installation's trouble and a 500, never a wrong password.
func TestAHashAboveTheBaselineIsTheInstallationsTrouble(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	in.exec(t, `update credentials set password_hash = '$argon2id$v=19$m=1048576,t=2,p=1$c2l4dGVlbiBieXRlIHNsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' where id = 'alice-password'`)
	if w := in.as(t, "alice", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("a hash asking for a GiB answered %d %s", w.Code, w.Body)
	}
	if len(*in.trouble) != 1 || !strings.Contains((*in.trouble)[0].Error(), "not one this release reads") {
		t.Errorf("the trouble said is %v", *in.trouble)
	}
}

// A refusal by a policy forbidding passwords compares nothing, and gives its login's attempt back,
// but tells a login holding a grant where passwords are forbidden from one nobody holds, so it is
// counted where it is asked from: thirty from one address, and the thirty-first is refused as a
// guess past the count would be, while another address is answered, for the same login.
func TestAPolicyRefusalIsCountedWhereItIsAskedFrom(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "forbidden", "optional")
	for i := range 30 {
		if w := in.login(t, `{"login":"alice","password":"x"}`, "198.51.100.7:1000"); w.Code != http.StatusForbidden {
			t.Fatalf("question %d from one address answered %d %s", i, w.Code, w.Body)
		}
	}
	if w := in.login(t, `{"login":"alice","password":"x"}`, "198.51.100.7:1000"); w.Code != http.StatusTooManyRequests {
		t.Errorf("a thirty-first question from one address answered %d %s", w.Code, w.Body)
	}
	if w := in.login(t, `{"login":"alice","password":"x"}`, "198.51.100.8:1000"); w.Code != http.StatusForbidden {
		t.Errorf("a question from another address answered %d %s", w.Code, w.Body)
	}
}

// A suspended account is refused as soon as its password is checked, right or wrong, and never
// waits on its user's row, which a wrong password never reaches: with the row held by another
// transaction, the right password is answered at once.
func TestASuspendedAccountIsRefusedWithoutWaitingOnItsRow(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	conn := dbtest.Superuser(t, in.super)
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	if _, err := tx.Exec(t.Context(), `select from users where login = 'dave' for update`); err != nil {
		t.Fatal(err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- in.as(t, "dave", "") }()
	select {
	case w := <-answered:
		if w.Code != http.StatusUnauthorized {
			t.Errorf("dave's right password answered %d %s", w.Code, w.Body)
		}
	case <-time.After(3 * time.Second):
		t.Error("dave's right password waited on his row, which a wrong one never does")
		tx.Rollback(t.Context())
		<-answered
	}
}

// Where a passkey is required, a password's session enrols passkeys and nothing else whatever the
// account holds beside it: carol, holding a password beside two device-bound passkeys, is confined
// as alice, holding none, is, and so is a password a recovery code sets, which "authorises only
// enrolling a new passkey" there. Where passkeys are optional, each opens a full session.
func TestAPasswordOnlyEnrolsWhereAPasskeyIsRequiredWhateverTheAccountHolds(t *testing.T) {
	in := somePasswords(t)
	in.passkeyed(t, "carol", "carol-passkey-2", false)
	in.signedIn(t, "carol", api.SessionEnrolment)
	in.signedIn(t, "alice", api.SessionEnrolment)
	in.passkeyed(t, "erin", "erin-1", false)
	in.passkeyed(t, "erin", "erin-2", false)
	if w := in.enrolWith(t, in.enrolCode(t, "erin", db.EnrolmentRecovery), "erin's own passphrase"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"session":"enrolment"`) {
		t.Errorf("a password erin set from a recovery code beside two passkeys answered %d %s", w.Code, w.Body)
	}
	in.policy(t, "allowed", "optional")
	in.signedIn(t, "carol", api.SessionFull)
	in.signedIn(t, "alice", api.SessionFull)
}

// Where device_bound_only applies, a synced passkey counts for none of min_passkeys: alice, holding
// two synced passkeys and a password, keeps the password when she registers her first device-bound
// passkey from the session it opens, and loses it with the second.
func TestASyncedPasskeyCountsForNoneOfMinPasskeysWhereOnlyDeviceBoundOnesDo(t *testing.T) {
	in := somePasswords(t)
	bound := true
	in.tighten(t, db.AuthPolicy{DeviceBoundOnly: &bound})
	for _, id := range []string{"alice-synced-1", "alice-synced-2"} {
		if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.AddCredential(ctx, db.Credential{ID: id, Login: "alice", Type: db.CredentialPasskey,
				PublicKey: []byte{1}, AAGUID: make([]byte, 16), BackupEligible: true, BackupState: true})
		}); err != nil {
			t.Fatal(err)
		}
	}
	c := in.signedIn(t, "alice", api.SessionEnrolment)
	if w := in.registered(t, c); w.Code != http.StatusOK {
		t.Fatalf("alice's first device-bound passkey answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "alice") == "" {
		t.Error("alice's password went with one device-bound passkey beside two synced ones, and min_passkeys is 2")
	}
	if w := in.registered(t, c); w.Code != http.StatusOK {
		t.Fatalf("alice's second device-bound passkey answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "alice") != "" {
		t.Error("alice's password outlived her second device-bound passkey")
	}
}

// What was checked is checked again under the user's row before anybody is signed in: passwords
// forbidden while a sign-in was checked are its 403, a password changed or a TOTP generator
// enrolled meanwhile its 401, and each is recorded as having happened during the sign-in.
func TestWhatChangesDuringASignInIsCheckedAgain(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	held := in.hashHeld(t, "alice")
	for _, c := range []struct {
		name   string
		change func()
		status int
		reason string
	}{
		{"passwords forbidden", func() { in.policy(t, "forbidden", "optional") }, http.StatusForbidden,
			"passwords were forbidden by the policy that applies to the account during the sign-in"},
		{"the password changed", func() {
			hash, err := password.Hash(thePasswords["alice"])
			if err != nil {
				t.Fatal(err)
			}
			in.exec(t, `update credentials set password_hash = '`+hash+`' where id = 'alice-password'`)
		}, http.StatusUnauthorized, "the password was changed or removed during the sign-in"},
		{"a generator enrolled", func() {
			sealed, err := in.totp.SealTOTP("alice", "alice-totp", bobsSecret)
			if err != nil {
				t.Fatal(err)
			}
			if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
				return w.AddCredential(ctx, db.Credential{ID: "alice-totp", Login: "alice", Type: db.CredentialTOTP, TOTPSealed: sealed})
			}); err != nil {
				t.Fatal(err)
			}
		}, http.StatusUnauthorized, "the TOTP generator was enrolled or removed during the sign-in"},
	} {
		api.BetweenChecksAndSignIn(in.passwords, c.change)
		w := in.as(t, "alice", "")
		api.BetweenChecksAndSignIn(in.passwords, nil)
		if w.Code != c.status || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s during alice's sign-in answered %d %s", c.name, w.Code, w.Body)
		}
		if c.status == http.StatusForbidden && !strings.Contains(w.Body.String(), `"setting":"password"`) {
			t.Errorf("%s during alice's sign-in answered %s", c.name, w.Body)
		}
		if got := in.failures(t); len(got) == 0 || !strings.HasSuffix(got[len(got)-1], c.reason) {
			t.Errorf("%s during alice's sign-in is recorded as %q", c.name, got)
		}
		if c.status == http.StatusForbidden && in.hashHeld(t, "alice") != "" {
			t.Errorf("%s during alice's sign-in left her the password it found forbidden", c.name)
		}
		in.policy(t, "allowed", "optional")
		in.exec(t, `delete from credentials where id = 'alice-totp'`)
		in.hold(t, "alice", held)
	}
	if n := in.count(t, `select count(*) from sessions where login = 'alice'`); n != 0 {
		t.Errorf("%d sessions of alice were opened", n)
	}
}
