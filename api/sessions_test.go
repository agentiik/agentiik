package api_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// The console's session, against a real PostgreSQL: an opaque identifier in a __Host- cookie, kept
// as its SHA-256, which lives twelve hours idle and thirty days at most, is revoked at once, is
// confined to enrolling where an enrolment code opened it, changes something only from the public
// URL's origin, and is never presented beside a bearer token.

// publicOrigin is the origin of the public URL the sessions of these tests are accepted on,
// https://Agentiik.Example.com:443/console/, as a browser writes it.
const publicOrigin = "https://agentiik.example.com"

// sessions is the installation of principals, its Principals accepting sessions on a clock the test
// moves. alice and carol hold a passkey each and alice a password as well; dave, suspended, holds
// nothing.
type sessions struct {
	principals
	clock *time.Time
}

func someSessions(t *testing.T) sessions {
	t.Helper()
	in := sessions{principals: somePrincipals(t)}
	clock := in.now
	in.clock = &clock
	p, err := api.NewPrincipals(in.pool, func() time.Time { return *in.clock })
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AcceptSessions("https://Agentiik.Example.com:443/console/"); err != nil {
		t.Fatal(err)
	}
	in.p = p
	err = in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, c := range []db.Credential{
			{ID: "alice-passkey", Login: "alice", Type: db.CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)},
			{ID: "alice-password", Login: "alice", Type: db.CredentialPassword, PasswordHash: "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"},
			{ID: "carol-passkey", Login: "carol", Type: db.CredentialPasskey, PublicKey: []byte{2}, AAGUID: make([]byte, 16)},
		} {
			if err := w.AddCredential(ctx, c); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// open opens a session of login as the sign-in routes do, at the test's clock.
func (in sessions) open(t *testing.T, login string, by api.OpenedBy) *http.Cookie {
	t.Helper()
	c, err := in.opening(t, login, by)
	if err != nil {
		t.Fatalf("a session of %s could not be opened: %s", login, err)
	}
	return c
}

func (in sessions) opening(t *testing.T, login string, by api.OpenedBy) (*http.Cookie, error) {
	t.Helper()
	var c *http.Cookie
	err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		var err error
		c, err = api.OpenSession(ctx, w, login, by, *in.clock)
		return err
	})
	return c, err
}

// recovery issues a recovery code for login and answers the hash it is kept as.
func (in sessions) recovery(t *testing.T, login, value string) []byte {
	t.Helper()
	code := db.EnrolmentCode{Hash: hashOf(value), Login: login, Kind: db.EnrolmentRecovery, IssuedBy: "carol",
		IssuedAt: *in.clock, ExpiresAt: in.clock.Add(time.Hour)}
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.IssueEnrolmentCode(ctx, code)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return code.Hash
}

// request is a request to path by method, carrying the cookies given and the Origin header where
// origin is not empty.
func request(t *testing.T, method, path, origin string, cookies ...*http.Cookie) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// asked is who a request by method, from origin, carrying the cookie is, as Identify says.
func (in sessions) asked(t *testing.T, method, origin string, c *http.Cookie) api.Identity {
	t.Helper()
	as, err := in.p.Identify(request(t, method, "/api/v1/runs", origin, c))
	if err != nil {
		t.Fatalf("identifying failed: %s", err)
	}
	return as
}

// idleUntil is the idle expiry the sessions table holds for the session c carries.
func (in sessions) idleUntil(t *testing.T, c *http.Cookie) time.Time {
	t.Helper()
	var until time.Time
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(),
		`select idle_expires_at from sessions where hash = $1`, hashOf(c.Value)).Scan(&until); err != nil {
		t.Fatal(err)
	}
	return until
}

