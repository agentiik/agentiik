package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// Recovery codes, over a real PostgreSQL: issued by an administrator at POST
// /api/v1/users/{login}/recovery, shown once and audited with both identities; refused for the
// administrator's own account and for a service account; issued by the installation itself on the
// break-glass path; and spent on the enrolment page, for a passkey or a password, ending the
// bootstrap token where the account is an administrator's and the token has not ended.

// recoveryOf reads a recovery code answered, failing the test unless it is one: shown once, its code
// the one its link carries, good for an hour from now.
func recoveryOf(t *testing.T, w *httptest.ResponseRecorder, now time.Time) api.RecoveryCode {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("the recovery code answered %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a recovery code shown once was answered with Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	var code api.RecoveryCode
	if err := json.Unmarshal(w.Body.Bytes(), &code); err != nil {
		t.Fatal(err)
	}
	if codeOf(t, code.Link) != code.Code || !code.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("the recovery code is %+v, its link carrying another code or lapsing at another time than %s", code, now.Add(time.Hour))
	}
	return code
}

// An administrator issues a user a recovery code, shown once, whose code is kept as its SHA-256 as
// a code of the kind recovery, issued by the administrator for the user, whether the user holds a
// credential or not. A fresh one revokes the one before it. Each is recorded as enrolment.issue by
// the administrator for the user, with its kind, and never with its code.
func TestAnAdministratorIssuesARecoveryCodeAuditedWithBothIdentities(t *testing.T) {
	in := somePeople(t)
	first := recoveryOf(t, in.ask(t, "POST", "/api/v1/users/alice/recovery", in.carol, "", nil), in.now)
	c, err := in.openCode(t, first.Code)
	if err != nil || c.Login != "alice" || c.Kind != db.EnrolmentRecovery || c.IssuedBy != "carol" {
		t.Errorf("the recovery code reads as %+v, %v", c, err)
	}

	// alice lost the passkey she holds: an enrolment link is refused her, a recovery code is
	// not, and it replaces the one before it.
	in.enrol(t, "alice")
	if w := in.ask(t, "POST", "/api/v1/users/alice/enrolment", in.carol, "", nil); w.Code != http.StatusConflict {
		t.Errorf("an enrolment link for alice, enrolled, answered %d %s", w.Code, w.Body)
	}
	second := recoveryOf(t, in.ask(t, "POST", "/api/v1/users/alice/recovery", in.carol, "", nil), in.now)
	if second.Code == first.Code {
		t.Fatal("a second recovery code was answered the first one's code")
	}
	if _, err := in.openCode(t, first.Code); !errors.Is(err, db.ErrNoEnrolmentCode) {
		t.Errorf("the recovery code a fresh one replaced was answered %v", err)
	}
	if _, err := in.openCode(t, second.Code); err != nil {
		t.Errorf("the fresh recovery code was answered %v", err)
	}

	entries := audited(t, in.pool)
	var got []string
	for _, e := range entries {
		got = append(got, e.Actor+" "+e.Action+" "+e.Target+" "+e.Result)
		for _, value := range []string{first.Code, second.Code} {
			if strings.Contains(e.Detail, value) {
				t.Errorf("entry %d carries a recovery code: %s", e.Seq, e.Detail)
			}
		}
	}
	if want := []string{"carol enrolment.issue alice done", "carol enrolment.issue alice done"}; !slices.Equal(got, want) {
		t.Fatalf("the audit log reads %q", got)
	}
	for i, replaced := range []bool{false, true} {
		d := detailOf(t, entries[i])
		if d["kind"] != "recovery" || d["replaced"] != replaced || d["expires_at"] != in.now.Add(time.Hour).Format(time.RFC3339Nano) {
			t.Errorf("entry %d details %s", entries[i].Seq, entries[i].Detail)
		}
	}
}

