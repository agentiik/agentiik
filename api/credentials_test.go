package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// The caller's credentials, over a real PostgreSQL: listed with what describes each and nothing any
// of them proves anything with; removed one by one as the policy allows, the minimum counted in the
// passkeys the policy accepts, never the last one, the password never where no passkey signs anybody
// in, and a TOTP generator only with a code it shows; each removal ending the sessions it opened and
// recorded as credential.remove.

// removing asks to remove id with the session c carries, from the sign-in page.
func (in passwordsOf) removing(t *testing.T, id string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return in.call(t, "DELETE", "/api/v1/me/credentials/"+id, "", c)
}

// A user's credentials are listed oldest first as $defs/credential writes them: a passkey with its
// kind and both its flags, a password and a generator by the identifier the engine minted; never a
// public key, a hash or a secret. A service account holds none, a token narrowed by a scope reaches
// none, and a session that may only enrol is refused them as everything else.
func TestTheCallersCredentialsAreListedWithoutAnythingTheyProve(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	in.passkeyed(t, "bob", "bob-bound", false)
	c := in.passkeyed(t, "bob", "bob-synced", true)
	in.exec(t, `update credentials set label = 'phone', backup_state = true where id = 'bob-synced'`,
		`update credentials set created_at = now() + interval '1 second' * (case type when 'password' then 1 when 'totp' then 2 else 3 end)
		  + (case id when 'bob-synced' then interval '1 second' else interval '0' end) where login = 'bob'`)

	w := in.call(t, "GET", "/api/v1/me/credentials", "", c)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("listing bob's credentials answered %d %s, Cache-Control %q", w.Code, w.Body, w.Header().Get("Cache-Control"))
	}
	var listed struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range listed.Credentials {
		got = append(got, c["type"].(string)+" "+c["id"].(string))
		for name := range c {
			if !slices.Contains([]string{"type", "id", "label", "backup_eligible", "backup_state", "kind", "created_at", "last_used_at"}, name) {
				t.Errorf("a credential is listed with %s", name)
			}
		}
	}
	if !slices.Equal(got, []string{"password bob-password", "totp bob-totp", "passkey bob-bound", "passkey bob-synced"}) {
		t.Errorf("bob's credentials are listed as %q", got)
	}
	if synced := listed.Credentials[3]; synced["kind"] != "synced" || synced["backup_eligible"] != true || synced["backup_state"] != true || synced["label"] != "phone" {
		t.Errorf("bob's synced passkey is listed as %v", synced)
	}
	if bound := listed.Credentials[2]; bound["kind"] != "device-bound" || bound["backup_eligible"] != false {
		t.Errorf("bob's device-bound passkey is listed as %v", bound)
	}
	for _, secret := range []string{"argon2id", "public_key", "sealed", "password_hash"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("the listing carries %s: %s", secret, w.Body)
		}
	}

	if w := in.bearing(t, "GET", "/api/v1/me/credentials", in.token(t, "bob", nil, nil), ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "bob-bound") {
		t.Errorf("bob's token listing his credentials answered %d %s", w.Code, w.Body)
	}
	if w := in.bearing(t, "GET", "/api/v1/me/credentials", in.token(t, "bob", []string{"run:read"}, nil), ""); w.Code != http.StatusForbidden {
		t.Errorf("a narrowed token listing credentials answered %d %s", w.Code, w.Body)
	}
	in.exec(t, `insert into namespaces (name) values ('ops')`)
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.CreateServiceAccount(ctx, db.ServiceAccount{Namespace: "ops", Name: "nightly", CreatedBy: "carol"})
	}); err != nil {
		t.Fatal(err)
	}
	if w := in.bearing(t, "GET", "/api/v1/me/credentials", in.token(t, "ops/nightly", nil, nil), ""); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"credentials":[]}` {
		t.Errorf("a service account listing its credentials answered %d %s", w.Code, w.Body)
	}
	in.policy(t, "allowed", "required")
	enrolling := in.signedIn(t, "alice", api.SessionEnrolment)
	if w := in.call(t, "GET", "/api/v1/me/credentials", "", enrolling); w.Code != http.StatusForbidden {
		t.Errorf("a session that may only enrol listing credentials answered %d %s", w.Code, w.Body)
	}
}

// A passkey the policy accepts is removed while min_passkeys remain, and not one past that, which is
// a 409 naming the setting; the sessions a removed passkey opened end with it, and the removal is
// recorded. The edges: three of two leave two, two of two leave none removable, and a namespace
// asking for more holds those with a grant in it to its number.
func TestAPasskeyIsRemovedWhileMinPasskeysRemain(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	c := in.passkeyed(t, "erin", "erin-1", false)
	in.passkeyed(t, "erin", "erin-2", false)
	opened := in.passkeyed(t, "erin", "erin-3", false)

	if w := in.removing(t, "erin-3", c); w.Code != http.StatusNoContent {
		t.Fatalf("removing one of three passkeys answered %d %s", w.Code, w.Body)
	}
	if code, _ := in.me(t, opened); code != http.StatusUnauthorized {
		t.Errorf("the session the removed passkey opened answered %d", code)
	}
	if code, _ := in.me(t, c); code != http.StatusOK {
		t.Errorf("the session the removal came from answered %d", code)
	}
	if got := in.trail(t); got[len(got)-1] != "erin credential.remove erin-3" {
		t.Errorf("the audit log ends %q", got[len(got)-1])
	}
	if n := in.count(t, `select count(*) from audit_log where action = 'credential.remove' and target = 'erin-3'
	                       and detail::jsonb->>'type' = 'passkey' and detail::jsonb->>'kind' = 'device-bound'`); n != 1 {
		t.Error("the removal does not record what kind of passkey went")
	}
	w := in.removing(t, "erin-2", c)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"setting":"min_passkeys"`) ||
		!strings.Contains(w.Body.String(), "removing this passkey would leave this account with 1 passkey the policy accepts, and min_passkeys is 2") {
		t.Errorf("removing one of two passkeys answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'erin'`); n != 2 {
		t.Errorf("erin holds %d credentials after a refusal", n)
	}

	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", MinPasskeys: 1})
	in.tighten(t, db.AuthPolicy{MinPasskeys: 3})
	alice := in.passkeyed(t, "alice", "alice-1", false)
	in.passkeyed(t, "alice", "alice-2", false)
	in.passkeyed(t, "alice", "alice-3", false)
	if w := in.removing(t, "alice-1", alice); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "min_passkeys is 3") {
		t.Errorf("alice, holding a grant in finance, removing one of three passkeys answered %d %s", w.Code, w.Body)
	}
	if w := in.removing(t, "erin-2", c); w.Code != http.StatusNoContent {
		t.Errorf("erin, holding nothing in finance, removing one of two passkeys where min_passkeys is 1 answered %d %s", w.Code, w.Body)
	}
	if w := in.removing(t, "erin-1", c); w.Code != http.StatusConflict {
		t.Errorf("erin removing her last passkey answered %d %s", w.Code, w.Body)
	}
}

