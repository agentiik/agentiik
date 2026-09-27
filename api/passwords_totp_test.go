package api_test

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/totp"
)

// A TOTP generator enrolled beside the password, and removed, over a real PostgreSQL; and the
// bootstrap token ended by a first administrator's password where the policy is met, and by their
// first passkey where it requires one.

// started starts a generator from the session c carries, failing the test unless it is answered, and
// answers what was and the secret it names.
func (in passwordsOf) started(t *testing.T, c *http.Cookie) (api.TOTPStarted, []byte) {
	t.Helper()
	w := in.call(t, "POST", "/api/v1/me/totp", "", c)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("starting a generator answered %d %s", w.Code, w.Body)
	}
	var s api.TOTPStarted
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s.Secret)
	if err != nil {
		t.Fatalf("the secret %q is not base32: %s", s.Secret, err)
	}
	return s, secret
}

// A generator started is not the account's: it shows its secret once, as text and in an otpauth://
// URI naming the account at the installation's host, waits ten minutes, and asks no sign-in for a
// code. A code it shows confirms it, a wrong one does not; confirmed, it is enrolled under the
// identifier it was started with, the confirming code's step spent, and every sign-in with the
// password asks for a code; a second is refused while it is held.
func TestATOTPGeneratorCountsOnlyOnceConfirmed(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c := in.signedIn(t, "alice", api.SessionFull)
	started, secret := in.started(t, c)

	if len(secret) != 20 || len(started.Secret) != 32 || started.ID == "" || !started.ExpiresAt.Equal(in.clock.Add(10*time.Minute)) {
		t.Errorf("the generator started is %+v, with a secret of %d bytes", started, len(secret))
	}
	u, err := url.Parse(started.URI)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Agentiik:alice@agentiik.example.com" ||
		q.Get("secret") != started.Secret || q.Get("issuer") != "Agentiik" || q.Get("algorithm") != "SHA1" ||
		q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Errorf("the URI is %s", started.URI)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'alice' and type = 'totp'`); n != 0 {
		t.Error("a generator started is enrolled before it is confirmed")
	}
	if w := in.as(t, "alice", ""); w.Code != http.StatusOK {
		t.Errorf("a generator started and not confirmed asked alice's sign-in for a code: %d %s", w.Code, w.Body)
	}

	now := totp.StepAt(*in.clock)
	confirm := func(code string) *httptest.ResponseRecorder {
		t.Helper()
		return in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, code), c)
	}
	if w := confirm(totp.Code(secret, now+2)); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "not the one the generator shows now") {
		t.Errorf("a code two steps ahead answered %d %s", w.Code, w.Body)
	}
	if w := confirm(totp.Code(bobsSecret, now)); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("another generator's code answered %d %s", w.Code, w.Body)
	}
	if w := confirm("12345"); w.Code != http.StatusBadRequest {
		t.Errorf("a code of five digits answered %d %s", w.Code, w.Body)
	}
	w := confirm(totp.Code(secret, now))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"type":"totp"`) || !strings.Contains(w.Body.String(), `"id":"`+started.ID+`"`) {
		t.Fatalf("the code it shows answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where id = $1 and login = 'alice' and type = 'totp' and totp_step = $2`, started.ID, now); n != 1 {
		t.Error("the generator is not enrolled with the step of the code that confirmed it")
	}
	if n := in.count(t, `select count(*) from totp_enrolments`); n != 0 {
		t.Error("the generator confirmed is still waiting")
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.enrol' and actor = 'alice' and target = $1 and detail::jsonb->>'type' = 'totp'`, started.ID); n != 1 {
		t.Error("the generator's enrolment is not recorded")
	}
	if w := in.as(t, "alice", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("alice's sign-in with no code answered %d %s", w.Code, w.Body)
	}
	if w := in.as(t, "alice", totp.Code(secret, now)); w.Code != http.StatusUnauthorized {
		t.Errorf("the code that confirmed the generator signed alice in: %d %s", w.Code, w.Body)
	}
	*in.clock = in.clock.Add(totp.Step)
	if w := in.as(t, "alice", totp.Code(secret, now+1)); w.Code != http.StatusOK {
		t.Errorf("the next code answered %d %s", w.Code, w.Body)
	}
	if w := in.call(t, "POST", "/api/v1/me/totp", "", c); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "holds a TOTP generator already") {
		t.Errorf("a second generator answered %d %s", w.Code, w.Body)
	}
}

