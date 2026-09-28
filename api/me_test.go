package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// GET /api/v1/me and the dismissal of a notification, on someSharing's installation.

// me is the caller's answer, which has to be 200, read as it was written.
func (in sharing) me(t *testing.T, who string) (api.Me, json.RawMessage) {
	t.Helper()
	w := in.ask(t, "GET", "/api/v1/me", who, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/me by %.24s answered %d: %s", who, w.Code, w.Body)
	}
	var me api.Me
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	return me, w.Body.Bytes()
}

// permissionsOf is a caller's permissions as the answer writes them, one line a scope, in order.
func permissionsOf(raw json.RawMessage) []string {
	var answer struct {
		Permissions map[string][]string `json:"permissions"`
	}
	json.Unmarshal(raw, &answer)
	var out []string
	for at, held := range answer.Permissions {
		out = append(out, at+": "+strings.Join(held, ","))
	}
	slices.Sort(out)
	return out
}

const (
	editor   = "workflow:read,workflow:run,workflow:write,run:read,run:read_data,secret:use,secret:write"
	allNine  = "workflow:read,workflow:run,workflow:write,workflow:delete,run:read,run:read_data,secret:use,secret:write,grant:manage"
	operator = "workflow:run,run:read"
)

// A user is answered their record, their groups as a grant names them and their permissions: a
// namespace's at its name, and a workflow's where a grant or a deny on it changes what the
// namespace gives, with the whole of what applies there.
func TestMeIsTheCallersIdentityGroupsAndPermissions(t *testing.T) {
	in := someSharing(t)
	me, raw := in.me(t, "alice")
	if me.Principal != "alice" || me.Admin || !slices.Equal(me.Groups, []string{"group:team-finance"}) ||
		me.User == nil || me.User.Login != "alice" || me.User.DisplayName != "Alice" || me.ServiceAccount != nil ||
		me.Notifications == nil || len(me.Notifications) != 0 {
		t.Errorf("alice is answered %s", raw)
	}
	var user struct {
		User json.RawMessage `json:"user"`
	}
	json.Unmarshal(raw, &user)
	valid(t, "/$defs/user", user.User)
	if got, want := permissionsOf(raw), []string{
		"finance/monthly-invoicing: workflow:read,workflow:run,workflow:write,run:read,secret:use,secret:write",
		"finance: " + editor,
	}; !slices.Equal(got, want) {
		t.Errorf("alice holds\n%q\nwant\n%q", got, want)
	}

	// A service account, with its record and no group; the owner of one workflow, there alone.
	me, raw = in.me(t, "finance/nightly")
	if me.Principal != "finance/nightly" || me.User != nil || me.ServiceAccount == nil || me.ServiceAccount.Kind != "service_account" ||
		me.ServiceAccount.Namespace != "finance" || me.ServiceAccount.Name != "nightly" || me.ServiceAccount.CreatedBy != "carol" ||
		len(me.Groups) != 0 || me.Groups == nil {
		t.Errorf("finance/nightly is answered %s", raw)
	}
	if got := permissionsOf(raw); !slices.Equal(got, []string{"finance/payroll: " + operator}) {
		t.Errorf("finance/nightly holds %q", got)
	}
	if _, raw := in.me(t, "gina"); !slices.Equal(permissionsOf(raw), []string{
		"finance/payroll: workflow:read,workflow:run,workflow:write,workflow:delete,run:read,run:read_data,grant:manage",
	}) {
		t.Errorf("the owner of payroll alone holds %q", permissionsOf(raw))
	}

	// An administrator holds nothing in a namespace for being one, and nothing through a token
	// whose scope narrows the power away; the bootstrap token owns every namespace.
	me, raw = in.me(t, "carol")
	if !me.Admin || len(me.Permissions) != 0 || !slices.Equal(me.Groups, []string{"group:admins"}) {
		t.Errorf("carol is answered %s", raw)
	}
	if me, raw := in.me(t, in.token(t, "carol", []string{"workflow:read"}, nil, in.now.Add(time.Hour))); me.Admin {
		t.Errorf("carol's narrowed token is answered as an administrator's: %s", raw)
	}
	in.withBootstrap(t, "agk_op_bootstrap")
	me, raw = in.me(t, "agk_op_bootstrap")
	if me.Principal != "operator" || !me.Admin || me.User != nil || me.ServiceAccount != nil || len(me.Groups) != 0 ||
		!slices.Equal(permissionsOf(raw), []string{"finance: " + allNine, "hr: " + allNine}) {
		t.Errorf("the bootstrap token is answered %s", raw)
	}
	in.endBootstrap(t)
	if w := in.ask(t, "GET", "/api/v1/me", "agk_op_bootstrap", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the bootstrap token once ended is answered %d", w.Code)
	}
	if w := in.ask(t, "GET", "/api/v1/me", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("nobody is answered %d", w.Code)
	}
}

