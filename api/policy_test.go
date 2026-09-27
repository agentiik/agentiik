package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The authentication policy's routes, over a real PostgreSQL, on the installation passwordsOf is:
// read by whoever is signed in and set whole by an administrator, a setting left out at its default;
// a namespace's tightening read by whoever holds a grant in it and set by an administrator, never
// looser than the installation's; and forbidding passwords, which deletes the passwords it reaches
// as rows and suspends the accounts it reaches that hold no passkey the policy accepts, but never
// every administrator who can sign in, and never on an installation addressed by an IP address.

// token mints principal a token for an hour, narrowed as given, and answers its value.
func (in passwordsOf) token(t *testing.T, principal string, permissions, within []string) string {
	t.Helper()
	value := "agk_test_" + ulid.New()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.MintToken(ctx, db.APIToken{
			ID: ulid.New(), Hash: hashOf(value), Principal: principal, Permissions: permissions, Within: within,
			CreatedAt: in.clock.Add(-time.Minute), ExpiresAt: in.clock.Add(time.Hour),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// administrator makes login an administrator.
func (in passwordsOf) administrator(t *testing.T, login string) {
	t.Helper()
	in.exec(t, `update users set admin = true where login = '`+login+`'`)
}

// bearing sends one request bearing token, as agk does.
func (in passwordsOf) bearing(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return sent(t, in.h, method, path, token, body)
}

// suspended answers the logins suspended, each with why where a reason is recorded, as login or
// login:reason, ordered by login.
func (in passwordsOf) suspended(t *testing.T) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(),
		`select login || coalesce(':' || suspended_for, '') from users where suspended order by login`)
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

// changes are the policy.change entries of the audit log, each as actor target namespace and its
// detail, in order.
func (in passwordsOf) changes(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(),
		`select actor, target, coalesce(namespace, ''), detail from audit_log where action = 'policy.change' order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []map[string]any
	for rows.Next() {
		var actor, target, namespace, detail string
		if err := rows.Scan(&actor, &target, &namespace, &detail); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(detail), &d); err != nil {
			t.Fatal(err)
		}
		d["_actor"], d["_target"], d["_namespace"] = actor, target, namespace
		all = append(all, d)
	}
	return all
}

const theDefaults = `{"password":"allowed","passkey":"required","user_verification":"required","device_bound_only":false,"min_passkeys":2}`

// The installation's policy is read, every setting written, by whoever is signed in, and not by a
// session that may only enrol; an administrator sets it whole, a setting left out returning to its
// default, and each change is recorded as policy.change with the policy as it was and as it stands.
// What the schema refuses is refused, and changes nothing.
func TestThePolicyIsReadByWhoeverIsSignedInAndSetWholeByAnAdministrator(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	alice, carol := in.token(t, "alice", nil, nil), in.token(t, "carol", nil, nil)
	narrowed := in.token(t, "alice", []string{"run:read"}, nil)

	for who, token := range map[string]string{"alice": alice, "a narrowed token": narrowed} {
		if w := in.bearing(t, "GET", "/api/v1/auth/policy", token, ""); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != theDefaults {
			t.Errorf("%s reading the policy answered %d %s", who, w.Code, w.Body)
		}
	}
	if w := in.bearing(t, "GET", "/api/v1/auth/policy", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("nobody reading the policy answered %d %s", w.Code, w.Body)
	}
	enrolling := in.signedIn(t, "alice", api.SessionEnrolment)
	if w := in.call(t, "GET", "/api/v1/auth/policy", "", enrolling); w.Code != http.StatusForbidden {
		t.Errorf("a session that may only enrol reading the policy answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", alice, `{"passkey":"optional"}`); w.Code != http.StatusForbidden {
		t.Errorf("alice, who administers nothing, setting the policy answered %d %s", w.Code, w.Body)
	}

	w := in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{"passkey":"optional","min_passkeys":3}`)
	set := `{"password":"allowed","passkey":"optional","user_verification":"required","device_bound_only":false,"min_passkeys":3}`
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != set {
		t.Fatalf("carol setting the policy answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "GET", "/api/v1/auth/policy", alice, ""); strings.TrimSpace(w.Body.String()) != set {
		t.Errorf("the policy reads back %s", w.Body)
	}
	if n := in.count(t, `select count(*) from auth_policy where namespace is null and password = 'allowed' and passkey = 'optional'
	                       and user_verification = 'required' and not device_bound_only and min_passkeys = 3`); n != 1 {
		t.Error("the policy is not stored whole as it was set")
	}
	changes := in.changes(t)
	if len(changes) != 1 || changes[0]["_actor"] != "carol" || changes[0]["_target"] != "installation" || changes[0]["_namespace"] != "" {
		t.Fatalf("the changes recorded are %v", changes)
	}
	if was, _ := json.Marshal(changes[0]["was"]); string(was) != `{"device_bound_only":false,"min_passkeys":2,"passkey":"required","password":"allowed","user_verification":"required"}` {
		t.Errorf("the change records the policy as it was as %s", was)
	}
	if _, deleted := changes[0]["passwords_deleted"]; deleted {
		t.Error("a change forbidding nothing records passwords deleted")
	}

	for _, body := range []string{
		`{"password":"maybe"}`, `{"passkey":"sometimes"}`, `{"user_verification":"never"}`, `{"device_bound_only":"yes"}`,
		`{"min_passkeys":0}`, `{"min_passkeys":2147483648}`, `{"min_passkeys":1.5}`, `{"password":null}`, `{"colour":"blue"}`,
		`null`, ``, `{"password":"allowed","password":"forbidden"}`,
	} {
		if w := in.bearing(t, "PUT", "/api/v1/auth/policy", carol, body); w.Code != http.StatusBadRequest {
			t.Errorf("setting the policy to %s answered %d %s", body, w.Code, w.Body)
		}
	}
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{}`); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != theDefaults {
		t.Errorf("an empty policy answered %d %s, and it is every setting at its default", w.Code, w.Body)
	}
	if n := len(in.changes(t)); n != 2 {
		t.Errorf("%d changes are recorded, and the refusals changed nothing", n)
	}
}

// A policy set through the route applies from the next request: a password's full session is
// confined once a passkey is required, and freed once passkeys are optional again.
func TestAPolicySetThroughTheRouteAppliesAtTheNextRequest(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	carol := in.token(t, "carol", nil, nil)
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{"passkey":"optional"}`); w.Code != http.StatusOK {
		t.Fatalf("setting the policy answered %d %s", w.Code, w.Body)
	}
	c := in.signedIn(t, "alice", api.SessionFull)
	if code, _ := in.me(t, c); code != http.StatusOK {
		t.Fatalf("alice's session answered %d", code)
	}
	in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{}`)
	if code, _ := in.me(t, c); code != http.StatusForbidden {
		t.Errorf("with a passkey required, alice's session answered %d", code)
	}
	in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{"passkey":"optional"}`)
	if code, _ := in.me(t, c); code != http.StatusOK {
		t.Errorf("with passkeys optional again, alice's session answered %d", code)
	}
}

