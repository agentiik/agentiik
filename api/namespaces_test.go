package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// /api/v1/namespaces against a real PostgreSQL, behind the real Principals: any user creates a
// namespace they own, an administrator one for another owner and bounds it, its owner or an
// administrator renames it, gives it a picture and removes it, and whoever holds a grant in one
// reads it.

// namespaces is somePrincipals serving the namespace routes, with a pool dmz besides default, and a
// token for carol, who administers the installation, alice, who edits finance through team-finance,
// and erin, who holds nothing but a deny in hr.
type namespaces struct {
	principals
	h                  http.Handler
	carol, alice, erin string
}

func someNamespaces(t *testing.T) namespaces {
	t.Helper()
	in := somePrincipals(t)
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewNamespaces(rt, api.NamespaceOptions{Pool: in.pool}); err != nil {
		t.Fatal(err)
	}
	err = in.pool.Installation(t.Context(), db.RunnerInventory, func(ctx context.Context, w *db.Wide) error {
		return w.CreateRunnerPool(ctx, db.RunnerPool{Name: "dmz", Labels: []string{"zone=dmz"}, CreatedBy: "carol"})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "erin", Profile: db.Profile{GivenName: "Erin"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = in.pool.In(t.Context(), "hr", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "erin", Scope: access.Scope{Namespace: "hr"}, Deny: access.RunReadData, GrantedBy: "carol"})
	})
	if err != nil {
		t.Fatal(err)
	}
	later := in.now.Add(time.Hour)
	return namespaces{
		principals: in, h: rt,
		carol: in.token(t, "carol", nil, nil, later),
		alice: in.token(t, "alice", nil, nil, later),
		erin:  in.token(t, "erin", nil, nil, later),
	}
}

// ask sends one request as the bearer of token, with body as it is written, and answers what came.
func (in namespaces) ask(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(t.Context(), method, path, nil)
	} else {
		r = httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	in.h.ServeHTTP(w, r)
	return w
}

// query answers one value the database holds, read behind the policies.
func (in namespaces) query(t *testing.T, into any, sql string, args ...any) {
	t.Helper()
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), sql, args...).Scan(into); err != nil {
		t.Fatalf("%s: %s", sql, err)
	}
}

// count answers a count the database holds.
func (in namespaces) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	in.query(t, &n, sql, args...)
	return n
}