// The cookie is what the OpenAPI document's session scheme names, set as the page says: opaque, 256
// bits, HttpOnly, Secure, SameSite=Lax, for the whole origin and bound to no Domain, as the __Host-
// prefix requires, and carrying no expiry of its own. The table keeps its SHA-256 and what opened
// it, and never the value.
func TestASessionIsAnOpaqueHostCookieKeptAsItsHash(t *testing.T) {
	in := someSessions(t)
	c := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})

	if c.Name != "__Host-agentiik_session" || c.Path != "/" || c.Domain != "" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("the session is set as %+v", c)
	}
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Errorf("the session cookie carries an expiry of its own, %d seconds or %s, and the server ends it", c.MaxAge, c.Expires)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err != nil || len(raw) != 32 || len(c.Value) != 43 {
		t.Errorf("the session's value is %q, and it is 256 bits in 43 base64url characters", c.Value)
	}
	set := c.String()
	if !regexp.MustCompile(`^__Host-agentiik_session=[A-Za-z0-9_-]+;`).MatchString(set) {
		t.Errorf("Set-Cookie reads %q, outside the OpenAPI document's pattern", set)
	}
	for _, attribute := range []string{"; Path=/", "; HttpOnly", "; Secure", "; SameSite=Lax"} {
		if !strings.Contains(set, attribute) {
			t.Errorf("Set-Cookie reads %q, without %s", set, attribute)
		}
	}
	for _, attribute := range []string{"Domain=", "Expires=", "Max-Age="} {
		if strings.Contains(set, attribute) {
			t.Errorf("Set-Cookie reads %q, with %s", set, attribute)
		}
	}

	var credential string
	var created, idle time.Time
	var code []byte
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(),
		`select credential, enrolment_code, created_at, idle_expires_at from sessions where hash = $1 and login = 'alice'`,
		hashOf(c.Value)).Scan(&credential, &code, &created, &idle); err != nil {
		t.Fatalf("no session is kept under the SHA-256 of the cookie's value: %s", err)
	}
	if credential != "alice-passkey" || code != nil || !created.Equal(in.now) || !idle.Equal(in.now.Add(12*time.Hour)) {
		t.Errorf("the session is kept as opened by %q and %x at %s, idle at %s", credential, code, created, idle)
	}

	if as := in.asked(t, "GET", "", c); as.Principal != "alice" || as.Enrolling || as.Refused != "" {
		t.Errorf("the session identified %+v", as)
	}
	if again := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"}); again.Value == c.Value {
		t.Error("two sessions were given one value")
	}
}

// A session ends twelve hours after its last request, each request keeping it open for as long
// again, and thirty days after it was opened however busy it is kept.
func TestASessionEndsIdleAndAtItsLifetime(t *testing.T) {
	in := someSessions(t)
	opened := in.now
	c := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})

	*in.clock = opened.Add(11 * time.Hour)
	if as := in.asked(t, "GET", "", c); as.Principal != "alice" {
		t.Fatalf("a session asked of eleven hours after it opened identified %+v", as)
	}
	if until := in.idleUntil(t, c); !until.Equal(opened.Add(23 * time.Hour)) {
		t.Errorf("a request kept the session open until %s, and twelve hours from it is %s", until, opened.Add(23*time.Hour))
	}
	// Within a minute of where it is, the idle expiry is left as it is rather than written again.
	*in.clock = opened.Add(11*time.Hour + 30*time.Second)
	if as := in.asked(t, "GET", "", c); as.Principal != "alice" {
		t.Fatalf("the session identified %+v", as)
	}
	if until := in.idleUntil(t, c); !until.Equal(opened.Add(23 * time.Hour)) {
		t.Errorf("a request thirty seconds after the last wrote the idle expiry again, as %s", until)
	}
	*in.clock = opened.Add(23*time.Hour - time.Second)
	if as := in.asked(t, "GET", "", c); as.Principal != "alice" {
		t.Errorf("a session asked a second before its idle expiry identified %+v", as)
	}
	*in.clock = in.clock.Add(12 * time.Hour)
	if as := in.asked(t, "GET", "", c); as.Principal != "" || as.Refused == "" {
		t.Errorf("a session left twelve hours idle identified %+v", as)
	}

	// Asked every eleven hours, one opened now is never idle and still ends at thirty days.
	opened = in.clock.Add(time.Hour)
	*in.clock = opened
	busy := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	ends := opened.Add(30 * 24 * time.Hour)
	for at := opened.Add(11 * time.Hour); at.Before(ends); at = at.Add(11 * time.Hour) {
		*in.clock = at
		if as := in.asked(t, "GET", "", busy); as.Principal != "alice" {
			t.Fatalf("a busy session asked %s after it opened identified %+v", at.Sub(opened), as)
		}
		if until := in.idleUntil(t, busy); until.After(ends) {
			t.Fatalf("a request %s after the session opened kept it open until %s, past the end of its thirty days", at.Sub(opened), until)
		}
	}
	*in.clock = ends.Add(-time.Second)
	if as := in.asked(t, "GET", "", busy); as.Principal != "alice" {
		t.Errorf("a busy session asked a second before its thirty days identified %+v", as)
	}
	*in.clock = ends
	if as := in.asked(t, "GET", "", busy); as.Principal != "" || as.Refused == "" {
		t.Errorf("a busy session asked at the end of its thirty days identified %+v", as)
	}
}