// A recovery code is refused for the administrator asking it, whatever they hold, since a code
// issued to oneself would turn a token somebody took into a passkey of their own; for a service
// account, which holds nothing a code replaces; for a login no user has or that no user can have;
// and with a body, which the route does not read. Nothing is issued or recorded for any of them.
func TestARecoveryCodeIsRefusedForOnesOwnAccountAndForAServiceAccount(t *testing.T) {
	in := somePeople(t)
	in.enrol(t, "carol")
	for _, c := range []struct {
		path, body string
		want       int
		says       string
	}{
		{"/api/v1/users/carol/recovery", "", http.StatusForbidden, api.SelfRecovery},
		{"/api/v1/users/finance%2Fagentiik/recovery", "", http.StatusNotFound, "a service account is no user"},
		{"/api/v1/users/nobody/recovery", "", http.StatusNotFound, "no user by that login"},
		{"/api/v1/users/Alice/recovery", "", http.StatusNotFound, "no user by that login"},
		{"/api/v1/users/operator/recovery", "", http.StatusNotFound, "no user by that login"},
		{"/api/v1/users/alice/recovery", `{"kind":"enrolment"}`, http.StatusBadRequest, "not a field"},
	} {
		w := in.ask(t, "POST", c.path, in.carol, c.body, nil)
		var said struct {
			Error string `json:"error"`
		}
		json.Unmarshal(w.Body.Bytes(), &said)
		if w.Code != c.want || !strings.Contains(said.Error, c.says) {
			t.Errorf("%s %s answered %d %s, want %d saying %q", c.path, c.body, w.Code, w.Body, c.want, c.says)
		}
	}
	if n := in.count(t, `select count(*) from enrolment_codes`) + in.count(t, `select count(*) from audit_log`); n != 0 {
		t.Errorf("the refusals left %d codes and entries", n)
	}
}

// The bootstrap token issues recovery codes, as it administers everything else, until the first
// administrator has enrolled; issued by operator, they end with it, and once it has ended it issues
// none.
func TestTheBootstrapTokenIssuesRecoveryCodesThatEndWithIt(t *testing.T) {
	in := somePeople(t)
	issued := recoveryOf(t, in.ask(t, "POST", "/api/v1/users/alice/recovery", in.bootstrap, "", nil), in.now)
	if c, err := in.openCode(t, issued.Code); err != nil || c.IssuedBy != "operator" || c.Kind != db.EnrolmentRecovery {
		t.Errorf("the bootstrap token's recovery code reads as %+v, %v", c, err)
	}
	by := recoveryOf(t, in.ask(t, "POST", "/api/v1/users/alice/recovery", in.carol, "", nil), in.now)
	carols := recoveryOf(t, in.ask(t, "POST", "/api/v1/users/carol/recovery", in.bootstrap, "", nil), in.now)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, in.now)
		return err
	})
	if _, err := in.openCode(t, carols.Code); !errors.Is(err, db.ErrNoEnrolmentCode) {
		t.Errorf("a recovery code the bootstrap token issued was answered %v once it ended", err)
	}
	if _, err := in.openCode(t, by.Code); err != nil {
		t.Errorf("an administrator's recovery code was answered %v once the bootstrap token ended", err)
	}
	if w := in.ask(t, "POST", "/api/v1/users/alice/recovery", in.bootstrap, "", nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "first administrator enrolled") {
		t.Errorf("the ended bootstrap token issuing a recovery code answered %d %s", w.Code, w.Body)
	}
}