// Nothing waiting is nothing to confirm: none started, one started again since, whose first secret
// confirms nothing, or one past its ten minutes. A generator needs a password beside it, and a
// session that may only enrol starts none.
func TestATOTPGeneratorWaitsTenMinutesBesideAPassword(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c := in.signedIn(t, "alice", api.SessionFull)
	confirm := func(code string) *httptest.ResponseRecorder {
		t.Helper()
		return in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, code), c)
	}
	if w := confirm("123456"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no TOTP generator is waiting") {
		t.Errorf("confirming nothing answered %d %s", w.Code, w.Body)
	}
	_, first := in.started(t, c)
	second, again := in.started(t, c)
	now := totp.StepAt(*in.clock)
	if w := confirm(totp.Code(first, now)); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("the first secret, replaced, answered %d %s", w.Code, w.Body)
	}
	*in.clock = in.clock.Add(10 * time.Minute)
	if w := confirm(totp.Code(again, totp.StepAt(*in.clock))); w.Code != http.StatusConflict {
		t.Errorf("a generator past its ten minutes answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where id = $1`, second.ID); n != 0 {
		t.Error("a generator past its minutes was enrolled")
	}

	erin := in.passkeyed(t, "erin", "erin-passkey", false)
	if w := in.call(t, "POST", "/api/v1/me/totp", "", erin); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "set a password first") {
		t.Errorf("erin, holding no password, answered %d %s", w.Code, w.Body)
	}
	// carol holds one passkey of the two a passkey required asks for.
	in.policy(t, "allowed", "required")
	enrolling := in.signedIn(t, "carol", api.SessionEnrolment)
	if w := in.call(t, "POST", "/api/v1/me/totp", "", enrolling); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "enrols passkeys and nothing else") {
		t.Errorf("a session that may only enrol answered %d %s", w.Code, w.Body)
	}
}

