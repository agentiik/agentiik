package api_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/password"
	"github.com/agentiik/agentiik/internal/totp"
	"github.com/agentiik/agentiik/internal/ulid"
)

// Setting a password, over a real PostgreSQL: from an enrolment code, which it spends once and which
// opens the session a password opens, ending the bootstrap where the first administrator's session
// is a full one; from a session, a full one or one that may only enrol, the current password asked
// for where one is held and counted as a guess; removed where min_passkeys allows it; and every one
// of them refused where passwords are forbidden, naming the setting, to a bearer token, and from
// another origin.

// call sends one request as the sign-in page does, with fetch, from the public URL's origin,
// carrying the cookies given.
func (in passwordsOf) call(t *testing.T, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Origin", in.origin)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	return w
}

// enrolCode issues login an enrolment code of kind, as an administrator would, and answers it.
func (in passwordsOf) enrolCode(t *testing.T, login, kind string) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	value := "agkenrol_" + base64.RawURLEncoding.EncodeToString(raw)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.IssueEnrolmentCode(ctx, db.EnrolmentCode{
			Hash: hashOf(value), Login: login, Kind: kind, IssuedBy: "carol", IssuedAt: *in.clock, ExpiresAt: in.clock.Add(time.Hour),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// enrolWith sets a password from an enrolment code.
func (in passwordsOf) enrolWith(t *testing.T, code, secret string) *httptest.ResponseRecorder {
	t.Helper()
	return in.call(t, "POST", "/api/v1/auth/password/enrol", fmt.Sprintf(`{"code":%q,"password":%q}`, code, secret))
}

// passkeyed opens a session of login by a passkey of theirs, which it enrols first where they hold
// none by that identifier.
func (in passwordsOf) passkeyed(t *testing.T, login, id string, synced bool) *http.Cookie {
	t.Helper()
	if n := in.count(t, `select count(*) from credentials where id = $1`, id); n == 0 {
		if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.AddCredential(ctx, db.Credential{
				ID: id, Login: login, Type: db.CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16), BackupEligible: synced,
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := in.opening(t, login, api.OpenedBy{Credential: id})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// setPolicy writes the installation's policy whole.
func (in passwordsOf) setPolicy(t *testing.T, p db.AuthPolicy) {
	t.Helper()
	if p.DeviceBoundOnly == nil {
		bound := false
		p.DeviceBoundOnly = &bound
	}
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.SetInstallationPolicy(ctx, p, *in.clock)
	}); err != nil {
		t.Fatal(err)
	}
}

// trail is the audit log's entries, as actor action target, in order.
func (in passwordsOf) trail(t *testing.T) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(), `select actor || ' ' || action || ' ' || target from audit_log order by seq`)
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

// hashHeld is the password hash login holds, empty where they hold none.
func (in passwordsOf) hashHeld(t *testing.T, login string) string {
	t.Helper()
	var hash string
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(), `select password_hash from credentials where login = $1 and type = 'password'`, login)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
	}
	return hash
}