// The break-glass path issues an administrator a recovery code as the installation itself, recorded
// as enrolment.issue by installation, revoking the one another administrator issued them, and
// whether they are suspended or hold no credential at all. It recovers nobody who is not an
// administrator, and no login no user has, writing nothing for either.
func TestTheBreakGlassPathRecoversAnAdministratorAndNobodyElse(t *testing.T) {
	in := somePeople(t)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "dan", DisplayName: "Dan", Admin: true})
	})
	in.enrol(t, "carol")
	before := recoveryOf(t, in.ask(t, "POST", "/api/v1/users/dan/recovery", in.carol, "", nil), in.now)
	in.exec(t, `update users set suspended = true where login in ('carol', 'dan')`)
	for _, login := range []string{"carol", "dan"} {
		code, err := api.BreakGlass(t.Context(), in.pool, "https://agentiik.example.com/", login, in.now)
		if err != nil {
			t.Fatalf("the break-glass path for %s answered %v", login, err)
		}
		if codeOf(t, code.Link) != code.Code || !code.ExpiresAt.Equal(in.now.Add(time.Hour)) {
			t.Errorf("the break-glass code for %s is %+v", login, code)
		}
		if c, err := in.openCode(t, code.Code); err != nil || c.Login != login || c.Kind != db.EnrolmentRecovery || c.IssuedBy != "installation" {
			t.Errorf("the break-glass code for %s reads as %+v, %v", login, c, err)
		}
	}
	if _, err := in.openCode(t, before.Code); !errors.Is(err, db.ErrNoEnrolmentCode) {
		t.Errorf("the recovery code the break-glass code replaced was answered %v", err)
	}

	codes, entries := in.count(t, `select count(*) from enrolment_codes`), len(audited(t, in.pool))
	for login, want := range map[string]error{"alice": api.ErrNotAdministrator, "nobody": db.ErrNoPrincipal} {
		if _, err := api.BreakGlass(t.Context(), in.pool, "https://agentiik.example.com", login, in.now); !errors.Is(err, want) {
			t.Errorf("the break-glass path for %s answered %v, want %v", login, err, want)
		}
	}
	if _, err := api.BreakGlass(t.Context(), in.pool, "https://agentiik.example.com", "Carol", in.now); err == nil || !strings.Contains(err.Error(), "is not a login") {
		t.Errorf("the break-glass path for a login no user can have answered %v", err)
	}
	if in.count(t, `select count(*) from enrolment_codes`) != codes || len(audited(t, in.pool)) != entries {
		t.Error("a refused break-glass code left a code or an entry")
	}
	var got []string
	for _, e := range audited(t, in.pool) {
		got = append(got, e.Actor+" "+e.Action+" "+e.Target)
	}
	if want := []string{"carol enrolment.issue dan", "installation enrolment.issue carol", "installation enrolment.issue dan"}; !slices.Equal(got, want) {
		t.Errorf("the audit log reads %q", got)
	}
}

// administering is an installation whose first administrator, carol, has enrolled a passkey from the
// link the bootstrap token made her, which ended it, and is signed in in a browser, and whose user
// bob has enrolled one from the link carol made him, and lost it.
func administering(t *testing.T) (ceremonies, *http.Cookie) {
	t.Helper()
	in := someCeremonies(t)
	w := in.enrol(t, newBrowser(), in.user(t, "carol", true), "")
	if w.Code != http.StatusOK {
		t.Fatalf("carol's registration answered %d %s", w.Code, w.Body)
	}
	carol := session(t, w)
	created := in.call(t, "POST", "/api/v1/users", `{"login":"bob"}`, "", carol)
	var bob api.CreatedUser
	if err := json.Unmarshal(created.Body.Bytes(), &bob); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("carol creating bob answered %d %s", created.Code, created.Body)
	}
	if w := in.enrol(t, newBrowser(), codeOf(t, bob.Enrolment.Link), ""); w.Code != http.StatusOK {
		t.Fatalf("bob's registration answered %d %s", w.Code, w.Body)
	}
	return in, carol
}