// entries is the audit log, each entry as actor, action, target and result.
func (in namespaces) entries(t *testing.T) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(), `select actor || ' ' || action || ' ' || target || ' ' || result from audit_log order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// An administrator creates a shared namespace with an owner and quotas. The owner is given the
// owner role on it in the same act, granted by the administrator, so that its members can share it
// and act in it at once; the namespace has its built-in identity, which holds nothing; and the
// creation is recorded, with the grant that made the owner one.
func TestAnAdministratorCreatesANamespaceItsOwnerOwns(t *testing.T) {
	in := someNamespaces(t)
	w := in.ask(t, "POST", "/api/v1/namespaces", in.carol,
		`{"name":"team-ops","kind":"shared","owner":"group:team-finance","quotas":{"max_runs_per_hour":500,"max_run_duration":"24h","allowed_runner_pools":["default","dmz"]}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a namespace answered %d: %s", w.Code, w.Body)
	}
	want := `{"name":"team-ops","kind":"shared","owner":"group:team-finance","quotas":{"max_concurrent_tasks":20,"max_runs_per_hour":500,"max_retention_days":90,"max_run_duration":"24h","allowed_runner_pools":["default","dmz"]},"former_names":[],"avatar_updated_at":null}`
	if strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("the namespace was answered as\n%s\nwant\n%s", w.Body, want)
	}
	valid(t, "/$defs/namespaceRecord", w.Body.Bytes())

	var role string
	in.query(t, &role, `select role || ' ' || granted_by from grants where namespace = 'team-ops' and principal = 'group:team-finance' and workflow is null`)
	if role != "owner carol" {
		t.Errorf("the owner's grant reads %q, want the owner role granted by carol", role)
	}
	if n := in.count(t, `select count(*) from grants where namespace = 'team-ops'`); n != 1 {
		t.Errorf("the new namespace holds %d grants, and only its owner's is written", n)
	}
	if n := in.count(t, `select count(*) from service_accounts where namespace = 'team-ops' and name = 'agentiik' and created_by is null`); n != 1 {
		t.Error("the new namespace has no built-in identity")
	}
	// The owner's grant is recorded as the grant it is, and the creation names it.
	var grant string
	in.query(t, &grant, `select id from grants where namespace = 'team-ops'`)
	if got := in.entries(t); len(got) != 2 || got[0] != "carol grant.create "+grant+" done" || got[1] != "carol namespace.create team-ops done" {
		t.Errorf("the audit log holds %q", got)
	}
	var detail string
	in.query(t, &detail, `select coalesce(namespace, '') || ' ' || detail from audit_log where action = 'grant.create'`)
	if detail != `team-ops {"principal":"group:team-finance","role":"owner","scope":"team-ops"}` {
		t.Errorf("the owner's grant is recorded in the namespace and with the detail %s", detail)
	}
	in.query(t, &detail, `select detail from audit_log where action = 'namespace.create'`)
	if !strings.Contains(detail, `"owner_grant":"`+grant+`"`) || !strings.Contains(detail, `"owner":"group:team-finance"`) {
		t.Errorf("the creation is recorded with %s", detail)
	}

	// alice is in team-finance, so she owns it: she reads it, and may share it.
	if w := in.ask(t, "GET", "/api/v1/namespaces/team-ops", in.alice, ""); w.Code != http.StatusOK {
		t.Errorf("a member of the owning group reading the namespace answered %d", w.Code)
	}
	if !in.holds(t, "alice", api.GrantManage, api.Target{Namespace: "team-ops"}) {
		t.Error("a member of the owning group does not hold grant:manage in the namespace it owns")
	}

	// A namespace created with nothing but a name and an owner holds the two quotas that always
	// hold a value, at their defaults; and one an administrator creates naming no owner is theirs.
	w = in.ask(t, "POST", "/api/v1/namespaces", in.carol, `{"name":"sandbox","owner":"bob"}`)
	if w.Code != http.StatusCreated || strings.TrimSpace(w.Body.String()) != `{"name":"sandbox","kind":"shared","owner":"bob","quotas":{"max_concurrent_tasks":20,"max_retention_days":90},"former_names":[],"avatar_updated_at":null}` {
		t.Errorf("a namespace with no quotas was answered %d %s", w.Code, w.Body)
	}
	w = in.ask(t, "POST", "/api/v1/namespaces", in.carol, `{"name":"carols"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"owner":"carol"`) {
		t.Errorf("an administrator's namespace naming no owner was answered %d %s", w.Code, w.Body)
	}
}