// matches says whether login's password is secret.
func (in passwordsOf) matches(t *testing.T, login, secret string) bool {
	t.Helper()
	hash := in.hashHeld(t, login)
	if hash == "" {
		return false
	}
	ok, err := password.Verify(hash, secret)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A new user's link sets their password where passkeys are optional: the Argon2id hash of what was
// sent kept, the code spent, and the full session a password opens, with their personal namespace;
// credential.enrol, enrolment.use and signin.succeed recorded, the sign-in last. The code sets
// nothing a second time, and the password signs in.
func TestAPasswordSetFromAnEnrolmentLinkSignsItsUserIn(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	code := in.enrolCode(t, "erin", db.EnrolmentNewUser)

	w := in.enrolWith(t, code, "erin's own passphrase")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("setting erin's password answered %d %s", w.Code, w.Body)
	}
	var answer api.PasswordEnrolled
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if c := answer.Credential; answer.Login != "erin" || answer.Session != api.SessionFull ||
		c.Type != db.CredentialPassword || c.ID == "" || !c.CreatedAt.Equal(*in.clock) || !c.LastUsedAt.IsZero() {
		t.Errorf("setting erin's password answered %s", w.Body)
	}
	if code, body := in.me(t, session(t, w)); code != http.StatusOK {
		t.Errorf("the session setting it opened answered %d %s", code, body)
	}
	if !in.matches(t, "erin", "erin's own passphrase") || !strings.HasPrefix(in.hashHeld(t, "erin"), "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("erin's password is kept as %q", in.hashHeld(t, "erin"))
	}
	if n := in.count(t, `select count(*) from enrolment_codes where login = 'erin' and used_at = $1`, *in.clock); n != 1 {
		t.Error("the code was not spent")
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'erin' and kind = 'personal'`); n != 1 {
		t.Error("erin's first sign-in gave her no personal namespace")
	}
	got := in.trail(t)
	if len(got) < 5 || !slices.Equal(got[len(got)-5:len(got)-3], []string{"erin credential.enrol " + answer.Credential.ID, "erin enrolment.use erin"}) ||
		got[len(got)-1] != "erin signin.succeed erin" {
		t.Errorf("the audit log reads %q", got)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'signin.succeed' and detail::jsonb->>'credential' = $1 and detail::jsonb->>'session' = 'full'`, answer.Credential.ID); n != 1 {
		t.Error("the sign-in is not recorded with the password and a full session")
	}

	if w := in.enrolWith(t, code, "another passphrase"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "that code opens nothing") {
		t.Errorf("the code spent answered %d %s", w.Code, w.Body)
	}
	if !in.matches(t, "erin", "erin's own passphrase") {
		t.Error("the spent code changed erin's password")
	}
	if w := in.login(t, `{"login":"erin","password":"erin's own passphrase"}`, ""); w.Code != http.StatusOK {
		t.Errorf("erin signing in with the password answered %d %s", w.Code, w.Body)
	}
}

// Where a passkey is required, the defaults, a password set from a link opens the session a password
// opens there, one that enrols passkeys and nothing else, and says so.
func TestAPasswordSetFromALinkWhereAPasskeyIsRequiredOpensASessionThatOnlyEnrols(t *testing.T) {
	in := somePasswords(t)
	w := in.enrolWith(t, in.enrolCode(t, "erin", db.EnrolmentNewUser), "erin's own passphrase")
	var answer api.PasswordEnrolled
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusOK || answer.Session != api.SessionEnrolment {
		t.Fatalf("setting erin's password answered %d %s", w.Code, w.Body)
	}
	if code, body := in.me(t, session(t, w)); code != http.StatusForbidden || !strings.Contains(body, "enrols passkeys and nothing else") {
		t.Errorf("the session answered %d %s", code, body)
	}
}