// What a token's scope narrows is narrowed here too: a workflow it reaches holds what the namespace
// gives there, which its namespace's key, outside it, no longer says; and a workflow whose denies
// take everything its namespace gives is left out, as one the caller cannot read, whose name the
// answer does not give them, though leaving it out reads as the namespace's permissions applying
// to it.
func TestMeIsNarrowedByTheCredential(t *testing.T) {
	in := someSharing(t)
	narrowed := in.token(t, "alice", []string{"workflow:run", "run:read_data", "grant:manage"}, []string{"finance/payroll", "hr"}, in.now.Add(time.Hour))
	if got := permissionsOf(func() json.RawMessage { _, raw := in.me(t, narrowed); return raw }()); !slices.Equal(got, []string{
		"finance/payroll: workflow:run,run:read_data",
	}) {
		t.Errorf("alice's narrowed token holds %q", got)
	}
	within := in.token(t, "alice", nil, []string{"finance"}, in.now.Add(time.Hour))
	if _, raw := in.me(t, within); !slices.Equal(permissionsOf(raw), []string{
		"finance/monthly-invoicing: workflow:read,workflow:run,workflow:write,run:read,secret:use,secret:write",
		"finance: " + editor,
	}) {
		t.Errorf("alice's token within finance holds %q", permissionsOf(raw))
	}

	for _, deny := range []string{"workflow:read", "run:read"} {
		in.granted(t, "/api/v1/hr/workflows/onboarding/grants", "carol", `{"principal":"ivan","deny":"`+deny+`"}`)
	}
	if _, raw := in.me(t, "ivan"); !slices.Equal(permissionsOf(raw), []string{"hr: workflow:read,run:read"}) || strings.Contains(string(raw), "onboarding") {
		t.Errorf("ivan, viewing hr and denied all of it on onboarding, is answered %s", raw)
	}
}