// Any user creates a shared namespace, and owns it: the owner role is theirs from the act that
// creates it, granted by themself, and the namespace takes the installation's defaults. Naming
// another owner, or any quota, is an administrator's, refused with 403 before anything is written;
// so is a namespace asked for by a service account or through a token narrowed by a scope, since a
// namespace is a person's.
func TestAUserCreatesANamespaceTheyOwn(t *testing.T) {
	in := someNamespaces(t)
	w := in.ask(t, "POST", "/api/v1/namespaces", in.alice, `{"name":"alice-lab"}`)
	if want := `{"name":"alice-lab","kind":"shared","owner":"alice","quotas":{"max_concurrent_tasks":20,"max_retention_days":90},"former_names":[],"avatar_updated_at":null}`; w.Code != http.StatusCreated || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("a user creating a namespace was answered %d %s, want %s", w.Code, w.Body, want)
	}
	valid(t, "/$defs/namespaceRecord", w.Body.Bytes())
	var role string
	in.query(t, &role, `select role || ' ' || granted_by from grants where namespace = 'alice-lab' and principal = 'alice'`)
	if role != "owner alice" {
		t.Errorf("the creator's grant reads %q, want the owner role granted by alice herself", role)
	}
	if !in.holds(t, "alice", api.GrantManage, api.Target{Namespace: "alice-lab"}) {
		t.Error("the user who created the namespace does not hold grant:manage in it")
	}
	if w := in.ask(t, "POST", "/api/v1/namespaces", in.alice, `{"name":"alice-notes","owner":"alice"}`); w.Code != http.StatusCreated {
		t.Errorf("a user naming themself as the owner was answered %d %s", w.Code, w.Body)
	}

	nightly := in.token(t, "finance/nightly", nil, nil, in.now.Add(time.Hour))
	narrowed := in.token(t, "alice", nil, []string{"finance"}, in.now.Add(time.Hour))
	carolNarrowed := in.token(t, "carol", nil, []string{"finance"}, in.now.Add(time.Hour))
	for _, c := range []struct {
		token, body, says string
	}{
		{in.alice, `{"name":"team-ops","owner":"bob"}`, "naming another owner"},
		{in.alice, `{"name":"team-ops","owner":"group:team-finance"}`, "naming another owner"},
		{in.alice, `{"name":"team-ops","quotas":{"max_runs_per_hour":5}}`, "quotas are an administrator's"},
		{nightly, `{"name":"team-ops"}`, "a service account creates no namespace"},
		{narrowed, `{"name":"team-ops"}`, "narrowed by a scope creates no namespace"},
		{carolNarrowed, `{"name":"team-ops","owner":"bob"}`, "narrowed by a scope creates no namespace"},
	} {
		if w := in.ask(t, "POST", "/api/v1/namespaces", c.token, c.body); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("%s answered %d %s, want 403 saying %q", c.body, w.Code, w.Body, c.says)
		}
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'team-ops'`); n != 0 {
		t.Error("a refused creation left a namespace behind")
	}
}

// Bounding a namespace, creating one for another owner, and removing, renaming or picturing one
// a caller does not own are an administrator's: anybody else is refused, with 403 at the
// installation's routes and the 404 of a namespace's own where the route is the owner's, and nothing
// is written or recorded. A token narrowed to a namespace carries no administrator's power, and the
// bootstrap token does until it has ended.
func TestOnlyAnAdministratorChangesANamespace(t *testing.T) {
	in := someNamespaces(t)
	narrowed := in.token(t, "carol", nil, []string{"finance"}, in.now.Add(time.Hour))
	for _, token := range []string{in.alice, narrowed} {
		for _, c := range []struct {
			method, path, body string
			want               int
		}{
			{"POST", "/api/v1/namespaces", `{"name":"team-ops","owner":"bob"}`, http.StatusForbidden},
			{"DELETE", "/api/v1/namespaces/finance", "", http.StatusNotFound},
			{"DELETE", "/api/v1/namespaces/nowhere", "", http.StatusNotFound},
			{"PATCH", "/api/v1/namespaces/finance", `{"name":"accounting"}`, http.StatusNotFound},
			{"DELETE", "/api/v1/namespaces/finance/avatar", "", http.StatusNotFound},
			{"PUT", "/api/v1/namespaces/finance/quotas", `{"max_runs_per_hour":1}`, http.StatusForbidden},
		} {
			if w := in.ask(t, c.method, c.path, token, c.body); w.Code != c.want {
				t.Errorf("%s %s by somebody who does not administer answered %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
			}
		}
	}
	if n := in.count(t, `select count(*) from namespaces`); n != 2 {
		t.Errorf("%d namespaces are there after refusals, and there were 2", n)
	}
	if got := in.entries(t); len(got) != 0 {
		t.Errorf("refusals were recorded: %q", got)
	}

	in.withBootstrap(t, "agk_bootstrap")
	if w := in.ask(t, "POST", "/api/v1/namespaces", "agk_bootstrap", `{"name":"team-ops","owner":"alice"}`); w.Code != http.StatusCreated {
		t.Fatalf("the bootstrap token creating a namespace answered %d: %s", w.Code, w.Body)
	}
	var grantedBy string
	in.query(t, &grantedBy, `select granted_by from grants where namespace = 'team-ops'`)
	if grantedBy != "operator" {
		t.Errorf("the bootstrap token's grant was written by %q", grantedBy)
	}
	in.endBootstrap(t)
	if w := in.ask(t, "DELETE", "/api/v1/namespaces/team-ops", "agk_bootstrap", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the bootstrap token after its end removing a namespace answered %d", w.Code)
	}
}

// A namespace is refused where its name is taken, by a namespace or a login, where its owner names
// no user or group, and where its quotas name a pool that does not exist, and nothing is left
// behind; a body the wire refuses is refused before anything is read from the database.
func TestANamespaceIsRefusedWhatTheWireAndTheInstallationRefuse(t *testing.T) {
	in := someNamespaces(t)
	for _, c := range []struct {
		body string
		want int
		says string
	}{
		{`{"name":"finance","owner":"alice"}`, http.StatusConflict, "finance is already a namespace"},
		{`{"name":"alice","owner":"bob"}`, http.StatusConflict, "is a user's login"},
		{`{"name":"team-ops","owner":"nobody"}`, http.StatusUnprocessableEntity, "names no user or group"},
		{`{"name":"team-ops","owner":"group:nowhere"}`, http.StatusUnprocessableEntity, "names no user or group"},
		{`{"name":"team-ops","owner":"finance/nightly"}`, http.StatusUnprocessableEntity, "names no user or group"},
		{`{"name":"team-ops","owner":"alice","quotas":{"allowed_runner_pools":["dmz","gpu"]}}`, http.StatusUnprocessableEntity, "names gpu, which is no runner pool"},
		{`{"name":"team-ops","owner":"operator"}`, http.StatusBadRequest, "the owner: operator"},
		{`{"owner":"alice"}`, http.StatusBadRequest, "a namespace has a name"},
		{`{"name":"runs","owner":"alice"}`, http.StatusBadRequest, "routes on"},
		{`{"name":"Team","owner":"alice"}`, http.StatusBadRequest, "not a namespace"},
		{`{"name":"team-ops","owner":"alice","kind":"personal"}`, http.StatusBadRequest, "first sign-in"},
		{`{"name":"team-ops","owner":"alice","kind":null}`, http.StatusBadRequest, "null"},
		{`{"name":"team-ops","owner":"alice","quotas":null}`, http.StatusBadRequest, "null"},
		{`{"name":"team-ops","owner":"alice","quotas":{"max_runs_per_hour":null}}`, http.StatusBadRequest, "null"},
		{`{"name":"team-ops","owner":"alice","quotas":{"max_runs_per_hour":0}}`, http.StatusBadRequest, "from 1"},
		{`{"name":"team-ops","owner":"alice","quotas":{"max_concurrent_tasks":2147483648}}`, http.StatusBadRequest, "from 1 to 2147483647"},
		{`{"name":"team-ops","owner":"alice","quotas":{"max_artifact_bytes":-1}}`, http.StatusBadRequest, "from 1"},
		{`{"name":"team-ops","owner":"alice","quotas":{"max_run_duration":"1h30m"}}`, http.StatusBadRequest, "timeout"},
		{`{"name":"team-ops","owner":"alice","quotas":{"max_run_duration":"99999999999d"}}`, http.StatusBadRequest, "longer than a clock measures"},
		{`{"name":"team-ops","owner":"alice","quotas":{"allowed_runner_pools":[]}}`, http.StatusBadRequest, "names no pool"},
		{`{"name":"team-ops","owner":"alice","quotas":{"allowed_runner_pools":["dmz","dmz"]}}`, http.StatusBadRequest, "twice"},
		{`{"name":"team-ops","owner":"alice","quotas":{"allowed_runner_pools":["DMZ"]}}`, http.StatusBadRequest, "not a runner pool's name"},
		{`{"name":"team-ops","owner":"alice","quotas":{"max_disk":1}}`, http.StatusBadRequest, "not a field"},
		{`{"name":"team-ops","owner":"alice","auth_policy":{}}`, http.StatusBadRequest, "not a field"},
		{`{"name":"team-ops","owner":"` + strings.Repeat("a", 64<<10) + `"}`, http.StatusRequestEntityTooLarge, "larger than"},
	} {
		w := in.ask(t, "POST", "/api/v1/namespaces", in.carol, c.body)
		if w.Code != c.want || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("%s answered %d %s, want %d saying %q", c.body, w.Code, w.Body, c.want, c.says)
		}
	}
	if n := in.count(t, `select count(*) from namespaces`); n != 2 {
		t.Errorf("%d namespaces are there after refusals, and there were 2", n)
	}
	if n := in.count(t, `select count(*) from principals where kind = 'service_account'`); n != 1 {
		t.Errorf("%d service accounts are there after refusals, and there was finance/nightly alone", n)
	}
	if got := in.entries(t); len(got) != 0 {
		t.Errorf("refusals were recorded: %q", got)
	}
}

// A namespace v0.2 created under stats, which v0.3.0 reserved for GET /api/v1/stats/pools, keeps
// its name until its owner renames it: it is read, bounded and removed as any other, and only a new
// namespace of that name, or a rename to it, is refused, naming the route that needs the word and
// the release that serves it.
func TestANamespaceCreatedBeforeItsWordWasReservedIsServed(t *testing.T) {
	in := someNamespaces(t)
	w := in.ask(t, "POST", "/api/v1/namespaces", in.carol, `{"name":"stats","owner":"alice"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "stats is a word the API routes on from v0.6.0, for GET /api/v1/stats/pools") {
		t.Errorf("creating stats answered %d %s", w.Code, w.Body)
	}
	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `insert into namespaces (name) values ('stats')`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v1/namespaces/stats", "", http.StatusOK},
		{"PUT", "/api/v1/namespaces/stats/quotas", `{"max_runs_per_hour":10}`, http.StatusOK},
		{"GET", "/api/v1/namespaces/stats/quotas", "", http.StatusOK},
		{"DELETE", "/api/v1/namespaces/stats", "", http.StatusNoContent},
		{"GET", "/api/v1/namespaces/stats", "", http.StatusNotFound},
	} {
		if w := in.ask(t, c.method, c.path, in.carol, c.body); w.Code != c.want {
			t.Errorf("%s %s answered %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
		}
	}
}

