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

// PATCH /api/v1/users/{login} giving a new login, against a real PostgreSQL, behind the real
// Principals: in place of a login v0.7.0 made a reserved word, and for nobody else.

// renamable is someProfiles with sessions accepted and three users whose logins v0.7.0 reserved:
// mcp, who has signed in, with a personal namespace, a passkey, a session, a token, a membership, a
// grant they hold and one they wrote; hooks, who has never signed in; and objects, an administrator.
type renamable struct {
	profiles
	session *http.Cookie
}

func someRenamable(t *testing.T) renamable {
	t.Helper()
	in := someProfiles(t)
	if err := in.p.AcceptSessions(publicOrigin); err != nil {
		t.Fatal(err)
	}
	var session *http.Cookie
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{
			{Login: "mcp", Email: "mc.petit@example.com", Profile: db.Profile{GivenName: "Marie-Claire", FamilyName: "Petit"}},
			{Login: "hooks", Profile: db.Profile{GivenName: "Hugo"}},
			{Login: "objects", Profile: db.Profile{GivenName: "Olga"}, Admin: true},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if _, err := w.CreateNamespace(ctx, db.Namespace{Name: "mcp", Kind: db.NamespacePersonal, Owner: "mcp"}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "mcp-passkey", Login: "mcp", Type: db.CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
			return err
		}
		if _, err := w.AddMember(ctx, "team-finance", "mcp"); err != nil {
			return err
		}
		for _, g := range []access.Grant{
			{ID: ulid.New(), Principal: "mcp", Scope: access.Scope{Namespace: "mcp"}, Role: access.Owner, GrantedBy: "mcp"},
			{ID: ulid.New(), Principal: "mcp", Scope: access.Scope{Namespace: "hr"}, Role: access.Viewer, GrantedBy: "carol"},
			{ID: ulid.New(), Principal: "bob", Scope: access.Scope{Namespace: "hr"}, Role: access.Editor, GrantedBy: "mcp"},
		} {
			if err := w.GrantAccess(ctx, g); err != nil {
				return err
			}
		}
		var err error
		session, err = api.OpenSession(ctx, w, "mcp", api.OpenedBy{Credential: "mcp-passkey"}, in.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	later := in.now.Add(time.Hour)
	for _, who := range []string{"mcp", "hooks", "objects"} {
		in.tokens[who] = in.token(t, who, nil, nil, later)
	}
	return renamable{profiles: in, session: session}
}

// count is how many rows a query counts.
func (in renamable) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// told is what the bearer of token is told, as GET /api/v1/me answers it.
func (in renamable) told(t *testing.T, who string) api.Me {
	t.Helper()
	w := in.ask(t, "GET", "/api/v1/me", who, "", nil)
	var me api.Me
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &me) != nil {
		t.Fatalf("GET /api/v1/me as %s answered %d: %s", who, w.Code, w.Body)
	}
	return me
}

// An administrator gives a user whose login v0.7.0 reserved a new login, and the user is answered
// under it from then on, by GET /api/v1/users/{login} and in their own GET /api/v1/me, through the
// token and the session they held, which stay valid; their personal namespace is renamed with them
// and keeps the old login as a former name; their credentials, membership and grants, those they
// wrote among them, follow. They are told with login_changed, naming the login they had, the one they
// were given and who gave it; the bootstrap token gives one as an administrator does, an address
// beside it; and an administrator giving themself one is told nothing. Each is recorded as
// user.rename, the personal namespace beside it as namespace.rename, personal, and an address as
// user.update.
func TestAnAdministratorGivesAUserANewLoginInPlaceOfAWordReservedSince(t *testing.T) {
	in := someRenamable(t)
	update := func(who, login, body string) *httptest.ResponseRecorder {
		return in.ask(t, "PATCH", "/api/v1/users/"+login, who, "application/json", []byte(body))
	}

	renamed, raw := userIn(t, update("carol", "mcp", `{"login":"mc-petit"}`), false)
	if renamed.Login != "mc-petit" || renamed.DisplayName != "Marie-Claire Petit" || renamed.Email != "mc.petit@example.com" {
		t.Errorf("mcp given mc-petit is answered %s", raw)
	}
	if got, _ := userIn(t, in.ask(t, "GET", "/api/v1/users/mc-petit", "carol", "", nil), false); got != renamed {
		t.Errorf("GET /api/v1/users/mc-petit answers %+v", got)
	}
	if w := in.ask(t, "GET", "/api/v1/users/mcp", "carol", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("GET /api/v1/users/mcp after the new login answered %d: %s", w.Code, w.Body)
	}

	// The token mcp held reads mc-petit, told of the login, and so does the session.
	me := in.told(t, "mcp")
	if me.Principal != "mc-petit" || me.User == nil || me.User.Login != "mc-petit" || !slices.Contains(me.Groups, "group:team-finance") ||
		!slices.Contains(me.Permissions["mc-petit"], api.GrantManage) || !slices.Contains(me.Permissions["hr"], api.WorkflowRead) {
		t.Errorf("mc-petit reads herself as %+v", me)
	}
	if len(me.Notifications) != 1 || me.Notifications[0].Kind != "login_changed" || me.Notifications[0].From != "mcp" ||
		me.Notifications[0].To != "mc-petit" || me.Notifications[0].By != "carol" || !me.Notifications[0].At.Equal(in.at) {
		t.Errorf("mc-petit is told %+v", me.Notifications)
	}
	r := request(t, "GET", "/api/v1/me", publicOrigin, in.session)
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"principal":"mc-petit"`) {
		t.Errorf("mcp's session after the new login answered %d: %s", w.Code, w.Body)
	}
	for what, query := range map[string]string{
		"her personal namespace":    `select count(*) from namespaces where name = 'mc-petit' and kind = 'personal' and owner = 'mc-petit' and former_names = '{mcp}' and storage = 'mcp'`,
		"its built-in identity":     `select count(*) from principals where id = 'mc-petit/agentiik'`,
		"her passkey":               `select count(*) from credentials where id = 'mcp-passkey' and login = 'mc-petit'`,
		"her membership":            `select count(*) from group_members where login = 'mc-petit'`,
		"the grants she holds":      `select count(*) / 2 from grants where principal = 'mc-petit'`,
		"the grant she wrote":       `select count(*) from grants where principal = 'bob' and granted_by = 'mc-petit'`,
		"nothing under the old one": `select 1 - count(*) from principals where id = 'mcp' or id like 'mcp/%'`,
	} {
		if n := in.count(t, query); n != 1 {
			t.Errorf("%s: %d rows answer the new login", what, n)
		}
	}

	// The bootstrap token gives hooks, who has no personal namespace, a login and an address at
	// once; objects, an administrator, gives themself one, and is told nothing.
	if hugo, _ := userIn(t, update("bootstrap", "hooks", `{"login":"hugo","email":"hugo@example.com"}`), false); hugo.Login != "hugo" || hugo.Email != "hugo@example.com" {
		t.Errorf("hooks given hugo and an address is answered %+v", hugo)
	}
	if told := in.told(t, "hooks").Notifications; len(told) != 1 || told[0].By != "operator" || told[0].From != "hooks" || told[0].To != "hugo" {
		t.Errorf("hugo is told %+v", told)
	}
	if olga, _ := userIn(t, update("objects", "objects", `{"login":"olga"}`), false); olga.Login != "olga" || !olga.Admin {
		t.Errorf("objects giving themself olga is answered %+v", olga)
	}
	if told := in.told(t, "objects").Notifications; len(told) != 0 {
		t.Errorf("olga, who gave herself the login, is told %+v", told)
	}

	for action, want := range map[string][]string{
		"user.rename": {
			`done carol mc-petit {"from":"mcp","to":"mc-petit"}`,
			`done operator hugo {"from":"hooks","to":"hugo"}`,
			`done objects olga {"from":"objects","to":"olga"}`,
		},
		"namespace.rename": {`done carol mc-petit {"from":"mcp","personal":true,"to":"mc-petit"}`},
		"user.update":      {`done operator hugo {"fields":["email"]}`},
	} {
		if got := in.entries(t, action); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("the audit log recorded %s as\n%s\nwant\n%s", action, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	if n := in.count(t, `select count(*) from audit_log where actor = 'mcp' or target = 'mcp'`); n != 0 {
		t.Errorf("%d entries of the audit log were written under the old login", n)
	}
}

// A new login is refused, saying why, and changes nothing: one off the grammar or the bound, a
// reserved word, operator or installation, null or no string, 400; a user whose login no release
// reserved after it was given, which never changes, a login another user or a namespace holds, as its
// name or a former one, and a user whose personal namespace has a run going, 409; a user who does not
// exist, 404; and anybody but an administrator, 403. Nothing is recorded or told.
func TestANewLoginIsRefusedWhatItCannotBe(t *testing.T) {
	in := someRenamable(t)
	conn := dbtest.Superuser(t, in.super)
	for _, stmt := range []string{
		`update namespaces set name = 'people', former_names = '{hr}' where name = 'hr'`,
		`insert into workflows (namespace, name) values ('mcp', 'daily')`,
		`insert into workflow_versions (namespace, workflow, commit, graph, author, created_at) values ('mcp', 'daily', 'c0ffee1', '{}', 'mcp', now())`,
		`insert into runs (namespace, id, workflow, commit, trigger) values ('mcp', '` + ulid.New() + `', 'daily', 'c0ffee1', 'manual')`,
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	for _, c := range []struct {
		who, login, body string
		status           int
		says             string
	}{
		{"carol", "mcp", `{"login":"MC"}`, http.StatusBadRequest, `login: "MC" is not a login`},
		{"carol", "mcp", `{"login":""}`, http.StatusBadRequest, "login: a user has a login"},
		{"carol", "mcp", `{"login":"` + strings.Repeat("m", 256) + `"}`, http.StatusBadRequest, "login: a login is at most 255 characters and this one is 256"},
		{"carol", "mcp", `{"login":"runs"}`, http.StatusBadRequest, "login: runs is reserved"},
		{"carol", "mcp", `{"login":"objects"}`, http.StatusBadRequest, "login: objects is reserved: it is the first segment of /objects"},
		{"carol", "mcp", `{"login":"operator"}`, http.StatusBadRequest, "login: operator is reserved"},
		{"carol", "mcp", `{"login":"installation"}`, http.StatusBadRequest, "login: installation is reserved"},
		{"carol", "mcp", `{"login":null}`, http.StatusBadRequest, "login: null names no login"},
		{"carol", "mcp", `{"login":7}`, http.StatusBadRequest, "login"},
		{"carol", "alice", `{"login":"alicia"}`, http.StatusConflict, "alice keeps their login: it is no word a later release reserved, and a login never changes otherwise"},
		{"carol", "mcp", `{"login":"bob"}`, http.StatusConflict, "bob is another user's login"},
		{"carol", "mcp", `{"login":"finance"}`, http.StatusConflict, "finance is a namespace's name"},
		{"carol", "mcp", `{"login":"hr"}`, http.StatusConflict, "hr is a name namespace people held before it was renamed"},
		{"carol", "mcp", `{"login":"mc-petit"}`, http.StatusConflict, "mcp keeps their login for now, since their personal namespace is renamed with it: namespace mcp has a run queued, running or waiting"},
		{"carol", "nobody", `{"login":"somebody"}`, http.StatusNotFound, "no user by that login"},
		{"alice", "mcp", `{"login":"mc-petit"}`, http.StatusForbidden, ""},
		{"mcp", "mcp", `{"login":"mc-petit"}`, http.StatusForbidden, ""},
	} {
		w := in.ask(t, "PATCH", "/api/v1/users/"+c.login, c.who, "application/json", []byte(c.body))
		if w.Code != c.status || !strings.Contains(refusalOf(w), c.says) {
			t.Errorf("PATCH /api/v1/users/%s %.40s as %q answered %d: %s, want %d saying %q", c.login, c.body, c.who, w.Code, w.Body, c.status, c.says)
		}
	}
	if n := in.count(t, `select count(*) from users where login in ('mcp', 'alice') and exists (select from namespaces where name = 'mcp' and owner = 'mcp' and former_names = '{}')`); n != 2 {
		t.Errorf("a refusal changed the users: %d of mcp and alice left as they were", n)
	}
	if n := in.count(t, `select count(*) from audit_log where action in ('user.rename', 'namespace.rename', 'user.update')`); n != 0 {
		t.Errorf("refusals were recorded: %d entries", n)
	}
	if n := in.count(t, `select count(*) from notifications where kind = 'login_changed'`); n != 0 {
		t.Errorf("refusals were told: %d notifications", n)
	}
}