// An enrolment code sets a password once: of two requests with one code at once, one sets it and the
// other is refused as a code that opens nothing, and one session is opened.
func TestAnEnrolmentCodeSetsOnePasswordWhenSentTwiceAtOnce(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	code := in.enrolCode(t, "erin", db.EnrolmentNewUser)
	answered := make(chan int, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() { answered <- in.enrolWith(t, code, fmt.Sprintf("erin's passphrase %d", i)).Code })
	}
	wg.Wait()
	close(answered)
	var codes []int
	for c := range answered {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusOK, http.StatusUnauthorized}) {
		t.Errorf("two passwords set with one code answered %v", codes)
	}
	if n := in.count(t, `select count(*) from sessions where login = 'erin'`); n != 1 {
		t.Errorf("one code opened %d sessions", n)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'enrolment.use'`); n != 1 {
		t.Errorf("one code was used %d times", n)
	}
}

// A recovery code sets a password in place of the one held: the identifier kept, the hash replaced,
// the sessions the old one opened ended, and the TOTP generator beside it removed, since the person
// it is for lost what signs them in. A suspended user sets one and opens no session.
func TestARecoveryCodeReplacesThePasswordAndTheGeneratorBesideIt(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	old, err := in.opening(t, "bob", api.OpenedBy{Credential: "bob-password"})
	if err != nil {
		t.Fatal(err)
	}
	w := in.enrolWith(t, in.enrolCode(t, "bob", db.EnrolmentRecovery), "bob's new passphrase")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"bob-password"`) {
		t.Fatalf("bob's recovery answered %d %s", w.Code, w.Body)
	}
	if !in.matches(t, "bob", "bob's new passphrase") || in.matches(t, "bob", "bob's own") {
		t.Error("bob's password was not replaced")
	}
	if n := in.count(t, `select count(*) from credentials where login = 'bob' and type = 'totp'`); n != 0 {
		t.Error("bob's generator outlived the password it stood beside")
	}
	if code, _ := in.me(t, old); code != http.StatusUnauthorized {
		t.Errorf("the session bob's old password opened answered %d", code)
	}
	if code, body := in.me(t, session(t, w)); code != http.StatusOK {
		t.Errorf("the session the recovery opened answered %d %s", code, body)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.remove' and target = 'bob-totp' and detail::jsonb->>'type' = 'totp'`); n != 1 {
		t.Error("the generator's removal is not recorded")
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.enrol' and target = 'bob-password'
	                       and detail::jsonb->>'replaced' = 'true' and detail::jsonb->>'sessions_ended' = '1'`); n != 1 {
		t.Error("the password's replacement is not recorded with the session it ended")
	}

	w = in.enrolWith(t, in.enrolCode(t, "dave", db.EnrolmentRecovery), "dave's new passphrase")
	if w.Code != http.StatusOK || w.Body.String() == "" || strings.Contains(w.Body.String(), `"session"`) || w.Header().Get("Set-Cookie") != "" {
		t.Errorf("dave, suspended, setting a password answered %d %s", w.Code, w.Body)
	}
	if !in.matches(t, "dave", "dave's new passphrase") {
		t.Error("dave's password was not set")
	}
}

// A user suspended for holding no passkey where passwords were forbidden comes back by a password
// set from a new user's link or a recovery code once passwords are allowed again: the suspension
// lifted, recorded beside the password, and the user signed in. Where passwords are still forbidden
// nothing is set and the suspension stays; one the policy did not make stays whatever is set (dave,
// above).
func TestAPasswordSetFromACodeLiftsASuspensionForHoldingNoPasskey(t *testing.T) {
	in := somePasswords(t)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "frank", DisplayName: "Frank"})
	}); err != nil {
		t.Fatal(err)
	}
	in.exec(t, `update users set suspended = true, suspended_for = 'no_passkey' where login in ('erin', 'frank')`)
	for login, kind := range map[string]string{"erin": db.EnrolmentRecovery, "frank": db.EnrolmentNewUser} {
		code := in.enrolCode(t, login, kind)
		in.policy(t, "forbidden", "optional")
		if w := in.enrolWith(t, code, login+"'s new passphrase"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
			t.Errorf("%s's %s code where passwords are forbidden answered %d %s", login, kind, w.Code, w.Body)
		}
		if got := in.suspended(t); !slices.Contains(got, login+":no_passkey") {
			t.Errorf("%s's code refused lifted the suspension: %q", login, got)
		}

		in.policy(t, "allowed", "optional")
		w := in.enrolWith(t, code, login+"'s new passphrase")
		var answer api.PasswordEnrolled
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusOK || answer.Session != api.SessionFull {
			t.Fatalf("%s's %s code answered %d %s", login, kind, w.Code, w.Body)
		}
		if got := in.suspended(t); slices.Contains(got, login+":no_passkey") {
			t.Errorf("%s's password left the suspension: %q", login, got)
		}
		if code, body := in.me(t, session(t, w)); code != http.StatusOK {
			t.Errorf("the session %s's password opened answered %d %s", login, code, body)
		}
		if n := in.count(t, `select count(*) from audit_log where action = 'credential.enrol' and actor = $1 and detail::jsonb->>'suspension_lifted' = 'no_passkey'`, login); n != 1 {
			t.Errorf("the lifting of %s's suspension is not recorded", login)
		}
	}
}

// What the enrolment route refuses, and with what: a request from another page 403; a body the
// schema refuses, a code outside its grammar and a password shorter than twelve characters or longer
// than 1024 bytes 400; a code nobody issued 401; a password that is the login 422; and passwords
// forbidden 403 naming the setting. Nothing is spent by any of them. Twelve characters are enough,
// counted as characters and not as bytes, and 1024 bytes are not too many.
func TestWhatTheEnrolmentRouteRefuses(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "frank-martinez", DisplayName: "Frank"})
	}); err != nil {
		t.Fatal(err)
	}
	code := in.enrolCode(t, "erin", db.EnrolmentNewUser)
	frank := in.enrolCode(t, "frank-martinez", db.EnrolmentNewUser)
	for _, c := range []struct {
		name, body string
		status     int
		says       string
	}{
		{"eleven characters", fmt.Sprintf(`{"code":%q,"password":"elevenchars"}`, code), http.StatusBadRequest, "at least 12 characters, and this one is 11"},
		{"eleven characters of two bytes", fmt.Sprintf(`{"code":%q,"password":"ééééééééééé"}`, code), http.StatusBadRequest, "this one is 11"},
		{"1025 bytes", fmt.Sprintf(`{"code":%q,"password":%q}`, code, strings.Repeat("a", 1025)), http.StatusBadRequest, "at most 1024 bytes"},
		{"no password", fmt.Sprintf(`{"code":%q}`, code), http.StatusBadRequest, "at least 12 characters"},
		{"a code outside its grammar", `{"code":"agkenrol_short","password":"a long enough passphrase"}`, http.StatusBadRequest, "code:"},
		{"a member nobody reads", fmt.Sprintf(`{"code":%q,"password":"a long enough passphrase","login":"erin"}`, code), http.StatusBadRequest, "not a field"},
		{"a code nobody issued", `{"code":"agkenrol_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","password":"a long enough passphrase"}`, http.StatusUnauthorized, "that code opens nothing"},
		{"the login", fmt.Sprintf(`{"code":%q,"password":"Frank-Martinez"}`, frank), http.StatusUnprocessableEntity, "not the login"},
	} {
		w := in.call(t, "POST", "/api/v1/auth/password/enrol", c.body)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.says) || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s answered %d %s", c.name, w.Code, w.Body)
		}
	}
	for _, origin := range []string{"", "null", "https://evil.example.com"} {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/password/enrol", strings.NewReader(fmt.Sprintf(`{"code":%q,"password":"a long enough passphrase"}`, code)))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		if in.h.ServeHTTP(w, r); w.Code != http.StatusForbidden {
			t.Errorf("setting a password from the origin %q answered %d %s", origin, w.Code, w.Body)
		}
	}
	in.policy(t, "forbidden", "required")
	if w := in.enrolWith(t, code, "a long enough passphrase"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Errorf("a password set where passwords are forbidden answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from enrolment_codes where used_at is not null`); n != 0 {
		t.Errorf("the refusals spent %d codes", n)
	}
	if n := in.count(t, `select count(*) from credentials where login in ('erin', 'frank-martinez')`); n != 0 {
		t.Errorf("the refusals enrolled %d credentials", n)
	}

	in.policy(t, "allowed", "optional")
	for login, secret := range map[string]string{"erin": "éééééééééééé", "frank-martinez": strings.Repeat("a", 1024)} {
		c := code
		if login == "frank-martinez" {
			c = frank
		}
		if w := in.enrolWith(t, c, secret); w.Code != http.StatusOK || !in.matches(t, login, secret) {
			t.Errorf("a password of %d characters and %d bytes answered %d %s", len([]rune(secret)), len(secret), w.Code, w.Body)
		}
	}
}