// An owner told of an administrator's grant reads it in GET /api/v1/me, and dismisses it once; one
// not theirs, one dismissed already and one a narrowed token would dismiss are the same absence,
// and a narrowed token reads none.
func TestAnOwnerReadsAndDismissesWhatTheyAreTold(t *testing.T) {
	in := someSharing(t)
	self := in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"carol","role":"editor","expires_at":"`+in.at.Add(24*time.Hour).Format(time.RFC3339)+`"}`)
	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `insert into notifications (id, recipient, kind, at, credential)
		values ('01JQ5P', 'frank', 'passkey_counter_refused', $1, 'aVBob25lUGFzc2tleQ')`, in.at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	me, raw := in.me(t, "frank")
	if len(me.Notifications) != 2 {
		t.Fatalf("frank is told %s", raw)
	}
	passkey, widened := me.Notifications[0], me.Notifications[1]
	if passkey.Kind != "passkey_counter_refused" || passkey.Credential != "aVBob25lUGFzc2tleQ" || passkey.Grant != nil || passkey.Namespace != "" ||
		passkey.Act != "" || passkey.By != "" || strings.Contains(string(raw), `"act":"",`) {
		t.Errorf("frank is told of a passkey as %+v", passkey)
	}
	if widened.Kind != "admin_access_widened" || widened.Namespace != "finance" || !widened.At.Equal(in.at) || widened.Credential != "" ||
		widened.Grant == nil || widened.Grant.ID != self.ID || widened.Grant.Principal != "carol" || widened.Grant.GrantedBy != "carol" ||
		widened.Grant.ExpiresAt == nil || !widened.Grant.ExpiresAt.Equal(*self.ExpiresAt) {
		t.Errorf("frank is told of carol's grant as %+v", widened)
	}
	var told struct {
		Notifications []struct {
			Grant json.RawMessage `json:"grant"`
		} `json:"notifications"`
	}
	json.Unmarshal(raw, &told)
	valid(t, "/$defs/accessGrant", told.Notifications[1].Grant)
	if want := `{"id":"` + widened.ID + `","kind":"admin_access_widened","at":"` + in.at.Format(time.RFC3339) + `","act":"granted","by":"carol","namespace":"finance","grant":`; !strings.Contains(string(raw), want) {
		t.Errorf("the notification is written\n%s\nwant it to hold\n%s", raw, want)
	}

	// A narrowed token reads none and dismisses none; nor does anybody else, or a malformed name.
	narrowed := in.token(t, "frank", nil, []string{"finance"}, in.now.Add(time.Hour))
	if me, raw := in.me(t, narrowed); len(me.Notifications) != 0 || me.Notifications == nil {
		t.Errorf("frank's narrowed token is told %s", raw)
	}
	for who, id := range map[string]string{narrowed: widened.ID, "hank": widened.ID, "carol": widened.ID, "frank": "not-an-id"} {
		if w := in.ask(t, "DELETE", "/api/v1/me/notifications/"+id, who, ""); w.Code != http.StatusNotFound {
			t.Errorf("dismissing %s as %.24s answered %d", id, who, w.Code)
		}
	}
	if w := in.ask(t, "DELETE", "/api/v1/me/notifications/"+widened.ID, "frank", ""); w.Code != http.StatusNoContent {
		t.Fatalf("frank dismissing his notification answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "DELETE", "/api/v1/me/notifications/"+widened.ID, "frank", ""); w.Code != http.StatusNotFound {
		t.Errorf("a notification dismissed twice answered %d", w.Code)
	}
	if me, _ := in.me(t, "frank"); len(me.Notifications) != 1 || me.Notifications[0].ID != passkey.ID {
		t.Errorf("frank is told %+v once he dismissed carol's grant", me.Notifications)
	}
	// dave, suspended, was told too, and his is his to dismiss.
	var daves int
	in.query(t, &daves, `select count(*) from notifications where recipient = 'dave'`)
	if daves != 1 {
		t.Errorf("dave is told %d notifications", daves)
	}
	// The audit log keeps the grant however it is dismissed.
	var kept int
	in.query(t, &kept, `select count(*) from audit_log where action = 'grant.create' and target = $1`, self.ID)
	if kept != 1 {
		t.Error("dismissing the notification took the grant's entry from the audit log")
	}
}

// A console session is a credential as a token is: a full one reads who its user is, and one a
// password opened where a passkey is required, which "enrols passkeys and nothing else", is refused
// on GET /api/v1/me, on the dismissal and on the grant routes with the 403 openapi.json names.
func TestASessionThatMayOnlyEnrolReadsNoIdentityAndSharesNothing(t *testing.T) {
	in := someSessions(t)
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewSharing(rt, api.SharingOptions{Pool: in.pool}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMe(rt, api.MeOptions{Pool: in.pool}); err != nil {
		t.Fatal(err)
	}
	full := in.open(t, "carol", api.OpenedBy{Credential: "carol-passkey"})
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, request(t, "GET", "/api/v1/me", "", full))
	var me api.Me
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &me) != nil || me.Principal != "carol" || !me.Admin {
		t.Errorf("carol's full session reading who she is answered %d: %s", w.Code, w.Body)
	}
	enrolling := in.open(t, "carol", api.OpenedBy{Credential: "carol-password"})
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/me"},
		{"DELETE", "/api/v1/me/notifications/01JQ5P"},
		{"GET", "/api/v1/finance/grants"},
		{"POST", "/api/v1/finance/grants"},
		{"DELETE", "/api/v1/finance/workflows/payroll/grants/01JQ5P"},
	} {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, request(t, c.method, c.path, publicOrigin, enrolling))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "this session enrols passkeys and nothing else") {
			t.Errorf("%s %s answered a session that may only enrol %d: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}
