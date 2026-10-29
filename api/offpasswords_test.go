package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/totp"
)

// The path off passwords and what guards the credentials added on it, over a real PostgreSQL: the
// passkey that brings an account to min_passkeys under a policy taking it off passwords takes the
// password; an account suspended for holding no passkey comes back by enrolling one from a code, the
// first administrator's enrolment then ending the bootstrap; and a passkey, a first password, a
// TOTP generator or an API token added from a session asks for a sign-in within the last ten
// minutes.

// proofLife is how recent a sign-in adding a credential from a session asks for, as the API holds
// it.
const proofLife = 10 * time.Minute

// signInAgain is RFC 9470's challenge, which a refusal for want of a recent sign-in carries.
const signInAgain = `Bearer error="insufficient_user_authentication", max_age="600"`

// registered runs a registration from the session c carries, as the enrolment page does, and
// answers the verification.
func (in passwordsOf) registered(t *testing.T, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	ceremonies := ceremonies{pool: in.pool, super: in.super, clock: in.clock, h: in.h}
	made, _, err := newBrowser().Create(ceremonies.options(t, `{"ceremony":"registration"}`, c))
	if err != nil {
		t.Fatal(err)
	}
	return ceremonies.verify(t, "registration", made, "", "", c)
}

// askedAgain says whether w is the refusal asking for a sign-in again: a 403 with its sentence and
// its challenge.
func askedAgain(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusForbidden && w.Header().Get("WWW-Authenticate") == signInAgain &&
		strings.Contains(w.Body.String(), "sign in again")
}