// A namespace is read by an administrator and by whoever holds a grant in it, its own or a group's;
// anybody else, and a principal holding nothing there but a deny, is answered as if it did not
// exist, and so is one that does not, whoever asks.
func TestANamespaceIsReadByWhoeverHoldsAGrantInIt(t *testing.T) {
	in := someNamespaces(t)
	nightly := in.token(t, "finance/nightly", nil, nil, in.now.Add(time.Hour))
	for _, c := range []struct {
		path, token string
		want        int
	}{
		{"/api/v1/namespaces/finance", in.carol, http.StatusOK},
		{"/api/v1/namespaces/hr", in.carol, http.StatusOK},
		{"/api/v1/namespaces/nowhere", in.carol, http.StatusNotFound},
		{"/api/v1/namespaces/%ff", in.carol, http.StatusNotFound},
		{"/api/v1/namespaces/finance", in.alice, http.StatusOK},
		{"/api/v1/namespaces/finance/quotas", in.alice, http.StatusOK},
		{"/api/v1/namespaces/finance", nightly, http.StatusOK},
		{"/api/v1/namespaces/hr", in.alice, http.StatusNotFound},
		{"/api/v1/namespaces/hr/quotas", in.alice, http.StatusNotFound},
		{"/api/v1/namespaces/hr", in.erin, http.StatusNotFound},
		{"/api/v1/namespaces/finance", "", http.StatusUnauthorized},
	} {
		w := in.ask(t, "GET", c.path, c.token, "")
		if w.Code != c.want {
			t.Errorf("GET %s answered %d, want %d: %s", c.path, w.Code, c.want, w.Body)
		}
	}

	listed := func(token string) string {
		t.Helper()
		w := in.ask(t, "GET", "/api/v1/namespaces", token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("listing answered %d: %s", w.Code, w.Body)
		}
		var l api.NamespaceList
		if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, n := range l.Namespaces {
			names = append(names, n.Name)
		}
		return strings.Join(names, ",")
	}
	for token, want := range map[string]string{in.carol: "finance,hr", in.alice: "finance", in.erin: "", nightly: "finance"} {
		if got := listed(token); got != want {
			t.Errorf("a listing answered %q, want %q", got, want)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/namespaces", in.erin, ""); strings.TrimSpace(w.Body.String()) != `{"namespaces":[]}` {
		t.Errorf("an empty listing reads %s", w.Body)
	}

	// A namespace v0.2 made names no owner, and reads so.
	w := in.ask(t, "GET", "/api/v1/namespaces/finance", in.carol, "")
	if strings.TrimSpace(w.Body.String()) != `{"name":"finance","kind":"shared","quotas":{"max_concurrent_tasks":20,"max_retention_days":90},"former_names":[],"avatar_updated_at":null}` {
		t.Errorf("a namespace with no owner reads %s", w.Body)
	}
	valid(t, "/$defs/quotas", in.ask(t, "GET", "/api/v1/namespaces/finance/quotas", in.alice, "").Body.Bytes())
}

// PUT on a namespace's quotas writes them whole: max_concurrent_tasks and max_retention_days keep
// their values where the body leaves them out, and each of the other four the body leaves out
// bounds nothing any more. A pool that does not exist is refused and nothing changes, and every
// change is recorded as namespace.update.
func TestANamespacesQuotasAreWrittenWhole(t *testing.T) {
	in := someNamespaces(t)
	all := `{"max_concurrent_tasks":7,"max_runs_per_hour":500,"max_artifact_bytes":536870912000,"max_retention_days":180,"max_run_duration":"4h","allowed_runner_pools":["dmz"]}`
	w := in.ask(t, "PUT", "/api/v1/namespaces/finance/quotas", in.carol, all)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != all {
		t.Fatalf("setting every quota answered %d %s", w.Code, w.Body)
	}
	valid(t, "/$defs/quotas", w.Body.Bytes())

	w = in.ask(t, "PUT", "/api/v1/namespaces/finance/quotas", in.carol, `{"max_runs_per_hour":10}`)
	if want := `{"max_concurrent_tasks":7,"max_runs_per_hour":10,"max_retention_days":180}`; w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("setting one quota answered %d %s, want %s", w.Code, w.Body, want)
	}
	if got := in.ask(t, "GET", "/api/v1/namespaces/finance/quotas", in.alice, ""); strings.TrimSpace(got.Body.String()) != strings.TrimSpace(w.Body.String()) {
		t.Errorf("the quotas read back as %s after being set to %s", got.Body, w.Body)
	}

	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/namespaces/finance/quotas", `{"allowed_runner_pools":["gpu"]}`, http.StatusUnprocessableEntity},
		{"/api/v1/namespaces/finance/quotas", `null`, http.StatusBadRequest},
		{"/api/v1/namespaces/finance/quotas", ``, http.StatusBadRequest},
		{"/api/v1/namespaces/finance/quotas", `{"max_retention_days":0}`, http.StatusBadRequest},
		{"/api/v1/namespaces/nowhere/quotas", `{}`, http.StatusNotFound},
		{"/api/v1/namespaces/Finance/quotas", `{}`, http.StatusNotFound},
		{"/api/v1/namespaces/%ff/quotas", `{}`, http.StatusNotFound},
		{"/api/v1/namespaces/finance/quotas", `{"allowed_runner_pools":["` + strings.Repeat("a", 64<<10) + `"]}`, http.StatusRequestEntityTooLarge},
	} {
		if got := in.ask(t, "PUT", c.path, in.carol, c.body); got.Code != c.want {
			t.Errorf("PUT %s %q answered %d, want %d: %s", c.path, c.body, got.Code, c.want, got.Body)
		}
	}
	if got := in.ask(t, "GET", "/api/v1/namespaces/finance/quotas", in.carol, ""); strings.TrimSpace(got.Body.String()) != strings.TrimSpace(w.Body.String()) {
		t.Errorf("a refused write changed the quotas to %s", got.Body)
	}

	// {} lifts every bound it can, and keeps the two that always hold a value.
	w = in.ask(t, "PUT", "/api/v1/namespaces/finance/quotas", in.carol, `{}`)
	if want := `{"max_concurrent_tasks":7,"max_retention_days":180}`; strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("an empty write answered %s, want %s", w.Body, want)
	}
	if got := in.entries(t); len(got) != 3 || got[0] != "carol namespace.update finance done" || got[2] != got[0] {
		t.Errorf("the audit log holds %q", got)
	}
	var detail string
	in.query(t, &detail, `select detail from audit_log where action = 'namespace.update' order by seq desc limit 1`)
	if detail != `{"quotas":{"max_concurrent_tasks":7,"max_retention_days":180}}` {
		t.Errorf("the last change is recorded with %s, and it is the quotas as they then stood", detail)
	}
}