// A recovery code carol issues from her browser enrols bob a new passkey on the enrolment page, from
// the options it is sent in, spends itself, and signs bob in, the passkey then signing him in again;
// recorded as the code carol issued him, of the kind recovery. A code a fresh one replaced, or one
// spent, opens nothing.
func TestARecoveryCodeEnrolsANewPasskeyAndSignsItsUserIn(t *testing.T) {
	in, carol := administering(t)
	replaced := recoveryOf(t, in.call(t, "POST", "/api/v1/users/bob/recovery", "", "", carol), *in.clock)
	code := recoveryOf(t, in.call(t, "POST", "/api/v1/users/bob/recovery", "", "", carol), *in.clock)
	if w := in.call(t, "POST", "/api/v1/auth/passkey/options", fmt.Sprintf(`{"ceremony":"registration","code":%q}`, replaced.Code), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a recovery code a fresh one replaced answered %d %s", w.Code, w.Body)
	}

	browser := newBrowser()
	w := in.enrol(t, browser, code.Code, "new phone")
	if w.Code != http.StatusOK {
		t.Fatalf("bob's registration with his recovery code answered %d %s", w.Code, w.Body)
	}
	var enrolled api.Verified
	if err := json.Unmarshal(w.Body.Bytes(), &enrolled); err != nil || enrolled.Login != "bob" || enrolled.Credential == nil || enrolled.Credential.Label != "new phone" {
		t.Errorf("the registration answered %s", w.Body)
	}
	session(t, w)
	if n := in.count(t, `select count(*) from credentials where login = 'bob' and type = 'passkey'`); n != 2 {
		t.Errorf("bob holds %d passkeys, the one he lost and the one he enrolled", n)
	}
	if w := in.call(t, "POST", "/api/v1/auth/passkey/options", fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code.Code), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a recovery code spent answered %d %s", w.Code, w.Body)
	}
	if w := in.signIn(t, browser); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"login":"bob"`) {
		t.Errorf("bob's sign-in with the passkey his recovery code enrolled answered %d %s", w.Code, w.Body)
	}

	var kind, by string
	in.query(t, `select detail::jsonb->>'kind', detail::jsonb->>'issued_by' from audit_log where action = 'enrolment.use' and target = 'bob' order by seq desc limit 1`, &kind, &by)
	if kind != "recovery" || by != "carol" {
		t.Errorf("bob's recovery is recorded as a code of the kind %q issued by %q", kind, by)
	}
	got := in.actions(t)
	if !slices.Contains(got, "carol enrolment.issue bob") || !slices.Contains(got, "bob enrolment.use bob") {
		t.Errorf("the audit log reads %q", got)
	}
}

// A recovery code carol issues sets bob a password on the enrolment page where the policy allows
// passwords and asks for no passkey, and it signs him in to a full session; the code is spent.
func TestARecoveryCodeSetsAPasswordAndSignsItsUserIn(t *testing.T) {
	in, carol := administering(t)
	bound := false
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.SetInstallationPolicy(ctx, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2}, *in.clock)
	}); err != nil {
		t.Fatal(err)
	}
	code := recoveryOf(t, in.call(t, "POST", "/api/v1/users/bob/recovery", "", "", carol), *in.clock)
	body := fmt.Sprintf(`{"code":%q,"password":"bob's recovered passphrase"}`, code.Code)
	w := in.call(t, "POST", "/api/v1/auth/password/enrol", body, "")
	var set api.PasswordEnrolled
	if err := json.Unmarshal(w.Body.Bytes(), &set); err != nil || w.Code != http.StatusOK || set.Login != "bob" || set.Session != api.SessionFull {
		t.Fatalf("bob's password from his recovery code answered %d %s", w.Code, w.Body)
	}
	session(t, w)
	if w := in.call(t, "POST", "/api/v1/auth/password/enrol", body, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("a recovery code spent set a password again: %d %s", w.Code, w.Body)
	}
}

// The bootstrap token ends at the enrolment of the first administrator who can sign in, whatever
// code brought them to it: a recovery code the token issued them, for a passkey, or for a password
// that opens a full session, or one that opens a session that may only enrol and the passkey
// registered from that session then; and a recovery code the break-glass path issued them. A
// user's recovery code, spent while it lives, ends nothing.
func TestARecoveryCodeEndsTheBootstrapWhereAnAdministratorEnrolsWithIt(t *testing.T) {
	ended := func(t *testing.T, in ceremonies) bool {
		t.Helper()
		return in.count(t, `select count(*) from bootstrap where enrolled_at is not null and token_hash is null`) == 1
	}
	recovered := func(t *testing.T, in ceremonies, login string) string {
		t.Helper()
		return recoveryOf(t, in.bearer(t, "POST", "/api/v1/users/"+login+"/recovery", in.bootstrap, ""), *in.clock).Code
	}
	password := func(t *testing.T, in ceremonies, code string) *httptest.ResponseRecorder {
		t.Helper()
		w := in.call(t, "POST", "/api/v1/auth/password/enrol", fmt.Sprintf(`{"code":%q,"password":"the first administrator's"}`, code), "")
		if w.Code != http.StatusOK {
			t.Fatalf("the password answered %d %s", w.Code, w.Body)
		}
		return w
	}
	optional := func(t *testing.T, in ceremonies) {
		t.Helper()
		bound := false
		if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.SetInstallationPolicy(ctx, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2}, *in.clock)
		}); err != nil {
			t.Fatal(err)
		}
	}
	endedBy := func(t *testing.T, in ceremonies, login string) {
		t.Helper()
		if !ended(t, in) {
			t.Fatal("the administrator's enrolment left the bootstrap going")
		}
		if w := in.bearer(t, "GET", "/api/v1/users", in.bootstrap, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("the bootstrap token after the administrator enrolled answered %d %s", w.Code, w.Body)
		}
		if got := in.actions(t); !slices.Contains(got, login+" bootstrap.end operator") {
			t.Errorf("the audit log reads %q", got)
		}
	}

	t.Run("a passkey, the password the link set lost", func(t *testing.T) {
		in := someCeremonies(t)
		password(t, in, in.user(t, "alice", true))
		if ended(t, in) {
			t.Fatal("a password that opens a session that may only enrol ended the bootstrap")
		}
		if w := in.enrol(t, newBrowser(), recovered(t, in, "alice"), ""); w.Code != http.StatusOK {
			t.Fatalf("alice's registration with a recovery code answered %d %s", w.Code, w.Body)
		}
		endedBy(t, in, "alice")
	})

	t.Run("a password opening a full session", func(t *testing.T) {
		in := someCeremonies(t)
		optional(t, in)
		in.user(t, "alice", true)
		password(t, in, recovered(t, in, "alice"))
		endedBy(t, in, "alice")
	})

	t.Run("a password, then a passkey from its session", func(t *testing.T) {
		in := someCeremonies(t)
		in.user(t, "alice", true)
		w := password(t, in, recovered(t, in, "alice"))
		if ended(t, in) {
			t.Fatal("a password that opens a session that may only enrol ended the bootstrap")
		}
		c := session(t, w)
		made, _, err := newBrowser().Create(in.options(t, `{"ceremony":"registration"}`, c))
		if err != nil {
			t.Fatal(err)
		}
		if w := in.verify(t, "registration", made, "", "", c); w.Code != http.StatusOK {
			t.Fatalf("the passkey registered from the session answered %d %s", w.Code, w.Body)
		}
		endedBy(t, in, "alice")
	})

	t.Run("the break-glass path", func(t *testing.T) {
		in := someCeremonies(t)
		in.user(t, "alice", true)
		code, err := api.BreakGlass(t.Context(), in.pool, publicOrigin, "alice", *in.clock)
		if err != nil {
			t.Fatal(err)
		}
		if w := in.enrol(t, newBrowser(), code.Code, ""); w.Code != http.StatusOK {
			t.Fatalf("alice's registration with the break-glass code answered %d %s", w.Code, w.Body)
		}
		endedBy(t, in, "alice")
	})

	t.Run("a user's", func(t *testing.T) {
		in := someCeremonies(t)
		in.user(t, "bob", false)
		if w := in.enrol(t, newBrowser(), recovered(t, in, "bob"), ""); w.Code != http.StatusOK {
			t.Fatalf("bob's registration with a recovery code answered %d %s", w.Code, w.Body)
		}
		if ended(t, in) {
			t.Error("a user's recovery ended the bootstrap")
		}
	})
}

// A new user's link enrols the first credential of an account that holds none, and opens nothing
// once the account holds one: dave, issued a recovery code beside the link he was created with,
// enrols a passkey with the code, and the link, which may sit in whatever it was sent through,
// then enrols no passkey and sets no password on his account.
func TestAnEnrolmentLinkOpensNothingOnceARecoveryCodeHasEnrolledItsUser(t *testing.T) {
	in, carol := administering(t)
	created := in.call(t, "POST", "/api/v1/users", `{"login":"dave"}`, "", carol)
	var dave api.CreatedUser
	if err := json.Unmarshal(created.Body.Bytes(), &dave); err != nil || created.Code != http.StatusCreated {
		t.Fatalf("carol creating dave answered %d %s", created.Code, created.Body)
	}
	link := codeOf(t, dave.Enrolment.Link)
	code := recoveryOf(t, in.call(t, "POST", "/api/v1/users/dave/recovery", "", "", carol), *in.clock)
	if w := in.enrol(t, newBrowser(), code.Code, ""); w.Code != http.StatusOK {
		t.Fatalf("dave's registration with his recovery code answered %d %s", w.Code, w.Body)
	}
	if w := in.call(t, "POST", "/api/v1/auth/passkey/options", fmt.Sprintf(`{"ceremony":"registration","code":%q}`, link), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("dave's link, once he held a passkey, started a registration: %d %s", w.Code, w.Body)
	}
	if w := in.call(t, "POST", "/api/v1/auth/password/enrol", fmt.Sprintf(`{"code":%q,"password":"somebody else's passphrase"}`, link), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("dave's link, once he held a passkey, set a password: %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'dave'`); n != 1 {
		t.Errorf("dave holds %d credentials, and he enrolled one", n)
	}
}