// Forbidding passwords deletes every password and every TOTP generator as rows, in the transaction
// that forbids them, which ends the sessions they opened; suspends every account holding no passkey
// the policy accepts, recording why, and nobody holding one; and the password sign-in refuses
// outright from then on. It is done when passwords come to be forbidden, and a change that keeps
// them forbidden suspends nobody created since.
func TestForbiddingPasswordsDeletesThemAndSuspendsWhoHoldsNoPasskey(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	carol := in.token(t, "carol", nil, nil)
	in.policy(t, "allowed", "optional")
	alice := in.signedIn(t, "alice", api.SessionFull)
	bobs := in.token(t, "bob", nil, nil)

	w := in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{"password":"forbidden"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"password":"forbidden"`) {
		t.Fatalf("forbidding passwords answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where type in ('password', 'totp')`); n != 0 {
		t.Errorf("%d passwords and generators are left", n)
	}
	if got := in.suspended(t); !slices.Equal(got, []string{"alice:no_passkey", "bob:no_passkey", "dave", "erin:no_passkey"}) {
		t.Errorf("the suspended accounts are %q", got)
	}
	// An administrator reads why, where the policy suspended the account, and nothing where it
	// did not.
	for login, want := range map[string]string{"alice": "no_passkey", "dave": ""} {
		w := in.bearing(t, "GET", "/api/v1/users/"+login, carol, "")
		var user map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil || w.Code != http.StatusOK || user["suspended"] != true {
			t.Fatalf("%s is answered %d %s", login, w.Code, w.Body)
		}
		if got, _ := user["suspended_for"].(string); got != want || (want == "") == (user["suspended_for"] != nil) {
			t.Errorf("%s is answered suspended for %v, want %q", login, user["suspended_for"], want)
		}
	}
	if code, _ := in.me(t, alice); code != http.StatusUnauthorized {
		t.Errorf("the session alice's password opened answered %d", code)
	}
	if w := in.bearing(t, "GET", "/api/v1/me", bobs, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("bob's token, bob suspended, answered %d %s", w.Code, w.Body)
	}
	if w := in.as(t, "alice", ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Errorf("alice's password answered %d %s", w.Code, w.Body)
	}
	changes := in.changes(t)
	if len(changes) != 1 {
		t.Fatalf("the changes recorded are %v", changes)
	}
	deleted, _ := json.Marshal(changes[0]["passwords_deleted"])
	suspended, _ := json.Marshal(changes[0]["suspended"])
	if string(deleted) != `["alice","bob","carol","dave"]` || string(suspended) != `["alice","bob","erin"]` {
		t.Errorf("the change records passwords deleted %s and accounts suspended %s", deleted, suspended)
	}

	// frank, created since, holds nothing, and a change keeping passwords forbidden leaves him be.
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "frank", DisplayName: "Frank"})
	}); err != nil {
		t.Fatal(err)
	}
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{"password":"forbidden","min_passkeys":1}`); w.Code != http.StatusOK {
		t.Fatalf("changing the policy answered %d %s", w.Code, w.Body)
	}
	if got := in.suspended(t); slices.Contains(got, "frank:no_passkey") {
		t.Errorf("a change keeping passwords forbidden suspended frank: %q", got)
	}
}

// A synced passkey is no passkey the policy accepts where device_bound_only comes with passwords
// forbidden: its holder is suspended, and one holding a device-bound passkey is not.
func TestForbiddingPasswordsCountsThePasskeysThePolicyAccepts(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	in.passkeyed(t, "alice", "alice-synced", true)
	in.passkeyed(t, "bob", "bob-bound", false)
	w := in.bearing(t, "PUT", "/api/v1/auth/policy", in.token(t, "carol", nil, nil), `{"password":"forbidden","device_bound_only":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("forbidding passwords answered %d %s", w.Code, w.Body)
	}
	if got := in.suspended(t); !slices.Equal(got, []string{"alice:no_passkey", "dave", "erin:no_passkey"}) {
		t.Errorf("the suspended accounts are %q", got)
	}
}