// A namespace forbidding passwords forbids setting one to those holding a grant in it, and not to
// the others, before anything is hashed: with every turn to hash held, the refusal comes at once
// rather than after the wait. A generator started before passwords were forbidden is not confirmed
// after.
func TestANamespaceForbiddingPasswordsForbidsSettingOne(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	alice := in.passkeyed(t, "alice", "alice-passkey", false)
	_, secret := in.started(t, alice)
	in.tighten(t, db.AuthPolicy{Password: "forbidden"})
	if w := in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, totp.Code(secret, totp.StepAt(*in.clock))), alice); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Errorf("alice confirming a generator answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'alice' and type = 'totp'`); n != 0 {
		t.Error("a generator was enrolled where passwords are forbidden")
	}
	carol := in.passkeyed(t, "carol", "carol-passkey", false)
	body := `{"password":"a long enough passphrase","current_password":"%s"}`
	done := api.Hashing(in.passwords, 1, 50*time.Millisecond)()
	if w := in.call(t, "PUT", "/api/v1/me/password", fmt.Sprintf(body, thePasswords["alice"]), alice); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Errorf("alice, holding a grant in finance, answered %d %s", w.Code, w.Body)
	}
	done()
	if w := in.call(t, "POST", "/api/v1/me/totp", "", alice); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Errorf("alice starting a generator answered %d %s", w.Code, w.Body)
	}
	if w := in.call(t, "PUT", "/api/v1/me/password", fmt.Sprintf(body, thePasswords["carol"]), carol); w.Code != http.StatusOK {
		t.Errorf("carol, holding none, answered %d %s", w.Code, w.Body)
	}
}