// A session revoked, or whose credential was removed, opens nothing from the next request, with the
// one sentence a token that opens nothing is refused with, and so does one of a suspended user while
// the suspension lasts. The others are left as they are.
func TestASessionRevokedOpensNothingFromTheNextRequest(t *testing.T) {
	in := someSessions(t)
	revoked := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	byPassword := in.open(t, "alice", api.OpenedBy{Credential: "alice-password"})
	carols := in.open(t, "carol", api.OpenedBy{Credential: "carol-passkey"})
	for _, c := range []*http.Cookie{revoked, byPassword, carols} {
		if as := in.asked(t, "GET", "", c); as.Principal == "" {
			t.Fatalf("a session just opened identified %+v", as)
		}
	}

	err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.RevokeSessions(ctx, "alice", hashOf(revoked.Value), *in.clock)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	as := in.asked(t, "GET", "", revoked)
	if as.Principal != "" || as.RefusedAs != 0 || !strings.Contains(as.Refused, "that session opens nothing") {
		t.Errorf("a revoked session identified %+v", as)
	}
	if as := in.asked(t, "GET", "", byPassword); as.Principal != "alice" {
		t.Errorf("revoking one session ended another of the same user: %+v", as)
	}

	// "Removing the credential ends the sessions it opened."
	err = in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.RemoveCredential(ctx, "alice", "alice-password")
	})
	if err != nil {
		t.Fatal(err)
	}
	if again := in.asked(t, "GET", "", byPassword); again.Principal != "" || again.Refused != as.Refused {
		t.Errorf("a session whose credential was removed identified %+v", again)
	}

	// "Its sessions and tokens open nothing while the suspension lasts."
	suspend := func(suspended bool) {
		t.Helper()
		if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.UpdateUser(ctx, db.User{Login: "carol", DisplayName: "Carol", Admin: true, Suspended: suspended})
		}); err != nil {
			t.Fatal(err)
		}
	}
	suspend(true)
	if again := in.asked(t, "GET", "", carols); again.Principal != "" || again.Refused != as.Refused {
		t.Errorf("a suspended user's session identified %+v", again)
	}
	suspend(false)
	if again := in.asked(t, "GET", "", carols); again.Principal != "carol" {
		t.Errorf("a session whose user's suspension was lifted identified %+v", again)
	}
}

// A session an enrolment code opened is known as one, and the router refuses it on every route it
// authorises by who asks, with the 403 the OpenAPI document names rather than the 404 a namespaced
// route answers anything else with; the same user's full session reaches them. A suspended user's
// code still opens one, since enrolling is how such an account comes back, and their credential
// opens none.
func TestASessionAnEnrolmentCodeOpenedEnrolsAndReachesNothingElse(t *testing.T) {
	in := someSessions(t)
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	served := func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
		w.WriteHeader(http.StatusOK)
	}
	rt.MustHandle("GET", "/api/v1/runners", api.Needs{Permission: api.GrantManage, Scope: api.Installation}, served)
	rt.MustHandle("POST", "/api/v1/{namespace}/workflows/{workflow}/runs", api.Needs{Permission: api.WorkflowRun, Scope: api.Workflow}, served)
	rt.MustHandle("GET", "/api/v1/namespaces", api.OnNamespace{}, served)
	rt.MustHandleAcross("GET", "/api/v1/runs", api.Across{Permission: api.RunRead},
		func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target, _ api.Holds) {
			w.WriteHeader(http.StatusOK)
		})

	carolEnrolling := in.open(t, "carol", api.OpenedBy{EnrolmentCode: in.recovery(t, "carol", "carol-recovery")})
	aliceEnrolling := in.open(t, "alice", api.OpenedBy{EnrolmentCode: in.recovery(t, "alice", "alice-recovery")})
	if as := in.asked(t, "GET", "", aliceEnrolling); as.Principal != "alice" || !as.Enrolling {
		t.Errorf("a session a recovery code opened identified %+v", as)
	}
	for _, c := range []struct {
		method, path string
		full         *http.Cookie
		enrolling    *http.Cookie
	}{
		{"GET", "/api/v1/runners", in.open(t, "carol", api.OpenedBy{Credential: "carol-passkey"}), carolEnrolling},
		{"POST", "/api/v1/finance/workflows/payroll/runs", in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"}), aliceEnrolling},
		{"GET", "/api/v1/namespaces", in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"}), aliceEnrolling},
		{"GET", "/api/v1/runs", in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"}), aliceEnrolling},
	} {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, request(t, c.method, c.path, publicOrigin, c.full))
		if w.Code != http.StatusOK {
			t.Errorf("%s %s answered a full session %d: %s", c.method, c.path, w.Code, w.Body)
		}
		w = httptest.NewRecorder()
		rt.ServeHTTP(w, request(t, c.method, c.path, publicOrigin, c.enrolling))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "this session enrols passkeys and nothing else") {
			t.Errorf("%s %s answered a session that may only enrol %d: %s", c.method, c.path, w.Code, w.Body)
		}
		if w.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s %s asked a session that may only enrol for another credential", c.method, c.path)
		}
	}

	// dave is suspended: his recovery code opens a session that may only enrol, and his passkey
	// none.
	daves := in.open(t, "dave", api.OpenedBy{EnrolmentCode: in.recovery(t, "dave", "dave-recovery")})
	if as := in.asked(t, "GET", "", daves); as.Principal != "dave" || !as.Enrolling {
		t.Errorf("a suspended user's recovery session identified %+v", as)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, request(t, "GET", "/api/v1/namespaces", "", daves))
	if w.Code != http.StatusForbidden {
		t.Errorf("a suspended user's recovery session was answered %d on a listing", w.Code)
	}
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.AddCredential(ctx, db.Credential{ID: "dave-passkey", Login: "dave", Type: db.CredentialPasskey, PublicKey: []byte{3}, AAGUID: make([]byte, 16)})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.opening(t, "dave", api.OpenedBy{Credential: "dave-passkey"}); !errors.Is(err, db.ErrSessionRefused) {
		t.Errorf("a suspended user's passkey opened a session, answered %v", err)
	}
}