// Under a passkey required, bob, holding a password and a generator, signs in with the password to
// a session that only enrols, and the second passkey he registers from it takes both, recorded, and
// the session with them, told to nobody, since no recovery code set the password. Where a namespace alice holds a grant in forbids passwords, which she held
// before it did, her second passkey takes hers too. Where passkeys are optional and passwords
// allowed, reaching min_passkeys takes nothing.
func TestThePasskeyThatBringsAnAccountToMinPasskeysTakesItsPassword(t *testing.T) {
	in := somePasswords(t)
	w := in.as(t, "bob", totp.Code(bobsSecret, totp.StepAt(*in.clock)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"session":"enrolment"`) {
		t.Fatalf("bob signing in answered %d %s", w.Code, w.Body)
	}
	c := session(t, w)
	for i := range 2 {
		if w := in.registered(t, c); w.Code != http.StatusOK {
			t.Fatalf("bob's passkey %d answered %d %s", i+1, w.Code, w.Body)
		}
	}
	if n := in.count(t, `select count(*) from credentials where login = 'bob' and type in ('password', 'totp')`); n != 0 {
		t.Errorf("bob holds %d of his password and generator beside two passkeys", n)
	}
	if code, _ := in.me(t, c); code != http.StatusUnauthorized {
		t.Errorf("the session bob's password opened answered %d", code)
	}
	if got := in.trail(t); !slices.Equal(got[len(got)-2:], []string{"bob credential.remove bob-password", "bob credential.remove bob-totp"}) {
		t.Errorf("the audit log ends %q", got[len(got)-2:])
	}
	// His password was set by no recovery code, and the passkeys it led to are told to nobody.
	if n := in.count(t, `select count(*) from notifications`); n != 0 {
		t.Errorf("the passkeys bob's own password led to are told %d times", n)
	}

	in.policy(t, "allowed", "optional")
	carol := in.passkeyed(t, "carol", "carol-passkey", false)
	if w := in.registered(t, carol); w.Code != http.StatusOK {
		t.Fatalf("carol's second passkey answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "carol") == "" {
		t.Error("carol's password went where passkeys are optional and passwords allowed")
	}

	in.tighten(t, db.AuthPolicy{Password: "forbidden"})
	alice := in.passkeyed(t, "alice", "alice-passkey", false)
	if w := in.registered(t, alice); w.Code != http.StatusOK {
		t.Fatalf("alice's second passkey answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "alice") != "" {
		t.Error("alice's password outlived her second passkey where finance forbids passwords")
	}
}

// Passwords forbidden while two users hold nothing yet suspend both, recorded as for having no
// passkey; each comes back by enrolling one from the link they were given, which lifts the
// suspension, records it on credential.enrol and signs them in, and the first administrator's then
// ends the bootstrap token. A suspension recorded for no reason is not the passkey's to lift.
func TestEnrollingAPasskeyFromACodeLiftsASuspensionForHavingNone(t *testing.T) {
	in := someCeremonies(t)
	first := in.user(t, "alice", true)
	bobs := in.user(t, "bob", false)
	dave := in.user(t, "dave", false)
	if w := in.bearer(t, "PUT", "/api/v1/auth/policy", in.bootstrap, `{"password":"forbidden"}`); w.Code != http.StatusOK {
		t.Fatalf("forbidding passwords answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from users where suspended and suspended_for = 'no_passkey'`); n != 3 {
		t.Fatalf("%d users are suspended for having no passkey, of three", n)
	}
	in.exec(t, `update users set suspended_for = null where login = 'dave'`)

	w := in.enrol(t, newBrowser(), bobs, "")
	if w.Code != http.StatusOK {
		t.Fatalf("bob's registration answered %d %s", w.Code, w.Body)
	}
	session(t, w)
	if n := in.count(t, `select count(*) from users where login = 'bob' and not suspended and suspended_for is null`); n != 1 {
		t.Error("bob's registration left him suspended")
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.enrol' and actor = 'bob'
	                       and detail::jsonb->>'suspension_lifted' = 'no_passkey'`); n != 1 {
		t.Error("the lifting is not recorded on bob's credential.enrol")
	}
	if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusOK {
		t.Fatalf("the bootstrap token after bob enrolled answered %d %s", w.Code, w.Body)
	}

	if w := in.enrol(t, newBrowser(), dave, ""); w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
		t.Errorf("dave's registration answered %d %s, setting %v", w.Code, w.Body, w.Result().Cookies())
	}
	if n := in.count(t, `select count(*) from users where login = 'dave' and suspended`); n != 1 {
		t.Error("a suspension recorded for no reason was lifted by a passkey")
	}

	if w := in.enrol(t, newBrowser(), first, ""); w.Code != http.StatusOK {
		t.Fatalf("alice's registration answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from users where login = 'alice' and not suspended`); n != 1 {
		t.Error("the first administrator's registration left her suspended")
	}
	if n := in.count(t, `select count(*) from bootstrap where enrolled_at is not null`); n != 1 {
		t.Error("the first administrator's registration, lifting her suspension, did not end the bootstrap")
	}
	if got := in.actions(t); !slices.Contains(got, "alice bootstrap.end operator") {
		t.Errorf("the audit log reads %q", got)
	}
}

// A passkey registered from a session asks for the session to have been signed in to within ten
// minutes of the options: at ten it has, past them the options are a 403 asking to sign in again,
// with RFC 9470's challenge, and a sign-in again opens a session that registers. The verification
// holds the session it carries to the options' issue, so an older session of the same user is asked
// again too.
func TestAPasskeyRegisteredFromASessionAsksForARecentSignIn(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	ceremonies := ceremonies{pool: in.pool, super: in.super, clock: in.clock, h: in.h}
	old := in.passkeyed(t, "carol", "carol-passkey", false)
	*in.clock = in.clock.Add(proofLife)
	ceremonies.options(t, `{"ceremony":"registration"}`, old)
	*in.clock = in.clock.Add(time.Second)
	if w := ceremonies.call(t, "POST", "/api/v1/auth/passkey/options", `{"ceremony":"registration"}`, "", old); !askedAgain(w) {
		t.Errorf("the options, eleven minutes after the sign-in, answered %d %s, %q", w.Code, w.Body, w.Header().Get("WWW-Authenticate"))
	}
	fresh := in.passkeyed(t, "carol", "carol-passkey", false)
	made, _, err := newBrowser().Create(ceremonies.options(t, `{"ceremony":"registration"}`, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if w := ceremonies.verify(t, "registration", made, "", "", old); !askedAgain(w) {
		t.Errorf("a registration finished from the older session answered %d %s", w.Code, w.Body)
	}
	made, _, err = newBrowser().Create(ceremonies.options(t, `{"ceremony":"registration"}`, fresh))
	if err != nil {
		t.Fatal(err)
	}
	*in.clock = in.clock.Add(4 * time.Minute)
	if w := ceremonies.verify(t, "registration", made, "", "", fresh); w.Code != http.StatusOK {
		t.Errorf("a registration from the fresh session answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'carol' and type = 'passkey'`); n != 2 {
		t.Errorf("carol holds %d passkeys, and registered one", n)
	}
}

// A first password set from a session asks for a sign-in within the last ten minutes; a password
// changed is sent beside the one it replaces, which proves it, however long ago the session was
// signed in to.
func TestAFirstPasswordFromASessionAsksForARecentSignIn(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	old := in.passkeyed(t, "erin", "erin-passkey", false)
	alice := in.signedIn(t, "alice", api.SessionFull)
	*in.clock = in.clock.Add(proofLife + time.Second)
	if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"erin's own passphrase"}`, old); !askedAgain(w) {
		t.Errorf("erin's first password, eleven minutes after her sign-in, answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "erin") != "" {
		t.Error("erin's first password was set all the same")
	}
	fresh := in.passkeyed(t, "erin", "erin-passkey", false)
	if w := in.call(t, "PUT", "/api/v1/me/password", `{"password":"erin's own passphrase"}`, fresh); w.Code != http.StatusOK {
		t.Errorf("erin's first password from a fresh session answered %d %s", w.Code, w.Body)
	}
	*in.clock = in.clock.Add(time.Hour)
	body := fmt.Sprintf(`{"password":"alice's new passphrase","current_password":%q}`, thePasswords["alice"])
	if w := in.call(t, "PUT", "/api/v1/me/password", body, alice); w.Code != http.StatusOK {
		t.Errorf("alice's password changed with the current one, an hour after her sign-in, answered %d %s", w.Code, w.Body)
	}
}

// A TOTP generator is started from a session signed in to within the last ten minutes, and confirmed
// from one signed in to within ten minutes of its start, or since: a session older than that is
// asked to sign in again at either.
func TestAGeneratorAsksForARecentSignIn(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	old := in.signedIn(t, "alice", api.SessionFull)
	*in.clock = in.clock.Add(proofLife + time.Second)
	if w := in.call(t, "POST", "/api/v1/me/totp", "", old); !askedAgain(w) {
		t.Errorf("a generator started eleven minutes after the sign-in answered %d %s", w.Code, w.Body)
	}
	fresh := in.signedIn(t, "alice", api.SessionFull)
	_, secret := in.started(t, fresh)
	*in.clock = in.clock.Add(9 * time.Minute)
	code := totp.Code(secret, totp.StepAt(*in.clock))
	if w := in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, code), old); !askedAgain(w) {
		t.Errorf("a generator confirmed from a session signed in to before its start answered %d %s", w.Code, w.Body)
	}
	if w := in.call(t, "POST", "/api/v1/me/totp/confirm", fmt.Sprintf(`{"totp":%q}`, code), fresh); w.Code != http.StatusOK {
		t.Errorf("a generator confirmed nine minutes after its start answered %d %s", w.Code, w.Body)
	}
}

// An API token minted from a session asks for a sign-in within the last ten minutes, as a passkey
// does, since it outlives the session: at ten it mints, past them it is the 403 asking to sign in
// again, with nothing written, and a fresh sign-in mints. A bearer token mints however old it is.
func TestATokenMintedFromASessionAsksForARecentSignIn(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	old := in.signedIn(t, "alice", api.SessionFull)
	*in.clock = in.clock.Add(proofLife)
	w := in.call(t, "POST", "/api/v1/auth/tokens", `{"device_label":"laptop"}`, old)
	if w.Code != http.StatusCreated {
		t.Fatalf("a token minted ten minutes after the sign-in answered %d %s", w.Code, w.Body)
	}
	var minted api.IssuedToken
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	*in.clock = in.clock.Add(time.Second)
	if w := in.call(t, "POST", "/api/v1/auth/tokens", `{"device_label":"laptop"}`, old); !askedAgain(w) {
		t.Errorf("a token minted ten minutes and a second after the sign-in answered %d %s, %q", w.Code, w.Body, w.Header().Get("WWW-Authenticate"))
	}
	if n := in.count(t, `select count(*) from api_tokens where principal = 'alice'`); n != 1 {
		t.Errorf("alice holds %d tokens, and was minted one", n)
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'api_token.create'`); n != 1 {
		t.Errorf("%d tokens minted are recorded, and one was", n)
	}
	fresh := in.signedIn(t, "alice", api.SessionFull)
	if w := in.call(t, "POST", "/api/v1/auth/tokens", `{}`, fresh); w.Code != http.StatusCreated {
		t.Errorf("a token minted from a fresh sign-in answered %d %s", w.Code, w.Body)
	}
	*in.clock = in.clock.Add(time.Hour)
	if w := in.bearing(t, "POST", "/api/v1/auth/tokens", minted.Token, `{}`); w.Code != http.StatusCreated {
		t.Errorf("a token minted with a bearer token an hour after it was answered %d %s", w.Code, w.Body)
	}
}

// A synced passkey is refused where device_bound_only comes to apply while it is verified, read
// again under the user's row: nothing is written, and a suspension for having no passkey is not
// lifted by a passkey that signs nobody in.
func TestAPolicyChangedWhileAPasskeyIsVerifiedIsTheOneItMeets(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	in.exec(t, `update users set suspended = true, suspended_for = 'no_passkey' where login = 'erin'`)
	code := in.enrolCode(t, "erin", db.EnrolmentRecovery)
	ceremonies := ceremonies{pool: in.pool, super: in.super, clock: in.clock, h: in.h}
	browser := newBrowser()
	browser.BackupEligible = true
	made, _, err := browser.Create(ceremonies.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)))
	if err != nil {
		t.Fatal(err)
	}
	bound := true
	api.BetweenVerifyAndRegister(in.passkeys, func() {
		in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2})
	})
	if w := ceremonies.verify(t, "registration", made, "", ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"device_bound_only"`) {
		t.Errorf("a synced passkey the policy came to refuse answered %d %s", w.Code, w.Body)
	}
	if got := in.suspended(t); !slices.Contains(got, "erin:no_passkey") {
		t.Errorf("the suspended accounts are %q", got)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'erin'`); n != 0 {
		t.Errorf("erin holds %d credentials", n)
	}
}