// Where a passkey is required, a password is set neither from a session nor from a code for an
// account holding the min_passkeys passkeys the policy accepts, since it could only ever open a
// session that enrols, with nothing left to enrol: 409 naming passkey, nothing set and the code not
// spent. A synced passkey where device_bound_only refuses it is not counted, and where no passkey is
// required a password is set whatever the account holds.
func TestAPasswordThatCouldOnlyEnrolIsNotSet(t *testing.T) {
	in := somePasswords(t)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "frank", DisplayName: "Frank"})
	}); err != nil {
		t.Fatal(err)
	}
	in.passkeyed(t, "frank", "frank-synced", true)
	frank := in.passkeyed(t, "frank", "frank-bound", false)
	const body = `{"password":"frank's first passphrase"}`
	refused := func(w *httptest.ResponseRecorder, what string) {
		t.Helper()
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"passkey"`) || !strings.Contains(w.Body.String(), "only ever open a session that enrols") {
			t.Errorf("%s answered %d %s", what, w.Code, w.Body)
		}
		if in.hashHeld(t, "frank") != "" {
			t.Fatalf("%s set a password", what)
		}
	}

	refused(in.call(t, "PUT", "/api/v1/me/password", body, frank), "frank's session, holding two passkeys")
	code := in.enrolCode(t, "frank", db.EnrolmentRecovery)
	refused(in.enrolWith(t, code, "frank's first passphrase"), "frank's recovery code")
	if n := in.count(t, `select count(*) from enrolment_codes where login = 'frank' and used_at is null and revoked_at is null`); n != 1 {
		t.Error("the recovery code refused was spent")
	}

	bound := true
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "required", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2})
	// Counted again in the transaction that would set it: a passkey registered once the account
	// was read brings it to min_passkeys.
	for _, what := range []string{"frank's session", "frank's recovery code"} {
		api.BetweenChecksAndSignIn(in.passwords, func() {
			api.BetweenChecksAndSignIn(in.passwords, nil)
			in.passkeyed(t, "frank", "frank-meanwhile", false)
		})
		if what == "frank's session" {
			refused(in.call(t, "PUT", "/api/v1/me/password", body, frank), what+", a passkey registered meanwhile")
		} else {
			refused(in.enrolWith(t, code, "frank's first passphrase"), what+", a passkey registered meanwhile")
		}
		in.exec(t, `delete from credentials where id = 'frank-meanwhile'`)
	}
	if w := in.call(t, "PUT", "/api/v1/me/password", body, frank); w.Code != http.StatusOK || !in.matches(t, "frank", "frank's first passphrase") {
		t.Errorf("frank's session, holding one passkey the policy accepts, answered %d %s", w.Code, w.Body)
	}
	in.exec(t, `delete from credentials where login = 'frank' and type = 'password'`)
	in.policy(t, "allowed", "optional")
	if w := in.enrolWith(t, code, "frank's second passphrase"); w.Code != http.StatusOK || !in.matches(t, "frank", "frank's second passphrase") {
		t.Errorf("frank's recovery code where no passkey is required answered %d %s", w.Code, w.Body)
	}
}

// A signed-in user changes their password with the current one: none sent, or one where none is
// held, is 400; a wrong one is 403 and counted as a guess, the eleventh in a quarter of an hour being
// a 429 whatever it sends; the right one sets it, keeping the identifier and the generator beside it,
// and ends every other session the password opened while the one it was changed from goes on.
func TestAPasswordIsChangedWithTheCurrentOne(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c, err := in.opening(t, "bob", api.OpenedBy{Credential: "bob-password"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := in.opening(t, "bob", api.OpenedBy{Credential: "bob-password"})
	if err != nil {
		t.Fatal(err)
	}
	set := func(body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		return in.call(t, "PUT", "/api/v1/me/password", body, cookies...)
	}
	if w := set(`{"password":"bob's new passphrase"}`, c); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "takes the current one") {
		t.Errorf("no current password answered %d %s", w.Code, w.Body)
	}
	if w := set(`{"password":"bob"}`, c); w.Code != http.StatusBadRequest {
		t.Errorf("a password of three characters answered %d %s", w.Code, w.Body)
	}
	if w := set(`{"password":"bob's new passphrase","current_password":"not bob's"}`, c); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "does not match") {
		t.Errorf("a wrong current password answered %d %s", w.Code, w.Body)
	}
	w := set(`{"password":"bob's new passphrase","current_password":"bob's own"}`, c)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("the right current password answered %d %s", w.Code, w.Body)
	}
	var held api.PasswordHeld
	if err := json.Unmarshal(w.Body.Bytes(), &held); err != nil || held.Type != "password" || held.ID != "bob-password" || !held.CreatedAt.Equal(*in.clock) || !held.LastUsedAt.IsZero() {
		t.Errorf("the change answered %s", w.Body)
	}
	if !in.matches(t, "bob", "bob's new passphrase") {
		t.Error("bob's password is not the new one")
	}
	if n := in.count(t, `select count(*) from credentials where id = 'bob-totp'`); n != 1 {
		t.Error("bob's generator went with the old password")
	}
	if code, body := in.me(t, c); code != http.StatusOK {
		t.Errorf("the session the password was changed from answered %d %s", code, body)
	}
	if code, _ := in.me(t, other); code != http.StatusUnauthorized {
		t.Errorf("another session the old password opened answered %d", code)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.enrol' and actor = 'bob' and target = 'bob-password'
	                       and detail::jsonb->>'replaced' = 'true' and detail::jsonb->>'sessions_ended' = '1'`); n != 1 {
		t.Error("the change is not recorded")
	}

	// The right one starts the count again: ten wrong guesses, each refused as a guess, and the
	// eleventh attempt, right or not, is refused.
	for i := range 10 {
		if w := set(`{"password":"bob's third passphrase","current_password":"a guess"}`, c); w.Code != http.StatusForbidden {
			t.Errorf("wrong guess %d after the change answered %d %s", i+1, w.Code, w.Body)
		}
	}
	w = set(`{"password":"bob's third passphrase","current_password":"bob's new passphrase"}`, c)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("the eleventh attempt answered %d %s", w.Code, w.Body)
	}
	if !in.matches(t, "bob", "bob's new passphrase") {
		t.Error("a refused attempt changed bob's password")
	}
}