// A session is opened by a credential of its own user or by an enrolment code, one of the two, and
// a code opens one session at most.
func TestASessionIsOpenedByOneThingOfItsOwnUser(t *testing.T) {
	in := someSessions(t)
	code := in.recovery(t, "alice", "alice-recovery")
	// Neither, or both, is refused before the database is asked, saying so.
	for what, by := range map[string]api.OpenedBy{
		"nothing": {}, "a credential and a code": {Credential: "alice-passkey", EnrolmentCode: code},
	} {
		if c, err := in.opening(t, "alice", by); c != nil || err == nil || !strings.Contains(err.Error(), "one of the two") {
			t.Errorf("a session opened by %s was answered %v, %v", what, c, err)
		}
	}
	for what, c := range map[string]struct {
		login string
		by    api.OpenedBy
		want  error
	}{
		"another user's credential":    {"alice", api.OpenedBy{Credential: "carol-passkey"}, db.ErrNoCredential},
		"another user's code":          {"carol", api.OpenedBy{EnrolmentCode: code}, db.ErrSessionRefused},
		"a code that was never issued": {"alice", api.OpenedBy{EnrolmentCode: hashOf("never")}, db.ErrSessionRefused},
	} {
		if cookie, err := in.opening(t, c.login, c.by); cookie != nil || !errors.Is(err, c.want) {
			t.Errorf("a session opened by %s was answered %v, %v", what, cookie, err)
		}
	}
	in.open(t, "alice", api.OpenedBy{EnrolmentCode: code})
	if _, err := in.opening(t, "alice", api.OpenedBy{EnrolmentCode: code}); !errors.Is(err, db.ErrSessionRefused) {
		t.Errorf("a code opened a second session, answered %v", err)
	}
}