// The bootstrap token's end and a recovery code it issues, as it ends, never wait on each other: the
// end revokes no recovery code, and the codes the token issued open nothing from then on, so that
// the first administrator's enrolment, holding the bootstrap state, never waits on a code the
// issue holds while the issue waits on the state. The enrolment ends the token, the issue finds it
// ended and issues nothing, and the code the token issued bob before opens nothing.
//
// The bootstrap state is held until both wait on it, the enrolment first, so that the order does
// not depend on the scheduler.
func TestTheBootstrapsEndAndARecoveryCodeItIssuesTakeTurns(t *testing.T) {
	in := someCeremonies(t)
	in.user(t, "bob", false)
	before := recoveryOf(t, in.bearer(t, "POST", "/api/v1/users/bob/recovery", in.bootstrap, ""), *in.clock)
	code := in.user(t, "alice", true)
	made, _, err := newBrowser().Create(in.options(t, fmt.Sprintf(`{"ceremony":"registration","code":%q}`, code)))
	if err != nil {
		t.Fatal(err)
	}
	holder, err := dbtest.Superuser(t, in.super).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(t.Context(), `select from bootstrap for update`); err != nil {
		t.Fatal(err)
	}
	waiting := func(n int) error {
		for deadline := time.Now().Add(10 * time.Second); ; {
			if in.count(t, `select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`) >= n {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%d transactions were expected to wait on a lock", n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	enrolment, issue := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { enrolment <- in.verify(t, "registration", made, "", "") }()
	err = waiting(1)
	if err == nil {
		go func() { issue <- in.bearer(t, "POST", "/api/v1/users/bob/recovery", in.bootstrap, "") }()
		err = waiting(2)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if w := <-enrolment; w.Code != http.StatusOK {
		t.Errorf("the first administrator's enrolment answered %d %s", w.Code, w.Body)
	}
	if w := <-issue; w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "first administrator enrolled") {
		t.Errorf("the recovery code the bootstrap token asked for as it ended answered %d %s", w.Code, w.Body)
	}
	if w := in.call(t, "POST", "/api/v1/auth/passkey/options", fmt.Sprintf(`{"ceremony":"registration","code":%q}`, before.Code), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the recovery code the bootstrap token issued bob opened a registration once it ended: %d %s", w.Code, w.Body)
	}
}