// A session a passkey opened sets a first password with no current one, and one sent there is 400,
// as a password that is the login is 422; a session that may only enrol, which a password opened,
// changes it with the current one; a session an enrolment code opened sets nothing.
func TestAPasswordIsSetFromAnySessionOfItsUser(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "frank-martinez", DisplayName: "Frank"})
	}); err != nil {
		t.Fatal(err)
	}
	frank := in.passkeyed(t, "frank-martinez", "frank-passkey", false)
	if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"FRANK-MARTINEZ"}`, frank); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "not the login") {
		t.Errorf("frank's login as his password answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "frank-martinez") != "" {
		t.Error("frank's login was set as his password")
	}
	erin := in.passkeyed(t, "erin", "erin-passkey", false)
	if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"erin's first passphrase","current_password":"x"}`, erin); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "holds no password") {
		t.Errorf("a current password where none is held answered %d %s", w.Code, w.Body)
	}
	w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"erin's first passphrase"}`, erin)
	if w.Code != http.StatusOK || !in.matches(t, "erin", "erin's first passphrase") {
		t.Fatalf("erin's first password answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.enrol' and actor = 'erin' and detail::jsonb->>'replaced' = 'false'`); n != 1 {
		t.Error("the first password is not recorded")
	}

	in.policy(t, "allowed", "required")
	alice := in.signedIn(t, "alice", api.SessionEnrolment)
	if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"alice's new passphrase","current_password":"correct horse battery staple"}`, alice); w.Code != http.StatusOK {
		t.Errorf("alice's session that may only enrol answered %d %s", w.Code, w.Body)
	}
	if !in.matches(t, "alice", "alice's new passphrase") {
		t.Error("alice's password is not the new one")
	}

	coded, err := in.opening(t, "carol", api.OpenedBy{EnrolmentCode: hashOf(in.enrolCode(t, "carol", db.EnrolmentRecovery))})
	if err != nil {
		t.Fatal(err)
	}
	if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"carol's new passphrase","current_password":"carol's own"}`, coded); w.Code != http.StatusForbidden {
		t.Errorf("a session a code opened answered %d %s", w.Code, w.Body)
	}
}