// A generator is removed with a code it shows now: a wrong one is 403 and counted as a guess, the
// generator left; the right one removes it, recorded as credential.remove, and the password signs in
// with no code again. An account holding none is a 404.
func TestATOTPGeneratorIsRemovedWithACodeItShows(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c, err := in.opening(t, "bob", api.OpenedBy{Credential: "bob-password"})
	if err != nil {
		t.Fatal(err)
	}
	remove := func(code string) *httptest.ResponseRecorder {
		t.Helper()
		return in.call(t, "DELETE", "/api/v1/me/totp", fmt.Sprintf(`{"totp":%q}`, code), c)
	}
	now := totp.StepAt(*in.clock)
	if w := remove(totp.Code(bobsSecret, now+3)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "left as it was") {
		t.Errorf("a wrong code answered %d %s", w.Code, w.Body)
	}
	if w := in.call(t, "DELETE", "/api/v1/me/totp", "", c); w.Code != http.StatusBadRequest {
		t.Errorf("no code answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where id = 'bob-totp'`); n != 1 {
		t.Fatal("a wrong code removed bob's generator")
	}
	if w := remove(totp.Code(bobsSecret, now)); w.Code != http.StatusNoContent {
		t.Fatalf("the code it shows answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'bob' and type = 'totp'`); n != 0 {
		t.Error("bob's generator is still there")
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.remove' and actor = 'bob' and target = 'bob-totp'`); n != 1 {
		t.Error("the removal is not recorded")
	}
	if w := in.as(t, "bob", ""); w.Code != http.StatusOK {
		t.Errorf("bob's password with no code answered %d %s", w.Code, w.Body)
	}
	if w := remove(totp.Code(bobsSecret, now)); w.Code != http.StatusNotFound {
		t.Errorf("removing it again answered %d %s", w.Code, w.Body)
	}

	// Wrong codes are guesses: ten of them, and the eleventh attempt is refused.
	carol := in.signedIn(t, "carol", api.SessionFull)
	started, secret := in.started(t, carol)
	if w := in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, totp.Code(secret, now)), carol); w.Code != http.StatusOK {
		t.Fatalf("confirming carol's generator answered %d %s", w.Code, w.Body)
	}
	for range 10 {
		in.call(t, "DELETE", "/api/v1/me/totp", fmt.Sprintf(`{"totp":%q}`, totp.Code(secret, now+5)), carol)
	}
	*in.clock = in.clock.Add(totp.Step)
	if w := in.call(t, "DELETE", "/api/v1/me/totp", fmt.Sprintf(`{"totp":%q}`, totp.Code(secret, now+1)), carol); w.Code != http.StatusTooManyRequests {
		t.Errorf("the eleventh attempt answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where id = $1`, started.ID); n != 1 {
		t.Error("a refused attempt removed carol's generator")
	}
}

// A first administrator's link sets a password and ends the bootstrap token where the session it
// opens is a full one: on an installation addressed by an IP address, whatever the stored policy
// says, and where the policy requires no passkey. Where it requires one, the session only enrols and
// the bootstrap stays, the token still an administrator's, until the first passkey registered from
// that session ends it.
func TestTheFirstAdministratorsPasswordEndsTheBootstrapWhereThePolicyIsMet(t *testing.T) {
	enrolled := func(t *testing.T, in ceremonies, origin, login string) (*httptest.ResponseRecorder, api.PasswordEnrolled) {
		t.Helper()
		// The link, whichever host the installation is addressed by, carries its code after its #.
		created := in.bearer(t, "POST", "/api/v1/users", in.bootstrap, fmt.Sprintf(`{"login":%q,"admin":true}`, login))
		var user api.CreatedUser
		if err := json.Unmarshal(created.Body.Bytes(), &user); err != nil || created.Code != http.StatusCreated {
			t.Fatalf("creating %s answered %d %s", login, created.Code, created.Body)
		}
		_, code, _ := strings.Cut(user.Enrolment.Link, "#")
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/auth/password/enrol",
			strings.NewReader(fmt.Sprintf(`{"code":%q,"password":"the first administrator's"}`, code)))
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		in.h.ServeHTTP(w, r)
		var answer api.PasswordEnrolled
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusOK {
			t.Fatalf("the first administrator's password answered %d %s", w.Code, w.Body)
		}
		return w, answer
	}
	ended := func(t *testing.T, in ceremonies) bool {
		t.Helper()
		return in.count(t, `select count(*) from bootstrap where enrolled_at is not null and token_hash is null`) == 1
	}

	t.Run("an IP address", func(t *testing.T) {
		in := ceremoniesOn(t, "https://192.0.2.10")
		_, answer := enrolled(t, in, "https://192.0.2.10", "alice")
		if answer.Session != api.SessionFull || !ended(t, in) {
			t.Errorf("the session is %q, and the bootstrap ended: %v", answer.Session, ended(t, in))
		}
		if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("the bootstrap token answered %d %s", w.Code, w.Body)
		}
		if got := in.actions(t); !slices.Contains(got, "alice bootstrap.end operator") {
			t.Errorf("the audit log reads %q", got)
		}
	})

	t.Run("passkeys optional", func(t *testing.T) {
		in := someCeremonies(t)
		bound := false
		if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.SetInstallationPolicy(ctx, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2}, *in.clock)
		}); err != nil {
			t.Fatal(err)
		}
		if _, answer := enrolled(t, in, publicOrigin, "alice"); answer.Session != api.SessionFull || !ended(t, in) {
			t.Errorf("the session is %q, and the bootstrap ended: %v", answer.Session, ended(t, in))
		}
	})

	t.Run("a passkey required", func(t *testing.T) {
		in := someCeremonies(t)
		w, answer := enrolled(t, in, publicOrigin, "alice")
		if answer.Session != api.SessionEnrolment || ended(t, in) {
			t.Fatalf("the session is %q, and the bootstrap ended: %v", answer.Session, ended(t, in))
		}
		if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusOK {
			t.Errorf("the bootstrap token after the password answered %d %s", w.Code, w.Body)
		}
		c := session(t, w)
		made, _, err := newBrowser().Create(in.options(t, `{"ceremony":"registration"}`, c))
		if err != nil {
			t.Fatal(err)
		}
		if w := in.verify(t, "registration", made, "", "", c); w.Code != http.StatusOK {
			t.Fatalf("the passkey registered from the session answered %d %s", w.Code, w.Body)
		}
		if !ended(t, in) {
			t.Error("the first passkey left the bootstrap going")
		}
		if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("the bootstrap token after the passkey answered %d %s", w.Code, w.Body)
		}
		got := in.actions(t)
		if len(got) < 2 || got[len(got)-2] != "alice credential.enrol "+made.ID || got[len(got)-1] != "alice bootstrap.end operator" {
			t.Errorf("the audit log reads %q", got)
		}
	})
}