// A passkey the policy does not accept, a synced one where device_bound_only applies, counts for
// nothing: removing it takes nothing from the minimum; and the session such a passkey opened opens
// nothing once the policy applies.
func TestASyncedPasskeyCountsForNothingWhereOnlyDeviceBoundOnesDo(t *testing.T) {
	in := somePasswords(t)
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", MinPasskeys: 1})
	c := in.passkeyed(t, "erin", "erin-bound", false)
	synced := in.passkeyed(t, "erin", "erin-synced-1", true)
	in.passkeyed(t, "erin", "erin-synced-2", true)
	if code, _ := in.me(t, synced); code != http.StatusOK {
		t.Fatalf("the session a synced passkey opened answered %d", code)
	}
	bound := true
	in.setPolicy(t, db.AuthPolicy{Password: "allowed", Passkey: "optional", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 1})
	if code, _ := in.me(t, synced); code != http.StatusUnauthorized {
		t.Errorf("with device_bound_only, the session a synced passkey opened answered %d", code)
	}
	if w := in.removing(t, "erin-synced-1", c); w.Code != http.StatusNoContent {
		t.Errorf("removing a synced passkey that counts for nothing answered %d %s", w.Code, w.Body)
	}
	if w := in.removing(t, "erin-bound", c); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "with 0 passkeys the policy accepts, and min_passkeys is 1") {
		t.Errorf("removing the one passkey the policy accepts answered %d %s", w.Code, w.Body)
	}
}