// A passkey registered from a session is not written for an account suspended while it was
// verified, since a suspended account enrols with a code alone.
func TestAPasskeyFromASessionIsNotWrittenForAnAccountSuspendedMeanwhile(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c := in.passkeyed(t, "carol", "carol-passkey", false)
	ceremonies := ceremonies{pool: in.pool, super: in.super, clock: in.clock, h: in.h}
	made, _, err := newBrowser().Create(ceremonies.options(t, `{"ceremony":"registration"}`, c))
	if err != nil {
		t.Fatal(err)
	}
	api.BetweenVerifyAndRegister(in.passkeys, func() {
		in.exec(t, `update users set suspended = true, suspended_for = 'no_passkey' where login = 'carol'`)
	})
	if w := ceremonies.verify(t, "registration", made, "", "", c); w.Code != http.StatusUnauthorized {
		t.Errorf("a passkey from the session of carol, suspended meanwhile, answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'carol' and type = 'passkey'`); n != 1 {
		t.Errorf("carol holds %d passkeys, and held one", n)
	}
}

// The bootstrap token's change of the policy is refused where the first administrator's enrolment
// ended the bootstrap while it was checked, as every act of the token is, and writes nothing.
func TestTheBootstrapTokensPolicyChangeMeetsItsEnd(t *testing.T) {
	in := someCeremonies(t)
	first := in.user(t, "alice", true)
	once := false
	api.BetweenIdentifyAndSetPolicy(in.policies, func() {
		if once {
			return
		}
		once = true
		if w := in.enrol(t, newBrowser(), first, ""); w.Code != http.StatusOK {
			t.Fatalf("alice's enrolment answered %d %s", w.Code, w.Body)
		}
	})
	if w := in.bearer(t, "PUT", "/api/v1/auth/policy", in.bootstrap, `{"passkey":"optional"}`); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "bootstrap token") {
		t.Errorf("the bootstrap token's change answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from auth_policy where namespace is null and passkey = 'required'`); n != 1 {
		t.Error("the bootstrap token's change was written after the bootstrap ended")
	}
}
