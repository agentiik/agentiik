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
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The grant routes and GET /api/v1/me against a real PostgreSQL, behind the real Principals.

// sharing is somePrincipals serving the grant routes and the routes about the caller, with a token
// for each principal a test asks as. Besides somePrincipals' own: frank owns finance, gina owns
// finance/payroll alone, hank is in team-ops, which owns hr by its record, ivan views hr, and carol,
// who administers the installation, is in admins. hr/reports is a service account of hr.
//
// The routes' clock, at, runs a minute after the one grants are resolved by, so that what they write
// is written after what was seeded, as it would be.
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
	// A namespace's grant is not revoked at a workflow's route, nor a workflow's at its namespace's.
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

// An administrator shares any namespace, and one granting themselves, or a group they belong to, is
// told to the namespace's owners, in their GET /api/v1/me: the principal its record names, a group's
// members for a group, or where it names none, whoever holds the owner role on it. A grant to
// somebody else, and a deny, widen nothing and are told to nobody; the power goes no further than a
// credential that carries it.
func TestAnAdministratorSharesAnyNamespaceAndItsOwnersAreTold(t *testing.T) {
	in := someSharing(t)
	if got := in.listed(t, "/api/v1/hr/grants", "carol"); len(got) != 2 {
		t.Errorf("an administrator lists hr's grants as %q", got)
	}
	self := in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"carol","role":"editor"}`)
	// finance's record names no owner, as a namespace from v0.2 names none: dave and frank hold the
	// owner role on it, bob held it until an hour ago, and gina holds it on payroll alone.
	if got := in.strings(t, `select recipient || ' ' || kind || ' ' || namespace || ' ' || (access_grant->>'id') from notifications order by recipient`); !slices.Equal(got, []string{
		"dave admin_access_widened finance " + self.ID, "frank admin_access_widened finance " + self.ID,
	}) {
		t.Errorf("carol's grant to herself in finance was told as %q", got)
	}
	var detail string
	in.query(t, &detail, `select detail from audit_log where target = $1`, self.ID)
	if detail != `{"notified":["dave","frank"],"principal":"carol","role":"editor","scope":"finance"}` {
		t.Errorf("carol's grant to herself is recorded as %s", detail)
	}
	if !in.holds(t, "carol", api.RunReadData, invoicingTarget) {
		t.Error("the grant carol wrote herself gives her nothing")
	}

	// hr's record names team-ops, whose members are told, and a group carol is in widens her too.
	group := in.granted(t, "/api/v1/hr/workflows/onboarding/grants", "carol", `{"principal":"group:admins","role":"viewer"}`)
	if got := in.strings(t, `select recipient from notifications where access_grant->>'id' = $1`, group.ID); !slices.Equal(got, []string{"hank"}) {
		t.Errorf("carol's grant to her group in hr was told to %q", got)
	}

	// To somebody else, a deny, or by somebody who does not administer: nothing is told.
	before := len(in.strings(t, `select id from notifications`))
	in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"alice","role":"viewer"}`)
	in.granted(t, "/api/v1/finance/grants", "carol", `{"principal":"carol","deny":"secret:write"}`)
	in.granted(t, "/api/v1/finance/grants", "frank", `{"principal":"frank","role":"editor"}`)
	if after := len(in.strings(t, `select id from notifications`)); after != before {
		t.Errorf("grants that widen no administrator's own access told %d notifications", after-before)
	}

	// A token narrowed by a scope carries no administrator's power, and the path an administrator
	// is let through to is still one that is there.
	narrowed := in.token(t, "carol", nil, []string{"hr"}, in.now.Add(time.Hour))
	if w := in.ask(t, "GET", "/api/v1/hr/grants", narrowed, ""); w.Code != http.StatusNotFound {
		t.Errorf("an administrator's narrowed token listing hr's grants answered %d", w.Code)
	}
	for _, path := range []string{"/api/v1/nowhere/grants", "/api/v1/finance/workflows/nightly/grants", "/api/v1/No-Such/grants", "/api/v1/finance/workflows/%ff/grants"} {
		for _, method := range []string{"GET", "POST"} {
			if w := in.ask(t, method, path, "carol", `{"principal":"carol","role":"owner"}`); w.Code != http.StatusNotFound {
				t.Errorf("%s %s by an administrator answered %d: %s", method, path, w.Code, w.Body)
			}
		}
	}

	// The bootstrap token, an administrator until the first one enrols, shares as operator and
	// widens nobody's own access, since no grant can name operator.
	in.withBootstrap(t, "agk_op_bootstrap")
	boot := in.granted(t, "/api/v1/hr/grants", "agk_op_bootstrap", `{"principal":"alice","role":"viewer"}`)
	if boot.GrantedBy != "operator" {
		t.Errorf("the bootstrap token's grant was written by %q", boot.GrantedBy)
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