// Forbidding passwords is refused, naming the setting and changing nothing, where it would suspend
// every administrator who can sign in once the bootstrap token has ended; while it lasts, or while
// another administrator holds a passkey, it is not.
func TestForbiddingPasswordsLeavesAnAdministratorWhoCanSignIn(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "alice")
	alice := in.token(t, "alice", nil, nil)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, *in.clock)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w := in.bearing(t, "PUT", "/api/v1/auth/policy", alice, `{"password":"forbidden"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"password"`) || !strings.Contains(w.Body.String(), "no administrator able to sign in") {
		t.Fatalf("forbidding the one administrator's password answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where type = 'password'`); n != 4 {
		t.Errorf("%d passwords are left of four", n)
	}
	if got := in.suspended(t); !slices.Equal(got, []string{"dave"}) {
		t.Errorf("the suspended accounts are %q", got)
	}
	if w := in.bearing(t, "GET", "/api/v1/auth/policy", alice, ""); strings.TrimSpace(w.Body.String()) != theDefaults {
		t.Errorf("the policy reads %s", w.Body)
	}

	in.administrator(t, "carol")
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", alice, `{"password":"forbidden"}`); w.Code != http.StatusOK {
		t.Errorf("with carol holding a passkey, forbidding passwords answered %d %s", w.Code, w.Body)
	}
}