// A request changing something that a session carries comes from the public URL's origin, as the
// browser names it in the Origin header, or is refused with 403 before the session is looked up, so
// that it keeps the session open no longer either. A request that reads is not asked, nor is one a
// bearer token carries, which no browser sends of its own accord.
func TestASessionChangesSomethingOnlyFromThePublicOrigin(t *testing.T) {
	in := someSessions(t)
	c := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	*in.clock = in.clock.Add(time.Hour)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, origin := range []string{"", "null", "https://evil.example.com", "https://agentiik.example.com:8443",
			"http://agentiik.example.com", "https://agentiik.example.com.evil.example", "https://AGENTIIK.example.com"} {
			as := in.asked(t, method, origin, c)
			if as.Principal != "" || as.RefusedAs != http.StatusForbidden || !strings.Contains(as.Refused, "public URL") {
				t.Errorf("%s from %q identified %+v", method, origin, as)
			}
		}
		if as := in.asked(t, method, publicOrigin, c); as.Principal != "alice" {
			t.Errorf("%s from the public origin identified %+v", method, as)
		}
	}
	if until := in.idleUntil(t, c); !until.Equal(in.clock.Add(12 * time.Hour)) {
		t.Fatalf("the session is idle at %s", until)
	}
	*in.clock = in.clock.Add(time.Hour)
	if as := in.asked(t, "POST", "https://evil.example.com", c); as.Principal != "" {
		t.Fatalf("a request of another origin identified %+v", as)
	}
	if until := in.idleUntil(t, c); !until.Equal(in.clock.Add(11 * time.Hour)) {
		t.Errorf("a request of another origin kept the session open until %s", until)
	}

	// Two Origin headers are not one of them.
	r := request(t, "POST", "/api/v1/runs", publicOrigin, c)
	r.Header.Add("Origin", "https://evil.example.com")
	if as, err := in.p.Identify(r); err != nil || as.Principal != "" || as.RefusedAs != http.StatusForbidden {
		t.Errorf("a request with two Origin headers identified %+v: %v", as, err)
	}
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		if as := in.asked(t, method, "https://evil.example.com", c); as.Principal != "alice" {
			t.Errorf("%s from another origin identified %+v", method, as)
		}
	}
	r = request(t, "POST", "/api/v1/runs", "")
	r.Header.Set("Authorization", "Bearer "+in.token(t, "alice", nil, nil, in.clock.Add(time.Hour)))
	if as, err := in.p.Identify(r); err != nil || as.Principal != "alice" {
		t.Errorf("a bearer token on a POST with no Origin identified %+v: %v", as, err)
	}

	// The router answers the refusal as the 403 it is, asking for no other credential.
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	rt.MustHandle("POST", "/api/v1/{namespace}/workflows/{workflow}/runs", api.Needs{Permission: api.WorkflowRun, Scope: api.Workflow},
		func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
			w.WriteHeader(http.StatusCreated)
		})
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, request(t, "POST", "/api/v1/finance/workflows/payroll/runs", "https://evil.example.com", c))
	if w.Code != http.StatusForbidden || w.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("a run started from another origin was answered %d, %q: %s", w.Code, w.Header().Get("WWW-Authenticate"), w.Body)
	}
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, request(t, "POST", "/api/v1/finance/workflows/payroll/runs", publicOrigin, c))
	if w.Code != http.StatusCreated {
		t.Errorf("a run started from the public origin was answered %d: %s", w.Code, w.Body)
	}
}

// The public URL's origin is what a browser writes: the host in lower case, the port where it is not
// 443, and no path. A URL no browser page has is refused.
func TestSessionsAreAcceptedOnThePublicURLsOrigin(t *testing.T) {
	in := someSessions(t)
	c := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	for publicURL, origin := range map[string]string{
		"https://agentiik.example.com":             "https://agentiik.example.com",
		"https://Agentiik.EXAMPLE.com/":            "https://agentiik.example.com",
		"https://agentiik.example.com:443/console": "https://agentiik.example.com",
		"https://agentiik.example.com:8443":        "https://agentiik.example.com:8443",
		"https://agentiik.example.com:08443":       "https://agentiik.example.com:8443",
		"https://[::1]:8443":                       "https://[::1]:8443",
		"https://127.0.0.1":                        "https://127.0.0.1",
	} {
		p, err := api.NewPrincipals(in.pool, func() time.Time { return *in.clock })
		if err != nil {
			t.Fatal(err)
		}
		if err := p.AcceptSessions(publicURL); err != nil {
			t.Errorf("%s was refused: %s", publicURL, err)
			continue
		}
		if as, err := p.Identify(request(t, "POST", "/api/v1/runs", origin, c)); err != nil || as.Principal != "alice" {
			t.Errorf("on %s, a request from %s identified %+v: %v", publicURL, origin, as, err)
		}
	}
	for _, publicURL := range []string{"", "http://agentiik.example.com", "agentiik.example.com", "https://", "https://agentiik.example.com:99999", "https://:8443"} {
		p, err := api.NewPrincipals(in.pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.AcceptSessions(publicURL); err == nil {
			t.Errorf("sessions were accepted on %q", publicURL)
		}
	}
}

// A request carrying a bearer token and a session, or two sessions, is refused with 400 rather than
// answered as either, since the page names no order between them. An empty cookie is no session.
func TestARequestCarryingTwoCredentialsIsRefused(t *testing.T) {
	in := someSessions(t)
	c := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	carols := in.open(t, "carol", api.OpenedBy{Credential: "carol-passkey"})
	token := in.token(t, "alice", nil, nil, in.clock.Add(time.Hour))

	both := request(t, "GET", "/api/v1/runs", "", c)
	both.Header.Set("Authorization", "Bearer "+token)
	two := request(t, "GET", "/api/v1/runs", "", c, carols)
	for what, r := range map[string]*http.Request{"a token and a session": both, "two sessions": two} {
		as, err := in.p.Identify(r)
		if err != nil || as.Principal != "" || as.RefusedAs != http.StatusBadRequest || !strings.Contains(as.Refused, "more than one credential") {
			t.Errorf("a request carrying %s identified %+v: %v", what, as, err)
		}
	}

	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	rt.MustHandle("GET", "/api/v1/runners", api.Needs{Permission: api.GrantManage, Scope: api.Installation},
		func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
			t.Error("a request carrying two credentials was served")
		})
	w := httptest.NewRecorder()
	r := request(t, "GET", "/api/v1/runners", "", carols)
	r.Header.Set("Authorization", "Bearer "+token)
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a request carrying two credentials was answered %d: %s", w.Code, w.Body)
	}

	empty := request(t, "GET", "/api/v1/runs", "", &http.Cookie{Name: api.SessionCookie, Value: ""})
	empty.Header.Set("Authorization", "Bearer "+token)
	if as, err := in.p.Identify(empty); err != nil || as.Principal != "alice" {
		t.Errorf("a bearer token beside an empty session cookie identified %+v: %v", as, err)
	}
}