// The password goes, with the generator beside it and the sessions it opened, where the account
// holds min_passkeys the policy accepts, and not before; a generator is removed with a code it shows
// and never here; somebody else's credential, one nobody holds and one no credential could be named
// by are the same 404; a bearer token removes nothing, and neither does a session that may only enrol.
func TestThePasswordIsRemovedHereAsAtItsOwnRoute(t *testing.T) {
	in := somePasswords(t)
	in.policy(t, "allowed", "optional")
	bob := in.passkeyed(t, "bob", "bob-1", false)
	passworded, err := in.opening(t, "bob", api.OpenedBy{Credential: "bob-password"})
	if err != nil {
		t.Fatal(err)
	}
	if w := in.removing(t, "bob-password", bob); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "removing the password would leave this account with 1 passkey the policy accepts, and min_passkeys is 2") {
		t.Errorf("removing bob's password beside one passkey answered %d %s", w.Code, w.Body)
	}
	if w := in.removing(t, "bob-totp", bob); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "DELETE /api/v1/me/totp") {
		t.Errorf("removing bob's generator here answered %d %s", w.Code, w.Body)
	}
	in.passkeyed(t, "bob", "bob-2", false)
	for what, id := range map[string]string{"carol's password": "carol-password", "nobody's": "nobodys", "no identifier": "not%20one", "too long": strings.Repeat("a", 1365)} {
		if w := in.removing(t, id, bob); w.Code != http.StatusNotFound {
			t.Errorf("removing %s answered %d %s", what, w.Code, w.Body)
		}
	}
	if w := in.bearing(t, "DELETE", "/api/v1/me/credentials/bob-password", in.token(t, "bob", nil, nil), ""); w.Code != http.StatusForbidden {
		t.Errorf("a bearer token removing bob's password answered %d %s", w.Code, w.Body)
	}
	if w := in.removing(t, "bob-password", bob); w.Code != http.StatusNoContent {
		t.Fatalf("removing bob's password beside two passkeys answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from credentials where login = 'bob' and type in ('password', 'totp')`); n != 0 {
		t.Errorf("bob still holds %d of his password and generator", n)
	}
	if code, _ := in.me(t, passworded); code != http.StatusUnauthorized {
		t.Errorf("the session bob's password opened answered %d", code)
	}
	if got := in.trail(t); !slices.Equal(got[len(got)-2:], []string{"bob credential.remove bob-password", "bob credential.remove bob-totp"}) {
		t.Errorf("the audit log ends %q", got[len(got)-2:])
	}

	in.policy(t, "allowed", "required")
	enrolling := in.signedIn(t, "alice", api.SessionEnrolment)
	if w := in.removing(t, "alice-password", enrolling); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "enrols passkeys and nothing else") {
		t.Errorf("a session that may only enrol removing its password answered %d %s", w.Code, w.Body)
	}
}

// On an installation addressed by an IP address, the password is the one credential that signs
// anybody in, and is removed nowhere, whatever passkeys the account holds.
func TestThePasswordIsNotRemovedHereWhereNoPasskeySignsIn(t *testing.T) {
	in := passwordsAt(t, "https://192.0.2.10", false)
	c := in.signedIn(t, "carol", api.SessionFull)
	in.passkeyed(t, "carol", "carol-passkey-2", false)
	if w := in.removing(t, "carol-password", c); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "addressed by an IP address") {
		t.Errorf("removing carol's password answered %d %s", w.Code, w.Body)
	}
}