// While the bootstrap token lasts, it is what administers the installation, and forbidding passwords
// is not refused for suspending the administrators, who come back by enrolling.
func TestForbiddingPasswordsWhileTheBootstrapLastsSuspendsTheAdministrators(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "alice")
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", in.token(t, "alice", nil, nil), `{"password":"forbidden"}`); w.Code != http.StatusOK {
		t.Fatalf("forbidding passwords answered %d %s", w.Code, w.Body)
	}
	if got := in.suspended(t); !slices.Contains(got, "alice:no_passkey") {
		t.Errorf("the suspended accounts are %q", got)
	}
}

// On an installation addressed by an IP address, passwords are the one way in: a policy forbidding
// them is refused naming the setting, the installation's and a namespace's alike, and anything else
// is stored as written.
func TestPasswordsAreNotForbiddenWhereTheInstallationIsAddressedByAnIPAddress(t *testing.T) {
	in := passwordsAt(t, "https://192.0.2.10", false)
	in.administrator(t, "carol")
	carol := in.token(t, "carol", nil, nil)
	for _, path := range []string{"/api/v1/auth/policy", "/api/v1/finance/auth/policy"} {
		if w := in.bearing(t, "PUT", path, carol, `{"password":"forbidden"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"password"`) {
			t.Errorf("forbidding passwords at %s answered %d %s", path, w.Code, w.Body)
		}
	}
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", carol, `{"device_bound_only":true}`); w.Code != http.StatusOK {
		t.Errorf("setting device_bound_only answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where type = 'password'`); n != 4 {
		t.Errorf("%d passwords are left of four", n)
	}
}