// Every route setting or removing a password or a generator refuses a bearer token, the user's own
// included, and the bootstrap token, which is nobody's account; and a request with no credential; a
// request from another origin carrying a session is refused before the session is read.
func TestPasswordsAreSetFromASessionAlone(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	token := "agktoken_" + ulid.New()
	bootstrap := "agk_op_" + ulid.New()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if _, err := w.SetBootstrapToken(ctx, hashOf(bootstrap)); err != nil {
			return err
		}
		return w.MintToken(ctx, db.APIToken{ID: ulid.New(), Hash: hashOf(token), Principal: "alice", CreatedAt: *in.clock, ExpiresAt: in.clock.Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	c := in.signedIn(t, "alice", api.SessionFull)
	for _, route := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/me/password", `{"password":"a long enough passphrase","current_password":"correct horse battery staple"}`},
		{"DELETE", "/api/v1/me/password", ""},
		{"POST", "/api/v1/me/totp", ""},
		{"POST", "/api/v1/me/totp/confirm", `{"totp":"123456"}`},
		{"DELETE", "/api/v1/me/totp", `{"totp":"123456"}`},
	} {
		for who, bearer := range map[string]string{"alice's token": token, "the bootstrap token": bootstrap} {
			if w := sent(t, in.h, route.method, route.path, bearer, route.body); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "from a browser's session") {
				t.Errorf("%s %s with %s answered %d %s", route.method, route.path, who, w.Code, w.Body)
			}
		}
		if w := in.call(t, route.method, route.path, route.body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential answered %d %s", route.method, route.path, w.Code, w.Body)
		}
		r := request(t, route.method, route.path, "https://agentiik.example.com.evil.example", c)
		w := httptest.NewRecorder()
		if in.h.ServeHTTP(w, r); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Origin") {
			t.Errorf("%s %s from another origin answered %d %s", route.method, route.path, w.Code, w.Body)
		}
	}
	if !in.matches(t, "alice", thePasswords["alice"]) {
		t.Error("a refused request changed alice's password")
	}
	if n := in.count(t, `select count(*) from totp_enrolments`); n != 0 {
		t.Error("a refused request started a generator")
	}
}