// A first administrator whose password was set while the policy required a passkey, which opened a
// session that only enrols and left the bootstrap token going, ends the token at the first sign-in
// of that password to a full session, the policy relaxed since: recorded as bootstrap.end before the
// sign-in, and once. A sign-in that only enrols ends nothing, and neither does a full one of somebody
// who administers nothing.
func TestAnAdministratorsPasswordSigningInToAFullSessionEndsTheBootstrap(t *testing.T) {
	in := someCeremonies(t)
	const secret = "a password of their own"
	for _, u := range []struct {
		login string
		admin bool
	}{{"alice", true}, {"bob", false}} {
		w := in.call(t, "POST", "/api/v1/auth/password/enrol", fmt.Sprintf(`{"code":%q,"password":%q}`, in.user(t, u.login, u.admin), secret), "")
		var answer api.PasswordEnrolled
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusOK || answer.Session != api.SessionEnrolment {
			t.Fatalf("%s setting a password answered %d %s", u.login, w.Code, w.Body)
		}
	}
	signIn := func(login, kind string) {
		t.Helper()
		w := in.call(t, "POST", "/api/v1/auth/login", fmt.Sprintf(`{"login":%q,"password":%q}`, login, secret), "")
		var answer api.SignedIn
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusOK || answer.Session != kind {
			t.Fatalf("%s signing in answered %d %s, want a session %s", login, w.Code, w.Body, kind)
		}
	}
	ended := func() bool {
		t.Helper()
		return in.count(t, `select count(*) from bootstrap where enrolled_at is not null and token_hash is null`) == 1
	}

	signIn("alice", api.SessionEnrolment)
	if ended() {
		t.Fatal("alice signing in to a session that only enrols ended the bootstrap")
	}
	bound := false
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.SetInstallationPolicy(ctx, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2}, *in.clock)
	}); err != nil {
		t.Fatal(err)
	}
	signIn("bob", api.SessionFull)
	if ended() {
		t.Fatal("bob, who administers nothing, signing in to a full session ended the bootstrap")
	}
	signIn("alice", api.SessionFull)
	if !ended() {
		t.Fatal("alice signing in to a full session left the bootstrap going")
	}
	if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the bootstrap token after alice signed in answered %d %s", w.Code, w.Body)
	}
	got := in.actions(t)
	if len(got) < 2 || !slices.Equal(got[len(got)-2:], []string{"alice bootstrap.end operator", "alice signin.succeed alice"}) {
		t.Errorf("the audit log reads %q", got)
	}
	signIn("alice", api.SessionFull)
	if n := in.count(t, `select count(*) from audit_log where action = 'bootstrap.end'`); n != 1 {
		t.Errorf("the bootstrap is recorded as ended %d times", n)
	}
}