// A namespace's tightening is read by an administrator and by whoever holds a grant in it, as its
// own settings alone, and by nobody else; it is set by an administrator alone, refused before the
// namespace is looked up to anybody else, recorded in the namespace, and removed by an empty object.
func TestANamespacesPolicyIsReadByWhoHoldsAGrantInItAndSetByAnAdministrator(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	alice, bob, carol := in.token(t, "alice", nil, nil), in.token(t, "bob", nil, nil), in.token(t, "carol", nil, nil)

	for who, token := range map[string]string{"alice": alice, "carol": carol} {
		if w := in.bearing(t, "GET", "/api/v1/finance/auth/policy", token, ""); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{}` {
			t.Errorf("%s reading finance's policy answered %d %s", who, w.Code, w.Body)
		}
	}
	if w := in.bearing(t, "GET", "/api/v1/finance/auth/policy", bob, ""); w.Code != http.StatusNotFound {
		t.Errorf("bob, holding nothing in finance, answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "GET", "/api/v1/nowhere/auth/policy", carol, ""); w.Code != http.StatusNotFound {
		t.Errorf("a namespace nobody made answered %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/api/v1/finance/auth/policy", "/api/v1/nowhere/auth/policy"} {
		if w := in.bearing(t, "PUT", path, alice, `{"min_passkeys":3}`); w.Code != http.StatusForbidden {
			t.Errorf("alice setting %s answered %d %s", path, w.Code, w.Body)
		}
	}
	if w := in.bearing(t, "PUT", "/api/v1/nowhere/auth/policy", carol, `{"min_passkeys":3}`); w.Code != http.StatusNotFound {
		t.Errorf("carol setting a namespace nobody made answered %d %s", w.Code, w.Body)
	}

	set := `{"device_bound_only":true,"min_passkeys":3}`
	if w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", carol, set); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != set {
		t.Fatalf("carol setting finance's policy answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "GET", "/api/v1/finance/auth/policy", alice, ""); strings.TrimSpace(w.Body.String()) != set {
		t.Errorf("finance's policy reads back %s", w.Body)
	}
	if changes := in.changes(t); len(changes) != 1 || changes[0]["_actor"] != "carol" || changes[0]["_target"] != "finance" || changes[0]["_namespace"] != "finance" {
		t.Errorf("the changes recorded are %v", changes)
	}
	if w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", carol, `{}`); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{}` {
		t.Errorf("an empty policy answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from auth_policy where namespace = 'finance'`); n != 0 {
		t.Error("finance's policy is kept once it tightens nothing")
	}
}

// A namespace may tighten the installation's policy, never loosen it: each setting looser than the
// installation's is refused naming it, against the installation's as it stands; one as strict is
// taken.
func TestANamespaceTightensThePolicyAndNeverLoosensIt(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	carol := in.token(t, "carol", nil, nil)
	bound := true
	in.setPolicy(t, db.AuthPolicy{Password: "forbidden", Passkey: "required", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 3})
	for body, setting := range map[string]string{
		`{"password":"allowed"}`:            "password",
		`{"passkey":"optional"}`:            "passkey",
		`{"user_verification":"preferred"}`: "user_verification",
		`{"device_bound_only":false}`:       "device_bound_only",
		`{"min_passkeys":2}`:                "min_passkeys",
	} {
		w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", carol, body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"`+setting+`"`) || !strings.Contains(w.Body.String(), "never loosen it") {
			t.Errorf("finance asking %s answered %d %s", body, w.Code, w.Body)
		}
	}
	if w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", carol, `{"password":"forbidden","min_passkeys":3,"device_bound_only":true}`); w.Code != http.StatusOK {
		t.Errorf("finance asking as much as the installation answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", carol, `{"min_passkeys":4}`); w.Code != http.StatusOK {
		t.Errorf("finance asking more answered %d %s", w.Code, w.Body)
	}
}

// A namespace coming to forbid passwords takes them, and the TOTP generators beside them, from the
// accounts holding a grant carrying a role in it, their own or a group's, and suspends those holding
// no passkey; an account whose grant there has expired, one holding only a deny there, and one
// holding nothing there keep theirs. What it did is recorded in the namespace.
func TestANamespaceForbiddingPasswordsTakesThemFromWhoHoldsAGrantInIt(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	gone := in.clock.Add(-time.Minute)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateGroup(ctx, "auditors"); err != nil {
			return err
		}
		_, err := w.AddMember(ctx, "auditors", "bob")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		for _, g := range []access.Grant{
			{Principal: "group:auditors", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer},
			{Principal: "dave", Scope: access.Scope{Namespace: "finance"}, Deny: access.RunReadData},
			{Principal: "erin", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, ExpiresAt: &gone},
		} {
			g.ID, g.GrantedBy = ulid.New(), "carol"
			if err := n.GrantAccess(ctx, g); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", in.token(t, "carol", nil, nil), `{"password":"forbidden"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("finance forbidding passwords answered %d %s", w.Code, w.Body)
	}
	for login, held := range map[string]int{"alice": 0, "bob": 0, "carol": 1, "dave": 1} {
		if n := in.count(t, `select count(*) from credentials where login = $1 and type = 'password'`, login); n != held {
			t.Errorf("%s holds %d passwords, and holds %d", login, n, held)
		}
	}
	if n := in.count(t, `select count(*) from credentials where type = 'totp'`); n != 0 {
		t.Error("bob's generator outlived his password")
	}
	if got := in.suspended(t); !slices.Equal(got, []string{"alice:no_passkey", "bob:no_passkey", "dave"}) {
		t.Errorf("the suspended accounts are %q", got)
	}
	changes := in.changes(t)
	deleted, _ := json.Marshal(changes[0]["passwords_deleted"])
	if len(changes) != 1 || changes[0]["_namespace"] != "finance" || string(deleted) != `["alice","bob"]` {
		t.Errorf("the change is recorded as %v", changes)
	}

	// carol, granted a role in finance since, is left as she is by a change that keeps
	// passwords forbidden there: it is their coming to be forbidden that takes them.
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "carol", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "carol"})
	}); err != nil {
		t.Fatal(err)
	}
	if w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", in.token(t, "carol", nil, nil), `{"password":"forbidden","min_passkeys":3}`); w.Code != http.StatusOK {
		t.Fatalf("finance keeping passwords forbidden answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "carol") == "" {
		t.Error("a change keeping passwords forbidden took carol's password")
	}
}