// Where sessions are not accepted, a session cookie is not read: a request carrying one carries no
// credential, and a bearer token beside it is the credential it carries.
func TestASessionIsNotReadWhereSessionsAreNotAccepted(t *testing.T) {
	in := someSessions(t)
	c := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	p, err := api.NewPrincipals(in.pool, func() time.Time { return *in.clock })
	if err != nil {
		t.Fatal(err)
	}
	if as, err := p.Identify(request(t, "GET", "/api/v1/runs", "", c)); err != nil || as.Principal != "" || as.Refused != "" {
		t.Errorf("a session was read where sessions are not accepted: %+v, %v", as, err)
	}
	r := request(t, "GET", "/api/v1/runs", "", c)
	r.Header.Set("Authorization", "Bearer "+in.token(t, "carol", nil, nil, in.clock.Add(time.Hour)))
	if as, err := p.Identify(r); err != nil || as.Principal != "carol" {
		t.Errorf("a bearer token beside a session not read identified %+v: %v", as, err)
	}
}

// The router answers an identification refused with a status of its own with that status and its
// sentence, asking for no other credential, and refuses a session that may only enrol on every
// route it authorises by who asks, whoever identified it.
func TestTheRouterAnswersARefusalWithTheStatusItNames(t *testing.T) {
	identify := func(r *http.Request) (api.Identity, error) {
		switch r.Header.Get("X-As") {
		case "enrolling":
			return api.Identity{Principal: "alice", Enrolling: true}, nil
		case "two":
			return api.Identity{Refused: "two credentials", RefusedAs: http.StatusBadRequest}, nil
		}
		return api.Identity{Principal: "alice"}, nil
	}
	rt, err := api.NewRouter(holder{who: "alice", what: api.GrantManage}, identify)
	if err != nil {
		t.Fatal(err)
	}
	rt.MustHandle("GET", "/api/v1/runners", api.Needs{Permission: api.GrantManage, Scope: api.Installation},
		func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) {
			w.WriteHeader(http.StatusOK)
		})
	for as, want := range map[string]struct {
		code int
		body string
	}{
		"":          {http.StatusOK, ""},
		"enrolling": {http.StatusForbidden, "this session enrols passkeys and nothing else"},
		"two":       {http.StatusBadRequest, "two credentials"},
	} {
		r := httptest.NewRequest("GET", "/api/v1/runners", nil)
		r.Header.Set("X-As", as)
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != want.code || !strings.Contains(w.Body.String(), want.body) || (want.code != http.StatusOK && w.Header().Get("WWW-Authenticate") != "") {
			t.Errorf("%q was answered %d, %q: %s", as, w.Code, w.Header().Get("WWW-Authenticate"), w.Body)
		}
	}
}
