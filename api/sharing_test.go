package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The grant routes and GET /api/v1/me against a real PostgreSQL, behind the real Principals.

// sharing is somePrincipals serving the grant routes and the routes about the caller, with a token
// for each principal a test asks as. Besides somePrincipals' own: frank owns finance, gina owns
// finance/payroll alone, hank is in team-ops, which owns hr by its record, ivan views hr, and
// carol, who administers the installation, is in admins. hr/reports is a service account of hr.
//
// The routes' clock, at, runs a minute after the one grants are resolved by, so that what they
// write is written after what was seeded, as it would be.
type sharing struct {
	principals
	h      http.Handler
	at     time.Time
	tokens map[string]string
}

func someSharing(t *testing.T) sharing {
	t.Helper()
	in := somePrincipals(t)
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	at := in.now.Add(time.Minute)
	now := func() time.Time { return at }
	if _, err := api.NewSharing(rt, api.SharingOptions{Pool: in.pool, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMe(rt, api.MeOptions{Pool: in.pool, Now: now}); err != nil {
		t.Fatal(err)
	}
	err = in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, login := range []string{"frank", "gina", "hank", "ivan"} {
			if err := w.CreateUser(ctx, db.User{Login: login, DisplayName: strings.ToUpper(login[:1]) + login[1:]}); err != nil {
				return err
			}
		}
		for group, member := range map[string]string{"team-ops": "hank", "admins": "carol"} {
			if err := w.CreateGroup(ctx, group); err != nil {
				return err
			}
			if _, err := w.AddMember(ctx, group, member); err != nil {
				return err
			}
		}
		return w.CreateServiceAccount(ctx, db.ServiceAccount{Namespace: "hr", Name: "reports", CreatedBy: "carol"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for namespace, grants := range map[string][]access.Grant{
		"finance": {
			{Principal: "frank", Scope: access.Scope{Namespace: "finance"}, Role: access.Owner},
			{Principal: "gina", Scope: access.Scope{Namespace: "finance", Workflow: "payroll"}, Role: access.Owner},
		},
		"hr": {
			{Principal: "group:team-ops", Scope: access.Scope{Namespace: "hr"}, Role: access.Owner},
			{Principal: "ivan", Scope: access.Scope{Namespace: "hr"}, Role: access.Viewer},
		},
	} {
		err := in.pool.In(t.Context(), namespace, func(ctx context.Context, n *db.NS) error {
			for _, g := range grants {
				g.ID, g.GrantedBy = ulid.New(), "carol"
				if err := n.GrantAccess(ctx, g); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `update namespaces set owner = 'group:team-ops' where name = 'hr'`); err != nil {
		t.Fatal(err)
	}
	later := in.now.Add(time.Hour)
	s := sharing{principals: in, h: rt, at: at, tokens: map[string]string{}}
	for _, who := range []string{"alice", "carol", "frank", "gina", "hank", "ivan", "finance/nightly"} {
		s.tokens[who] = in.token(t, who, nil, nil, later)
	}
	return s
}

// ask sends one request as who, a principal's token or nobody's where who is empty.
func (in sharing) ask(t *testing.T, method, path, who, body string) *httptest.ResponseRecorder {
	t.Helper()
	token := in.tokens[who]
	if who != "" && token == "" {
		token = who
	}
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

// granted writes one grant through the route, which has to answer 201, and answers it.
func (in sharing) granted(t *testing.T, path, who, body string) access.Grant {
	t.Helper()
	w := in.ask(t, "POST", path, who, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST %s %s by %s answered %d: %s", path, body, who, w.Code, w.Body)
	}
	valid(t, "/$defs/accessGrant", w.Body.Bytes())
	var g access.Grant
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// listed is a listing of grants, each as its identifier, principal, what it gives and its scope.
func (in sharing) listed(t *testing.T, path, who string) []string {
	t.Helper()
	w := in.ask(t, "GET", path, who, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s by %s answered %d: %s", path, who, w.Code, w.Body)
	}
	var list api.GrantList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range list.Grants {
		gives := string(g.Role)
		if g.Deny != "" {
			gives = "deny " + string(g.Deny)
		}
		out = append(out, g.Principal+" "+gives+" "+g.Scope.String())
	}
	return out
}

// query answers one value the database holds, read behind the policies.
func (in sharing) query(t *testing.T, into any, sql string, args ...any) {
	t.Helper()
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), sql, args...).Scan(into); err != nil {
		t.Fatalf("%s: %s", sql, err)
	}
}

// strings answers a column of text the database holds, in the order the query gives.
func (in sharing) strings(t *testing.T, sql string, args ...any) []string {
	t.Helper()
	rows, err := dbtest.Superuser(t, in.super).Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// An owner shares their namespace, and the grant gives what it says from the next request; it is
// listed with its scope and recorded, and revoked, it gives nothing from the next request and is
// recorded as it was.
func TestAnOwnerSharesANamespaceAndRevokesIt(t *testing.T) {
	in := someSharing(t)
	ends := in.at.Add(30 * 24 * time.Hour)
	g := in.granted(t, "/api/v1/finance/grants", "frank",
		`{"principal":"group:team-ops","role":"viewer","expires_at":"`+ends.Format(time.RFC3339)+`"}`)
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(g.ID) || g.Principal != "group:team-ops" ||
		g.Scope != (access.Scope{Namespace: "finance"}) || g.Role != access.Viewer || g.Deny != "" ||
		g.ExpiresAt == nil || !g.ExpiresAt.Equal(ends) || g.GrantedBy != "frank" || !g.GrantedAt.Equal(in.at) {
		t.Errorf("the grant was answered %+v", g)
	}
	if !in.holds(t, "hank", api.WorkflowRead, payrollTarget) || in.holds(t, "hank", api.WorkflowRun, payrollTarget) {
		t.Error("a member of team-ops does not read finance's workflows, and only reads them, once team-ops views finance")
	}
	// The namespace's own grants, none expired and none written on one of its workflows.
	got := in.listed(t, "/api/v1/finance/grants", "frank")
	want := []string{"group:team-finance editor finance", "dave owner finance", "frank owner finance", "group:team-ops viewer finance"}
	if !slices.Equal(got, want) {
		t.Errorf("finance's grants are listed as %q, want %q", got, want)
	}
	var entry string
	in.query(t, &entry, `select actor || ' ' || action || ' ' || target || ' ' || coalesce(namespace, '') || ' ' || detail from audit_log`)
	if want := "frank grant.create " + g.ID + ` finance {"expires_at":"` + ends.Format(time.RFC3339) + `","principal":"group:team-ops","role":"viewer","scope":"finance"}`; entry != want {
		t.Errorf("the grant is recorded as\n%s\nwant\n%s", entry, want)
	}

	if w := in.ask(t, "DELETE", "/api/v1/finance/grants/"+g.ID, "frank", ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoking the grant answered %d: %s", w.Code, w.Body)
	}
	if in.holds(t, "hank", api.WorkflowRead, payrollTarget) {
		t.Error("the grant revoked still gives what it gave")
	}
	if got := in.listed(t, "/api/v1/finance/grants", "frank"); slices.Contains(got, "group:team-ops viewer finance") {
		t.Errorf("the grant revoked is listed: %q", got)
	}
	in.query(t, &entry, `select actor || ' ' || action || ' ' || target || ' ' || coalesce(namespace, '') || ' ' || detail from audit_log where action = 'grant.delete'`)
	if want := "frank grant.delete " + g.ID + ` finance {"expires_at":"` + ends.Format(time.RFC3339) + `","principal":"group:team-ops","role":"viewer","scope":"finance"}`; entry != want {
		t.Errorf("the revocation is recorded as\n%s\nwant\n%s", entry, want)
	}
	if w := in.ask(t, "DELETE", "/api/v1/finance/grants/"+g.ID, "frank", ""); w.Code != http.StatusNotFound {
		t.Errorf("revoking the grant again answered %d: %s", w.Code, w.Body)
	}

	// A deny is written and listed as one, and takes its permission away at once.
	d := in.granted(t, "/api/v1/finance/grants", "frank", `{"principal":"alice","deny":"secret:write"}`)
	if d.Deny != access.SecretWrite || d.Role != "" || d.ExpiresAt != nil {
		t.Errorf("the deny was answered %+v", d)
	}
	if in.holds(t, "alice", api.SecretWrite, financeTarget) || !in.holds(t, "alice", api.SecretUse, financeTarget) {
		t.Error("the deny of secret:write took something other than secret:write from alice")
	}
}

// A workflow's list is what applies to it: its namespace's grants, marked with the namespace's
// scope, before its own. Whoever may share one workflow shares it and nothing else, and a grant is
// revoked at the scope it was written at.
func TestAWorkflowsGrantsShowWhatItInherits(t *testing.T) {
	in := someSharing(t)
	d := in.granted(t, "/api/v1/finance/workflows/payroll/grants", "gina", `{"principal":"alice","deny":"workflow:run"}`)
	if d.Scope != (access.Scope{Namespace: "finance", Workflow: "payroll"}) || d.GrantedBy != "gina" {
		t.Errorf("the deny on payroll was answered %+v", d)
	}
	got := in.listed(t, "/api/v1/finance/workflows/payroll/grants", "gina")
	want := []string{
		"group:team-finance editor finance", "dave owner finance", "frank owner finance",
		"finance/nightly operator finance/payroll", "gina owner finance/payroll", "alice deny workflow:run finance/payroll",
	}
	if !slices.Equal(got, want) {
		t.Errorf("payroll's grants are listed as\n%q\nwant\n%q", got, want)
	}
	if got := in.listed(t, "/api/v1/finance/workflows/monthly-invoicing/grants", "frank"); !slices.Equal(got, []string{
		"group:team-finance editor finance", "dave owner finance", "frank owner finance", "alice deny run:read_data finance/monthly-invoicing",
	}) {
		t.Errorf("monthly-invoicing's grants are listed as %q", got)
	}

	// gina shares payroll and nothing more.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/finance/grants", ""},
		{"POST", "/api/v1/finance/grants", `{"principal":"alice","role":"viewer"}`},
		{"GET", "/api/v1/finance/workflows/monthly-invoicing/grants", ""},
		{"POST", "/api/v1/finance/workflows/monthly-invoicing/grants", `{"principal":"alice","role":"viewer"}`},
	} {
		if w := in.ask(t, c.method, c.path, "gina", c.body); w.Code != http.StatusNotFound {
			t.Errorf("%s %s by the owner of payroll alone answered %d: %s", c.method, c.path, w.Code, w.Body)
		}
	}

	var teamFinance string
	in.query(t, &teamFinance, `select id from grants where principal = 'group:team-finance'`)
	// A namespace's grant is not revoked at a workflow's route, nor a workflow's at its
	// namespace's.
	if w := in.ask(t, "DELETE", "/api/v1/finance/workflows/payroll/grants/"+teamFinance, "frank", ""); w.Code != http.StatusNotFound {
		t.Errorf("revoking a namespace's grant at a workflow's route answered %d", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/finance/grants/"+d.ID, "frank", ""); w.Code != http.StatusNotFound {
		t.Errorf("revoking a workflow's deny at its namespace's route answered %d", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/finance/workflows/monthly-invoicing/grants/"+d.ID, "frank", ""); w.Code != http.StatusNotFound {
		t.Errorf("revoking payroll's deny at monthly-invoicing's route answered %d", w.Code)
	}
	if !in.holds(t, "alice", api.WorkflowRead, payrollTarget) || in.holds(t, "alice", api.WorkflowRun, payrollTarget) {
		t.Error("the deny on payroll does not hold, or the namespace's grant went")
	}
	if w := in.ask(t, "DELETE", "/api/v1/finance/workflows/payroll/grants/"+d.ID, "gina", ""); w.Code != http.StatusNoContent {
		t.Errorf("the owner of payroll revoking its deny answered %d: %s", w.Code, w.Body)
	}
	if !in.holds(t, "alice", api.WorkflowRun, payrollTarget) {
		t.Error("the deny revoked still takes workflow:run away")
	}

	// A workflow that is not there has no grants, to its namespace's owner as to anybody.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/finance/workflows/nightly/grants", ""},
		{"POST", "/api/v1/finance/workflows/nightly/grants", `{"principal":"alice","role":"viewer"}`},
		{"DELETE", "/api/v1/finance/workflows/nightly/grants/" + d.ID, ""},
	} {
		if w := in.ask(t, c.method, c.path, "frank", c.body); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not yours") {
			t.Errorf("%s %s answered %d: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

// Sharing takes grant:manage where the route names, and a refusal is the absence of the namespace
// or the workflow: an editor, a viewer, a token narrowed to what shares nothing, and a principal
// holding nothing there are each answered 404, nobody is answered 401, and nothing is written.
func TestSharingTakesGrantManage(t *testing.T) {
	in := someSharing(t)
	narrowed := in.token(t, "frank", []string{"workflow:read", "run:read"}, nil, in.now.Add(time.Hour))
	elsewhere := in.token(t, "frank", nil, []string{"hr"}, in.now.Add(time.Hour))
	var ids string
	in.query(t, &ids, `select string_agg(id, ',') from grants where namespace = 'finance' and principal = 'frank'`)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/finance/grants", ""},
		{"POST", "/api/v1/finance/grants", `{"principal":"alice","role":"owner"}`},
		{"DELETE", "/api/v1/finance/grants/" + ids, ""},
		{"GET", "/api/v1/finance/workflows/payroll/grants", ""},
		{"POST", "/api/v1/finance/workflows/payroll/grants", `{"principal":"alice","role":"owner"}`},
		{"GET", "/api/v1/hr/grants", ""},
		{"POST", "/api/v1/hr/grants", `{"principal":"ivan","role":"owner"}`},
	}
	for _, who := range []string{"alice", "ivan", "finance/nightly", narrowed, elsewhere} {
		for _, c := range routes {
			if w := in.ask(t, c.method, c.path, who, c.body); w.Code != http.StatusNotFound {
				t.Errorf("%s %s by %.24s answered %d: %s", c.method, c.path, who, w.Code, w.Body)
			}
		}
	}
	for _, c := range routes {
		if w := in.ask(t, c.method, c.path, "", c.body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential answered %d", c.method, c.path, w.Code)
		}
	}
	var n int
	in.query(t, &n, `select count(*) from audit_log`)
	if n != 0 {
		t.Errorf("refused requests left %d entries in the audit log", n)
	}
	in.query(t, &n, `select count(*) from grants where granted_by <> 'carol'`)
	if n != 0 {
		t.Errorf("refused requests wrote %d grants", n)
	}
}

// An administrator writes a grant in any namespace, for anybody, by the installation's power, and
// the namespace's owners are told of each in their GET /api/v1/me: the principal its record names,
// a group's members for a group, or where it names none, whoever holds the owner role on it.
// Listing and revoking are not the power's: in a namespace, an administrator holds what their
// grants give. The power goes no further than a credential that carries it.
func TestAnAdministratorSharesAnyNamespaceAndItsOwnersAreTold(t *testing.T) {
	in := someSharing(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/hr/grants"}, {"GET", "/api/v1/finance/workflows/payroll/grants"},
	} {
		if w := in.ask(t, c.method, c.path, "carol", ""); w.Code != http.StatusNotFound {
			t.Errorf("%s %s by an administrator holding nothing there answered %d", c.method, c.path, w.Code)
		}
	}
	told := func(g access.Grant) []string {
		t.Helper()
		return in.strings(t, `select recipient from notifications where access_grant->>'id' = $1 and at = $2 order by recipient`, g.ID, in.at)
	}
	var detail string
	recorded := func(action, id string) string {
		t.Helper()
		in.query(t, &detail, `select detail from audit_log where action = $1 and target = $2`, action, id)
		return detail
	}

	// finance's record names no owner, as a namespace from v0.2 names none: dave and frank hold the
	// owner role on it, bob held it until an hour ago, and gina holds it on payroll alone.
	self := in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"carol","role":"editor"}`)
	if got := told(self); !slices.Equal(got, []string{"dave", "frank"}) {
		t.Errorf("carol's grant to herself in finance was told to %q", got)
	}
	if got := in.strings(t, `select kind || ' ' || namespace from notifications where access_grant->>'id' = $1`, self.ID); len(got) != 2 || got[0] != "admin_access_widened finance" {
		t.Errorf("carol's grant to herself was told as %q", got)
	}
	if got := recorded("grant.create", self.ID); got != `{"notified":["dave","frank"],"principal":"carol","role":"editor","scope":"finance"}` {
		t.Errorf("carol's grant to herself is recorded as %s", got)
	}
	if !in.holds(t, "carol", api.RunReadData, invoicingTarget) {
		t.Error("the grant carol wrote herself gives her nothing")
	}
	// For anybody, and a deny too: the owners are told of each.
	alice := in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"alice","role":"viewer"}`)
	aliceDeny := in.granted(t, "/api/v1/finance/workflows/payroll/grants", "carol", `{"principal":"alice","deny":"run:read_data"}`)
	for _, g := range []access.Grant{alice, aliceDeny} {
		if got := told(g); !slices.Equal(got, []string{"dave", "frank"}) {
			t.Errorf("carol's grant %+v was told to %q", g, got)
		}
	}
	// hr's record names team-ops, whose members are told.
	group := in.granted(t, "/api/v1/hr/workflows/onboarding/grants", "carol", `{"principal":"group:admins","role":"viewer"}`)
	if got := told(group); !slices.Equal(got, []string{"hank"}) {
		t.Errorf("carol's grant to her group in hr was told to %q", got)
	}
	// By somebody who does not administer, to themselves or anybody: nothing is told.
	before := len(in.strings(t, `select id from notifications`))
	in.granted(t, "/api/v1/finance/grants", "frank", `{"principal":"frank","role":"editor"}`)
	in.granted(t, "/api/v1/finance/grants", "frank", `{"principal":"alice","role":"editor"}`)
	if after := len(in.strings(t, `select id from notifications`)); after != before {
		t.Errorf("grants by an owner who does not administer told %d notifications", after-before)
	}
	// Revoking is grant:manage's alone.
	if w := in.ask(t, "DELETE", "/api/v1/finance/workflows/payroll/grants/"+aliceDeny.ID, "carol", ""); w.Code != http.StatusNotFound {
		t.Errorf("an administrator holding nothing in finance revoked a deny there, answered %d", w.Code)
	}

	// A token narrowed by a scope carries no administrator's power, and the path an administrator
	// is let through to is still one that is there.
	narrowed := in.token(t, "carol", nil, []string{"hr"}, in.now.Add(time.Hour))
	if w := in.ask(t, "POST", "/api/v1/hr/grants", narrowed, `{"principal":"carol","role":"owner"}`); w.Code != http.StatusNotFound {
		t.Errorf("an administrator's narrowed token sharing hr answered %d", w.Code)
	}
	for _, path := range []string{"/api/v1/nowhere/grants", "/api/v1/hr/workflows/nightly/grants", "/api/v1/No-Such/grants", "/api/v1/hr/workflows/%ff/grants"} {
		if w := in.ask(t, "POST", path, "carol", `{"principal":"carol","role":"owner"}`); w.Code != http.StatusNotFound {
			t.Errorf("POST %s by an administrator answered %d: %s", path, w.Code, w.Body)
		}
	}

	// Once carol owns finance by a grant, she shares it as its owners do, and is told of by her
	// own access alone: a role given to herself, a group she is in or a service account of a
	// namespace she owns, whose tokens she may mint, and a deny taken from any of them.
	deny := in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"carol","deny":"secret:write"}`)
	in.granted(t, "/api/v1/finance/grants", "frank", `{"principal":"carol","role":"owner"}`)
	if err := in.pool.In(t.Context(), "hr", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "group:admins", Scope: access.Scope{Namespace: "hr"}, Role: access.Owner, GrantedBy: "hank"})
	}); err != nil {
		t.Fatal(err)
	}
	before = len(in.strings(t, `select id from notifications`))
	in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"ivan","role":"viewer"}`)
	in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"alice","deny":"workflow:delete"}`)
	if w := in.ask(t, "DELETE", "/api/v1/finance/workflows/payroll/grants/"+aliceDeny.ID, "carol", ""); w.Code != http.StatusNoContent {
		t.Fatalf("carol, owning finance, revoking alice's deny answered %d", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/finance/grants/"+alice.ID, "carol", ""); w.Code != http.StatusNoContent {
		t.Fatalf("carol, owning finance, revoking alice's role answered %d", w.Code)
	}
	if after := len(in.strings(t, `select id from notifications`)); after != before {
		t.Errorf("an owner who administers, sharing with others, told %d notifications", after-before)
	}
	reports := in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"hr/reports","role":"viewer"}`)
	if got := told(reports); !slices.Equal(got, []string{"dave", "frank"}) {
		t.Errorf("carol's grant to a service account of hr, which her group owns, was told to %q", got)
	}
	if w := in.ask(t, "DELETE", "/api/v1/finance/grants/"+deny.ID, "carol", ""); w.Code != http.StatusNoContent {
		t.Fatalf("carol revoking the deny on herself answered %d", w.Code)
	}
	if got := in.strings(t, `select recipient || ' ' || (access_grant->>'deny') from notifications where access_grant->>'id' = $1 order by recipient, at`, deny.ID); !slices.Equal(got, []string{
		"dave secret:write", "dave secret:write", "frank secret:write", "frank secret:write",
	}) {
		t.Errorf("carol's deny on herself, written and then lifted, was told as %q", got)
	}
	if got := recorded("grant.delete", deny.ID); got != `{"deny":"secret:write","notified":["dave","frank"],"principal":"carol","scope":"finance"}` {
		t.Errorf("carol lifting the deny on herself is recorded as %s", got)
	}

	// The bootstrap token owns every namespace until it ends, and shares as operator as their
	// owners do, widening nobody's own access, since no grant can name operator.
	in.withBootstrap(t, "agk_op_bootstrap")
	before = len(in.strings(t, `select id from notifications`))
	boot := in.granted(t, "/api/v1/hr/grants", "agk_op_bootstrap", `{"principal":"alice","role":"viewer"}`)
	if boot.GrantedBy != "operator" {
		t.Errorf("the bootstrap token's grant was written by %q", boot.GrantedBy)
	}
	if after := len(in.strings(t, `select id from notifications`)); after != before {
		t.Errorf("the bootstrap token's grant told %d notifications", after-before)
	}
	if w := in.ask(t, "POST", "/api/v1/hr/grants", "agk_op_bootstrap", `{"principal":"operator","role":"owner"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a grant to operator answered %d: %s", w.Code, w.Body)
	}
}

// A grant names somebody: a principal that does not exist is 422, and so is a service account of a
// namespace its writer does not see, with the same sentence, so that the answer never says whether
// that namespace exists. A body that does not say one thing is 400, and an expiry past is 422.
// Nothing refused is written or recorded.
func TestAGrantNamesSomebodyItsWriterSees(t *testing.T) {
	in := someSharing(t)
	for _, principal := range []string{"nobody", "group:nowhere", "hr/reports", "hr/agentiik", "nowhere/agentiik", "finance/nowhere", "installation"} {
		w := in.ask(t, "POST", "/api/v1/finance/grants", "frank", `{"principal":"`+principal+`","role":"viewer"}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "principal "+principal+" names nobody") {
			t.Errorf("a grant to %s answered %d: %s", principal, w.Code, w.Body)
		}
	}
	ends := in.at.Format(time.RFC3339)
	for body, want := range map[string]int{
		`{"principal":"alice","role":"viewer","deny":"run:read_data"}`: http.StatusBadRequest,
		`{"principal":"alice"}`:                                                                 http.StatusBadRequest,
		`{"role":"viewer"}`:                                                                     http.StatusBadRequest,
		`{"principal":"alice","deny":"owner"}`:                                                  http.StatusBadRequest,
		`{"principal":"alice","role":"admin"}`:                                                  http.StatusBadRequest,
		`{"principal":"alice","deny":"run:everything"}`:                                         http.StatusBadRequest,
		`{"principal":"Alice","role":"viewer"}`:                                                 http.StatusBadRequest,
		`{"principal":"operator","role":"viewer"}`:                                              http.StatusBadRequest,
		`{"principal":"alice","role":"viewer","scope":"hr"}`:                                    http.StatusBadRequest,
		`{"principal":"alice","role":"viewer","expires_at":"soon"}`:                             http.StatusBadRequest,
		`{"principal":"alice","role":"viewer","expires_at":"` + ends + `"}`:                     http.StatusUnprocessableEntity,
		`{"principal":"alice","role":"viewer","padding":"` + strings.Repeat("x", 70<<10) + `"}`: http.StatusRequestEntityTooLarge,
	} {
		if w := in.ask(t, "POST", "/api/v1/finance/grants", "frank", body); w.Code != want {
			t.Errorf("%.80s answered %d, want %d: %s", body, w.Code, want, w.Body)
		}
	}
	var n int
	in.query(t, &n, `select count(*) from grants where granted_by <> 'carol'`)
	if n != 0 {
		t.Errorf("refused grants wrote %d rows", n)
	}
	in.query(t, &n, `select count(*) from audit_log`)
	if n != 0 {
		t.Errorf("refused grants were recorded %d times", n)
	}

	// A service account of finance itself, of a namespace frank sees since he holds a grant in it,
	// and any to an administrator, who sees every namespace; but not through a token that does not
	// reach the namespace.
	in.granted(t, "/api/v1/finance/grants", "frank", `{"principal":"finance/nightly","role":"viewer"}`)
	in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"hr/reports","role":"viewer"}`)
	in.granted(t, "/api/v1/hr/grants", "carol", `{"principal":"frank","role":"viewer"}`)
	in.granted(t, "/api/v1/finance/workflows/payroll/grants", "frank", `{"principal":"hr/reports","role":"operator"}`)
	within := in.token(t, "frank", nil, []string{"finance"}, in.now.Add(time.Hour))
	if w := in.ask(t, "POST", "/api/v1/finance/grants", within, `{"principal":"hr/reports","role":"viewer"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a grant to a service account of a namespace the token does not reach answered %d: %s", w.Code, w.Body)
	}
}

// An administrator putting themselves in a group widens their own access wherever the group's grants
// give something, and taking themselves out of one, or removing one they are in, wherever its denies
// took something away: each tells those namespaces' owners, as a grant they wrote themselves does,
// with the group's grant or deny, and is recorded naming who was told, by namespace. Putting somebody
// else in, putting themselves in a group that holds nothing, or doing again what is done already,
// tells nobody.
func TestAnAdministratorJoiningOrLeavingAGroupTellsTheOwnersWhereItWidensTheirAccess(t *testing.T) {
	in := someSharing(t)
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: in.pool, PublicURL: "https://agentiik.example.com", Now: func() time.Time { return in.at }}); err != nil {
		t.Fatal(err)
	}
	groups := in
	groups.h = rt
	// auditors edits finance/payroll and is denied run:read_data in hr; empty holds nothing.
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, name := range []string{"auditors", "empty"} {
			if err := w.CreateGroup(ctx, name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for namespace, g := range map[string]access.Grant{
		"finance": {Principal: "group:auditors", Scope: access.Scope{Namespace: "finance", Workflow: "payroll"}, Role: access.Editor},
		"hr":      {Principal: "group:auditors", Scope: access.Scope{Namespace: "hr"}, Deny: access.RunReadData},
	} {
		g.ID, g.GrantedBy = ulid.New(), "frank"
		if err := in.pool.In(t.Context(), namespace, func(ctx context.Context, n *db.NS) error { return n.GrantAccess(ctx, g) }); err != nil {
			t.Fatal(err)
		}
	}
	told := func() []string {
		t.Helper()
		return in.strings(t, `select recipient || ' ' || kind || ' ' || namespace || ' ' || (access_grant->>'principal') || ' '
		                             || coalesce(access_grant->>'role', 'deny ' || (access_grant->>'deny'))
		                        from notifications order by recipient, namespace, at, id`)
	}
	asked := func(method, path, action, detail string) {
		t.Helper()
		if w := groups.ask(t, method, path, "carol", ""); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
			t.Fatalf("%s %s by carol answered %d: %s", method, path, w.Code, w.Body)
		}
		var got string
		in.query(t, &got, `select detail from audit_log where action = $1 order by seq desc limit 1`, action)
		if got != detail {
			t.Errorf("%s %s is recorded as %s", method, path, got)
		}
	}

	asked("PUT", "/api/v1/groups/auditors/members/alice", "group_member.add", `{"member":"alice"}`)
	asked("PUT", "/api/v1/groups/empty/members/carol", "group_member.add", `{"member":"carol"}`)
	if got := told(); len(got) != 0 {
		t.Errorf("putting alice in auditors and carol in a group holding nothing told %q", got)
	}
	// finance's record names no owner, and dave and frank hold its owner role.
	asked("PUT", "/api/v1/groups/auditors/members/carol", "group_member.add", `{"member":"carol","notified":{"finance":["dave","frank"]}}`)
	joined := []string{
		"dave admin_access_widened finance group:auditors editor", "frank admin_access_widened finance group:auditors editor",
	}
	if got := told(); !slices.Equal(got, joined) {
		t.Errorf("carol putting herself in auditors told %q", got)
	}
	asked("PUT", "/api/v1/groups/auditors/members/carol", "group_member.add", `{"member":"carol"}`)
	if got := told(); !slices.Equal(got, joined) {
		t.Errorf("carol putting herself in auditors again told %q", got)
	}
	// hr's record names team-ops, whose member hank is told of the deny carol no longer has.
	asked("DELETE", "/api/v1/groups/auditors/members/carol", "group_member.remove", `{"member":"carol","notified":{"hr":["hank"]}}`)
	asked("DELETE", "/api/v1/groups/auditors/members/alice", "group_member.remove", `{"member":"alice"}`)
	left := []string{
		"dave admin_access_widened finance group:auditors editor", "frank admin_access_widened finance group:auditors editor",
		"hank admin_access_widened hr group:auditors deny run:read_data",
	}
	if got := told(); !slices.Equal(got, left) {
		t.Errorf("carol taking herself out of auditors told %q", got)
	}
	// Removing a group she is in lifts its deny from her, as leaving it does; the finance owners
	// are told again of her joining it first.
	asked("PUT", "/api/v1/groups/auditors/members/carol", "group_member.add", `{"member":"carol","notified":{"finance":["dave","frank"]}}`)
	asked("DELETE", "/api/v1/groups/auditors", "group.delete", `{"members":["carol"],"notified":{"hr":["hank"]}}`)
	asked("DELETE", "/api/v1/groups/empty", "group.delete", `{"members":["carol"]}`)
	if got := told(); !slices.Equal(got, []string{
		"dave admin_access_widened finance group:auditors editor", "dave admin_access_widened finance group:auditors editor",
		"frank admin_access_widened finance group:auditors editor", "frank admin_access_widened finance group:auditors editor",
		"hank admin_access_widened hr group:auditors deny run:read_data", "hank admin_access_widened hr group:auditors deny run:read_data",
	}) {
		t.Errorf("carol removing a group she is in told %q", got)
	}
	entries, err := in.pool.AuditTrail().After(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(entries); err != nil {
		t.Error(err)
	}
}