// endBootstrap ends the bootstrap token, as the first administrator's enrolment does.
func (in passwordsOf) endBootstrap(t *testing.T) {
	t.Helper()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, *in.clock)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Refusing synced passkeys takes a way in from whoever holds nothing else: where it would leave no
// administrator able to sign in once the bootstrap token has ended, the installation's policy and a
// namespace's are refused naming device_bound_only, and changed not at all, forbidding passwords
// beside it included, since the passwords erin does not hold are not what she loses; forbidding them
// alone takes nothing from her. Beside an administrator holding a device-bound passkey, neither is
// refused.
func TestRefusingEveryAdministratorsSyncedPasskeyIsRefused(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "erin")
	in.passkeyed(t, "erin", "erin-synced", true)
	in.endBootstrap(t)
	erin := in.token(t, "erin", nil, nil)
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "erin", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "carol"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, body string }{
		{"/api/v1/auth/policy", `{"device_bound_only":true}`},
		{"/api/v1/finance/auth/policy", `{"device_bound_only":true}`},
		{"/api/v1/auth/policy", `{"password":"forbidden","device_bound_only":true}`},
		{"/api/v1/finance/auth/policy", `{"password":"forbidden","device_bound_only":true}`},
	} {
		w := in.bearing(t, "PUT", c.path, erin, c.body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"device_bound_only"`) || !strings.Contains(w.Body.String(), "no administrator able to sign in") {
			t.Errorf("%s at %s, erin's one passkey synced, answered %d %s", c.body, c.path, w.Code, w.Body)
		}
	}
	if n := in.count(t, `select count(*) from auth_policy where device_bound_only or password = 'forbidden'`); n != 0 {
		t.Error("a policy refused was written")
	}
	if n := in.count(t, `select count(*) from credentials where type = 'password'`); n != 4 {
		t.Errorf("%d passwords are left of four after the refusals", n)
	}
	if w := in.bearing(t, "PUT", "/api/v1/finance/auth/policy", erin, `{"password":"forbidden"}`); w.Code != http.StatusOK {
		t.Errorf("forbidding passwords in finance, which takes nothing from erin, answered %d %s", w.Code, w.Body)
	}
	in.administrator(t, "carol")
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", erin, `{"device_bound_only":true}`); w.Code != http.StatusOK {
		t.Errorf("beside carol, refusing synced passkeys answered %d %s", w.Code, w.Body)
	}
}

// A role, or a group holding one, brings who it reaches under its namespace's policy, so that giving
// the one administrator who can sign in a role where their way in is refused, or putting them in a
// group holding one there, is a 409 naming the setting once the bootstrap token has ended, and
// writes nothing: hr forbids passwords, which alice alone holds, and ops refuses synced passkeys,
// erin's only one. A deny brings nobody under a policy, nor does a role given somebody who
// administers nothing; and beside carol, who holds a passkey, neither act is refused.
func TestAGrantOrAMembershipLeavingNoAdministratorAbleToSignInIsRefused(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "alice")
	in.endBootstrap(t)
	alice := in.token(t, "alice", nil, nil)
	bound := true
	in.exec(t, `insert into namespaces (name) values ('hr'), ('ops')`)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.SetNamespacePolicy(ctx, "hr", db.AuthPolicy{Password: "forbidden"}, *in.clock); err != nil {
			return err
		}
		if err := w.SetNamespacePolicy(ctx, "ops", db.AuthPolicy{DeviceBoundOnly: &bound}, *in.clock); err != nil {
			return err
		}
		for _, group := range []string{"hr-team", "ops-team"} {
			if err := w.CreateGroup(ctx, group); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for namespace, group := range map[string]string{"hr": "group:hr-team", "ops": "group:ops-team"} {
		if err := in.pool.In(t.Context(), namespace, func(ctx context.Context, n *db.NS) error {
			return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: group, Scope: access.Scope{Namespace: namespace}, Role: access.Viewer, GrantedBy: "carol"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	grants := func() int {
		t.Helper()
		return in.count(t, `select count(*) from grants where namespace in ('hr', 'ops') and principal in ('alice', 'erin')`)
	}
	refused := func(w *httptest.ResponseRecorder, setting, what string) {
		t.Helper()
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"`+setting+`"`) || !strings.Contains(w.Body.String(), "no administrator able to sign in") {
			t.Errorf("%s answered %d %s", what, w.Code, w.Body)
		}
	}

	refused(in.bearing(t, "POST", "/api/v1/hr/grants", alice, `{"principal":"alice","role":"viewer"}`), "password", "a role for alice in hr")
	refused(in.bearing(t, "PUT", "/api/v1/groups/hr-team/members/alice", alice, ""), "password", "alice put in hr-team")
	if n := grants(); n != 0 {
		t.Errorf("%d grants refused were written", n)
	}
	if n := in.count(t, `select count(*) from group_members where login = 'alice'`); n != 0 {
		t.Error("alice was put in hr-team")
	}
	if n := in.count(t, `select count(*) from audit_log where action in ('grant.create', 'group_member.add')`); n != 0 {
		t.Errorf("%d acts refused were recorded", n)
	}
	if w := in.bearing(t, "POST", "/api/v1/hr/grants", alice, `{"principal":"alice","deny":"run:read_data"}`); w.Code != http.StatusCreated {
		t.Errorf("a deny for alice in hr answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "POST", "/api/v1/hr/grants", alice, `{"principal":"bob","role":"viewer"}`); w.Code != http.StatusCreated {
		t.Errorf("a role for bob, who administers nothing, answered %d %s", w.Code, w.Body)
	}

	in.administrator(t, "erin")
	in.passkeyed(t, "erin", "erin-synced", true)
	in.exec(t, `update users set admin = false where login = 'alice'`)
	erin := in.token(t, "erin", nil, nil)
	refused(in.bearing(t, "POST", "/api/v1/ops/grants", erin, `{"principal":"erin","role":"viewer"}`), "device_bound_only", "a role for erin in ops")
	refused(in.bearing(t, "PUT", "/api/v1/groups/ops-team/members/erin", erin, ""), "device_bound_only", "erin put in ops-team")
	if n := grants(); n != 1 {
		t.Errorf("%d grants are written, the deny alone expected", n)
	}

	in.exec(t, `update users set admin = true where login = 'alice'`)
	in.administrator(t, "carol")
	if w := in.bearing(t, "POST", "/api/v1/hr/grants", alice, `{"principal":"alice","role":"viewer"}`); w.Code != http.StatusCreated {
		t.Errorf("beside carol, a role for alice in hr answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "PUT", "/api/v1/groups/ops-team/members/erin", alice, ""); w.Code != http.StatusOK {
		t.Errorf("beside carol, erin put in ops-team answered %d %s", w.Code, w.Body)
	}
}

// The setting a refusal names is the one the act took the way in by: alice's synced passkey was
// refused before the grant, by the installation's device_bound_only, and her password is what a role
// in hr, forbidding passwords, takes, so the grant's 409 names password.
func TestALockoutNamesTheSettingTheActTookTheWayInBy(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "alice")
	in.passkeyed(t, "alice", "alice-synced", true)
	in.endBootstrap(t)
	bound := true
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2})
	in.exec(t, `insert into namespaces (name) values ('hr')`, `insert into auth_policy (namespace, password) values ('hr', 'forbidden')`)
	w := in.bearing(t, "POST", "/api/v1/hr/grants", in.token(t, "alice", nil, nil), `{"principal":"alice","role":"viewer"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Errorf("a role for alice in hr answered %d %s", w.Code, w.Body)
	}
}

// On an installation addressed by an IP address passwords are allowed whatever a namespace's policy
// says, so a role there takes nobody's password: the one administrator, holding a password alone, is
// given one in a namespace whose stored policy forbids them.
func TestAGrantTakesNoPasswordWhereTheInstallationIsAddressedByAnIPAddress(t *testing.T) {
	in := passwordsAt(t, "https://192.0.2.10", false)
	in.administrator(t, "alice")
	in.endBootstrap(t)
	in.exec(t, `insert into namespaces (name) values ('hr')`)
	in.exec(t, `insert into auth_policy (namespace, password) values ('hr', 'forbidden')`)
	if w := in.bearing(t, "POST", "/api/v1/hr/grants", in.token(t, "alice", nil, nil), `{"principal":"alice","role":"viewer"}`); w.Code != http.StatusCreated {
		t.Errorf("a role for alice in hr answered %d %s", w.Code, w.Body)
	}
}

// A change is not refused for taking a way in from administrators where none had one to take: erin,
// the one administrator, holds nothing but a token.
func TestAPolicyChangeWhereNoAdministratorCouldSignInIsNotRefused(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "erin")
	in.endBootstrap(t)
	if w := in.bearing(t, "PUT", "/api/v1/auth/policy", in.token(t, "erin", nil, nil), `{"device_bound_only":true}`); w.Code != http.StatusOK {
		t.Errorf("a change where no administrator could sign in answered %d %s", w.Code, w.Body)
	}
}

// On an installation addressed by an IP address no passkey signs anybody in, so an administrator
// holding a passkey and no password is no administrator who can sign in: the one holding a password
// is not removed beside them.
func TestAnAdministratorsPasskeySignsNobodyInWhereTheInstallationIsAddressedByAnIPAddress(t *testing.T) {
	in := passwordsAt(t, "https://192.0.2.10", false)
	in.administrator(t, "carol")
	in.administrator(t, "erin")
	in.passkeyed(t, "erin", "erin-passkey", false)
	in.endBootstrap(t)
	if w := in.bearing(t, "DELETE", "/api/v1/users/carol", in.token(t, "erin", nil, nil), ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "last administrator who can sign in") {
		t.Errorf("removing carol beside erin, holding a passkey alone, answered %d %s", w.Code, w.Body)
	}
}

// The last administrator who can sign in is counted as the policy that applies to them says: an
// administrator holding only a synced passkey where device_bound_only applies signs nobody in, and
// the one other administrator is not removed beside them.
func TestRemovingAnAdministratorCountsWhomTheirPolicyLetsSignIn(t *testing.T) {
	in := somePasswords(t)
	in.administrator(t, "carol")
	in.administrator(t, "erin")
	in.passkeyed(t, "erin", "erin-synced", true)
	in.endBootstrap(t)
	bound := true
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 1})
	carol := in.token(t, "carol", nil, nil)
	if w := in.bearing(t, "DELETE", "/api/v1/users/carol", carol, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "last administrator who can sign in") {
		t.Errorf("removing carol beside erin, whose one passkey is synced, answered %d %s", w.Code, w.Body)
	}
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", MinPasskeys: 1})
	if w := in.bearing(t, "DELETE", "/api/v1/users/carol", carol, ""); w.Code != http.StatusNoContent {
		t.Errorf("removing carol beside erin, synced passkeys accepted, answered %d %s", w.Code, w.Body)
	}
}

// A password an account came to hold beside a namespace forbidding passwords, by a grant given it
// since, is refused at the sign-in that offers it and goes then, recorded as the installation's act,
// rather than signing in again once the grant has ended.
func TestAPasswordASignInFindsForbiddenGoes(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	in.tighten(t, db.AuthPolicy{Password: "forbidden"})
	if err := in.pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "carol", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "alice"})
	}); err != nil {
		t.Fatal(err)
	}
	if in.hashHeld(t, "carol") == "" {
		t.Fatal("carol's password went with the grant, which this test does not ask of it")
	}
	if w := in.as(t, "carol", ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"setting":"password"`) {
		t.Fatalf("carol's password answered %d %s", w.Code, w.Body)
	}
	if in.hashHeld(t, "carol") != "" {
		t.Error("carol's password, forbidden, outlived the sign-in that found it")
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.remove' and actor = 'installation'
	                       and target = 'carol-password' and detail::jsonb->>'login' = 'carol'`); n != 1 {
		t.Error("the password's removal is not recorded")
	}
	in.exec(t, `delete from grants where principal = 'carol'`)
	if w := in.as(t, "carol", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("carol's password, once her grant ended, answered %d %s", w.Code, w.Body)
	}
}