// DELETE removes a namespace that holds nothing but its built-in identity, which goes first, with
// its grants and its authentication policy, and refuses one that holds a workflow, saying so, and a
// user's personal namespace, which goes only with its user.
func TestANamespaceIsRemovedOnlyWhenItHoldsNothing(t *testing.T) {
	in := someNamespaces(t)
	if w := in.ask(t, "POST", "/api/v1/namespaces", in.carol, `{"name":"team-ops","owner":"alice"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating answered %d: %s", w.Code, w.Body)
	}
	super := dbtest.Superuser(t, in.super)
	for _, stmt := range []string{
		`insert into auth_policy (namespace, device_bound_only) values ('team-ops', true)`,
		`insert into principals (id, kind) values ('dave-personal', 'user')`,
		`insert into users (login, display_name) values ('dave-personal', 'Dave')`,
		`insert into namespaces (name, kind, owner) values ('dave-personal', 'personal', 'dave-personal')`,
		// A namespace v0.2 made, which has no built-in identity.
		`insert into namespaces (name) values ('legacy')`,
	} {
		if _, err := super.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
	token := in.token(t, "team-ops/agentiik", nil, nil, in.now.Add(time.Hour))

	for _, c := range []struct {
		path string
		want int
		says string
	}{
		{"/api/v1/namespaces/finance", http.StatusConflict, "namespace finance holds 2 workflows"},
		{"/api/v1/namespaces/dave-personal", http.StatusConflict, "personal namespace"},
		{"/api/v1/namespaces/nowhere", http.StatusNotFound, "no namespace"},
		{"/api/v1/namespaces/%ff", http.StatusNotFound, "no such thing"},
	} {
		if w := in.ask(t, "DELETE", c.path, in.carol, ""); w.Code != c.want || !strings.Contains(w.Body.String(), c.says) {
			t.Errorf("DELETE %s answered %d %s, want %d saying %q", c.path, w.Code, w.Body, c.want, c.says)
		}
	}

	for _, name := range []string{"team-ops", "legacy"} {
		if w := in.ask(t, "DELETE", "/api/v1/namespaces/"+name, in.carol, ""); w.Code != http.StatusNoContent {
			t.Fatalf("removing %s answered %d: %s", name, w.Code, w.Body)
		}
	}
	for what, sql := range map[string]string{
		"the namespace":             `select count(*) from namespaces where name = 'team-ops'`,
		"its grants":                `select count(*) from grants where namespace = 'team-ops'`,
		"its authentication policy": `select count(*) from auth_policy where namespace = 'team-ops'`,
		"its built-in identity":     `select count(*) from principals where id = 'team-ops/agentiik'`,
		"its identity's token":      `select count(*) from api_tokens where principal = 'team-ops/agentiik'`,
	} {
		if n := in.count(t, sql); n != 0 {
			t.Errorf("%s is still there after the namespace was removed", what)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/namespaces", token, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the removed built-in identity's token answered %d", w.Code)
	}
	got := in.entries(t)
	if len(got) != 4 || got[2] != "carol namespace.delete team-ops done" || got[3] != "carol namespace.delete legacy done" {
		t.Errorf("the audit log holds %q", got)
	}

	// Created again under the same name, it starts with none of the old one's grants.
	if w := in.ask(t, "POST", "/api/v1/namespaces", in.carol, `{"name":"team-ops","owner":"bob"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating it again answered %d: %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from grants where namespace = 'team-ops' and principal = 'alice'`); n != 0 {
		t.Error("a namespace created again kept the grant of the one removed before it")
	}
}

// valid holds an answer to the vendored wire's definition at pointer.
func valid(t *testing.T, pointer string, answer []byte) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(answer))
	if err != nil {
		t.Fatalf("the answer is not JSON: %s", answer)
	}
	if err := wire(t, pointer).Validate(v); err != nil {
		t.Errorf("the answer is not what %s describes: %s\n%s", pointer, err, answer)
	}
}

// An owner removed while the namespace naming it is being created is the owner that names nobody,
// found by the namespace's reference to it rather than by the check before: 422, and nothing
// written or recorded.
func TestAnOwnerRemovedDuringACreationNamesNobody(t *testing.T) {
	in := someNamespaces(t)
	removing, err := dbtest.Superuser(t, in.super).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer removing.Rollback(context.WithoutCancel(t.Context()))
	if _, err := removing.Exec(t.Context(), `delete from principals where id = 'bob'`); err != nil {
		t.Fatal(err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- in.ask(t, "POST", "/api/v1/namespaces", in.carol, `{"name":"team-ops","owner":"bob"}`)
	}()
	// The creation read bob before the removal committed, and waits on the removal's lock to
	// refer to him: the one session of this test's database waiting on a lock. PostgreSQL is
	// shared, so the wait is looked for in this database alone.
	watching := dbtest.Superuser(t, in.super)
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := watching.QueryRow(t.Context(), `select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the creation never waited on the removal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := removing.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	w := <-answered
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "names no user or group") {
		t.Errorf("a creation whose owner was removed under it answered %d %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from namespaces where name = 'team-ops'`); n != 0 {
		t.Error("the namespace was created with an owner removed under it")
	}
	if got := in.entries(t); len(got) != 0 {
		t.Errorf("a refused creation was recorded: %q", got)
	}
}