// The password is removed where the account holds as many passkeys the policy accepts as
// min_passkeys says, a synced one counting for none where device_bound_only applies, and never where
// that would leave no credential: a 409 naming min_passkeys otherwise. Its generator goes with it,
// and so do the sessions it opened, the one it is removed from among them; each removal is recorded.
// An account holding no password is a 404, and a session that may only enrol reaches none of it.
func TestThePasswordIsRemovedBesideMinPasskeys(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	remove := func(c *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		return in.call(t, "DELETE", "/api/v1/me/password", "", c)
	}

	carol := in.passkeyed(t, "carol", "carol-passkey", false)
	if w := remove(carol); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"min_passkeys"`) || !strings.Contains(w.Body.String(), "with 1 passkey the policy accepts, and min_passkeys is 2") {
		t.Errorf("carol, holding one passkey of two, answered %d %s", w.Code, w.Body)
	}
	bob, err := in.opening(t, "bob", api.OpenedBy{Credential: "bob-password"})
	if err != nil {
		t.Fatal(err)
	}
	if w := remove(bob); w.Code != http.StatusConflict {
		t.Errorf("bob, holding no passkey, answered %d %s", w.Code, w.Body)
	}
	in.passkeyed(t, "bob", "bob-synced-1", true)
	in.passkeyed(t, "bob", "bob-synced-2", true)
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: new(true), MinPasskeys: 2})
	if w := remove(bob); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "with 0 passkeys the policy accepts") {
		t.Errorf("bob, holding two synced passkeys where only device-bound ones count, answered %d %s", w.Code, w.Body)
	}
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", MinPasskeys: 2})
	if w := remove(bob); w.Code != http.StatusNoContent {
		t.Fatalf("bob, holding two passkeys, answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'bob' and type in ('password', 'totp')`); n != 0 {
		t.Errorf("bob still holds %d of his password and generator", n)
	}
	if code, _ := in.me(t, bob); code != http.StatusUnauthorized {
		t.Errorf("the session bob's password opened, which removed it, answered %d", code)
	}
	if got := in.trail(t); !slices.Equal(got[len(got)-2:], []string{"bob credential.remove bob-password", "bob credential.remove bob-totp"}) {
		t.Errorf("the audit log reads %q", got)
	}

	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", MinPasskeys: 1})
	if w := remove(carol); w.Code != http.StatusNoContent {
		t.Errorf("carol, holding one passkey of one, answered %d %s", w.Code, w.Body)
	}
	if w := remove(carol); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "holds no password") {
		t.Errorf("carol, holding no password any more, answered %d %s", w.Code, w.Body)
	}
	erin := in.passkeyed(t, "erin", "erin-passkey", false)
	if w := remove(erin); w.Code != http.StatusNotFound {
		t.Errorf("erin, who never held a password, answered %d %s", w.Code, w.Body)
	}

	in.policy(t, "allowed", "required")
	alice := in.signedIn(t, "alice", api.SessionEnrolment)
	if w := remove(alice); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "enrols passkeys and nothing else") {
		t.Errorf("alice's session that may only enrol answered %d %s", w.Code, w.Body)
	}
}

// A namespace asking for more passkeys than the installation holds those with a grant in it to its
// number, and nobody else.
func TestANamespaceMinPasskeysHoldsThoseWithAGrantInIt(t *testing.T) {
	in := somePasswords(t)
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", MinPasskeys: 1})
	in.tighten(t, db.AuthPolicy{MinPasskeys: 3})
	in.passkeyed(t, "alice", "alice-passkey-1", false)
	alice := in.passkeyed(t, "alice", "alice-passkey-2", false)
	if w := in.call(t, "DELETE", "/api/v1/me/password", "", alice); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "with 2 passkeys the policy accepts, and min_passkeys is 3") {
		t.Errorf("alice, holding a grant in finance, answered %d %s", w.Code, w.Body)
	}
	carol := in.passkeyed(t, "carol", "carol-passkey", false)
	if w := in.call(t, "DELETE", "/api/v1/me/password", "", carol); w.Code != http.StatusNoContent {
		t.Errorf("carol, holding none, answered %d %s", w.Code, w.Body)
	}
}

// On an installation addressed by an IP address, where no passkey signs anybody in, the password is
// never removed, whatever passkeys the account holds; and it is set there whatever the stored policy
// says of passwords.
func TestThePasswordIsKeptWhereNoPasskeySignsIn(t *testing.T) {
	in := passwordsAt(t, "https://192.0.2.10", false)
	in.policy(t, "forbidden", "required")
	c := in.signedIn(t, "carol", api.SessionFull)
	in.passkeyed(t, "carol", "carol-passkey-2", false)
	if w := in.call(t, "DELETE", "/api/v1/me/password", "", c); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "addressed by an IP address") {
		t.Errorf("removing carol's password answered %d %s", w.Code, w.Body)
	}
	if w := in.enrolWith(t, in.enrolCode(t, "erin", db.EnrolmentNewUser), "erin's own passphrase"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"session":"full"`) {
		t.Errorf("erin's password from a link answered %d %s", w.Code, w.Body)
	}
}