// What was checked is checked again in the transaction that acts on it: a generator started again
// between a code confirming the first and the enrolment is not enrolled, the one started last still
// waiting; a generator removed between a code checked and the removal is 409; and a password
// changed between the current one checked and the change is 409, the change it met kept.
func TestWhatChangesWhileAPasswordOrAGeneratorIsSetIsCheckedAgain(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c := in.signedIn(t, "alice", api.SessionFull)
	_, secret := in.started(t, c)
	var again api.TOTPStarted
	api.BetweenChecksAndSignIn(in.passwords, func() {
		api.BetweenChecksAndSignIn(in.passwords, nil)
		again, _ = in.started(t, c)
	})
	if w := in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, totp.Code(secret, totp.StepAt(*in.clock))), c); w.Code != http.StatusConflict {
		t.Errorf("a generator started again during its confirmation answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'alice' and type = 'totp'`); n != 0 {
		t.Error("a generator replaced during its confirmation was enrolled")
	}
	if n := in.count(t, `select count(*) from totp_enrolments where id = $1`, again.ID); n != 1 {
		t.Error("the generator started last is not waiting")
	}

	bob, err := in.opening(t, "bob", api.OpenedBy{Credential: "bob-password"})
	if err != nil {
		t.Fatal(err)
	}
	api.BetweenChecksAndSignIn(in.passwords, func() {
		api.BetweenChecksAndSignIn(in.passwords, nil)
		in.exec(t, `delete from credentials where id = 'bob-totp'`)
	})
	if w := in.call(t, "DELETE", "/api/v1/me/totp", fmt.Sprintf(`{"totp":%q}`, totp.Code(bobsSecret, totp.StepAt(*in.clock))), bob); w.Code != http.StatusConflict {
		t.Errorf("a generator removed during its removal answered %d %s", w.Code, w.Body)
	}

	api.BetweenChecksAndSignIn(in.passwords, func() {
		api.BetweenChecksAndSignIn(in.passwords, nil)
		if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"bob's other passphrase","current_password":"bob's own"}`, bob); w.Code != http.StatusOK {
			t.Errorf("the change during the change answered %d %s", w.Code, w.Body)
		}
	})
	if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"bob's new passphrase","current_password":"bob's own"}`, bob); w.Code != http.StatusConflict {
		t.Errorf("a password changed during its change answered %d %s", w.Code, w.Body)
	}
	if !in.matches(t, "bob", "bob's other passphrase") {
		t.Error("the change that met another undid it")
	}
}

// A generator the session enrolled itself does not start the account's count again: its right code
// gives back its own attempt and no other, so that guesses at the password from a session are held
// to the count whatever generator it enrols and removes between them.
func TestRemovingAGeneratorStartsNoCountAgain(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c := in.signedIn(t, "alice", api.SessionFull)
	_, secret := in.started(t, c)
	now := totp.StepAt(*in.clock)
	if w := in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, totp.Code(secret, now)), c); w.Code != http.StatusOK {
		t.Fatalf("confirming the generator answered %d %s", w.Code, w.Body)
	}
	guess := func() int {
		t.Helper()
		return in.call(t, "PUT", "/api/v1/me/password", `{"password":"a long enough passphrase","current_password":"a guess"}`, c).Code
	}
	for range 9 {
		if code := guess(); code != http.StatusForbidden {
			t.Fatalf("a guess answered %d", code)
		}
	}
	*in.clock = in.clock.Add(totp.Step)
	if w := in.call(t, "DELETE", "/api/v1/me/totp", fmt.Sprintf(`{"totp":%q}`, totp.Code(secret, now+1)), c); w.Code != http.StatusNoContent {
		t.Fatalf("removing the generator answered %d %s", w.Code, w.Body)
	}
	if code := guess(); code != http.StatusForbidden {
		t.Errorf("the tenth guess answered %d", code)
	}
	if code := guess(); code != http.StatusTooManyRequests {
		t.Errorf("the eleventh guess, after a generator removed, answered %d", code)
	}
}

// An installation addressed by an IPv6 address names the login alone as the generator's account,
// since the Key Uri Format allows no colon in it.
func TestAGeneratorsAccountHoldsNoColon(t *testing.T) {
	in := passwordsAt(t, "https://[2001:db8::1]", false)
	c := in.signedIn(t, "alice", api.SessionFull)
	started, _ := in.started(t, c)
	if !strings.HasPrefix(started.URI, "otpauth://totp/Agentiik:alice?") {
		t.Errorf("the URI is %s", started.URI)
	}
}
