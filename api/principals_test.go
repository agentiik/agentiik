package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

// Who a request is from, and what they may do, against a real PostgreSQL: a bearer token looked up
// by its hash, the bootstrap token while it has not ended, and the grants of a principal and its
// groups resolved by package access, an administrator administering the installation and reading
// no payload.

// principals is an installation of principals and grants.
//
// finance holds monthly-invoicing and payroll, and hr holds onboarding. alice is in team-finance,
// which edits finance, and is denied run:read_data on monthly-invoicing; carol administers the
// installation and holds no grant; dave owns finance and is suspended; bob owned finance until an
// hour ago; finance/nightly, a service account, operates payroll.
type principals struct {
	pool  *db.Pool
	super string
	now   time.Time
	p     *api.Principals
}

func somePrincipals(t *testing.T) principals {
	t.Helper()
	pool, super := dbtest.Open(t)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance'), ('hr')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing'), ('finance', 'payroll'), ('hr', 'onboarding')`,
	} {
		if _, err := dbtest.Superuser(t, super).Exec(t.Context(), stmt); err != nil {
			t.Fatalf("seeding: %s", err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{
			{Login: "alice", DisplayName: "Alice"},
			{Login: "bob", DisplayName: "Bob"},
			{Login: "carol", DisplayName: "Carol", Admin: true},
			{Login: "dave", DisplayName: "Dave", Suspended: true},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := w.CreateGroup(ctx, "team-finance"); err != nil {
			return err
		}
		if _, err := w.AddMember(ctx, "team-finance", "alice"); err != nil {
			return err
		}
		return w.CreateServiceAccount(ctx, db.ServiceAccount{Namespace: "finance", Name: "nightly", CreatedBy: "carol"})
	})
	if err != nil {
		t.Fatal(err)
	}
	ended := now.Add(-time.Hour)
	err = pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		for _, g := range []access.Grant{
			{Principal: "group:team-finance", Scope: access.Scope{Namespace: "finance"}, Role: access.Editor},
			{Principal: "alice", Scope: access.Scope{Namespace: "finance", Workflow: "monthly-invoicing"}, Deny: access.RunReadData},
			{Principal: "finance/nightly", Scope: access.Scope{Namespace: "finance", Workflow: "payroll"}, Role: access.Operator},
			{Principal: "dave", Scope: access.Scope{Namespace: "finance"}, Role: access.Owner},
			{Principal: "bob", Scope: access.Scope{Namespace: "finance"}, Role: access.Owner, ExpiresAt: &ended},
		} {
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
	p, err := api.NewPrincipals(pool, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return principals{pool: pool, super: super, now: now, p: p}
}

// token mints a token of principal, narrowed as given, expiring at expires, and answers its value.
func (in principals) token(t *testing.T, principal string, permissions, within []string, expires time.Time) string {
	t.Helper()
	value := "agk_test_" + principal + "_" + ulid.New()
	hash := sha256.Sum256([]byte(value))
	err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.MintToken(ctx, db.APIToken{
			ID: ulid.New(), Hash: hash[:], Principal: principal, Permissions: permissions, Within: within,
			CreatedAt: in.now.Add(-time.Hour), ExpiresAt: expires,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// identified is who a request bearing value is, as Identify says.
func (in principals) identified(t *testing.T, value string) api.Identity {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/runs", nil)
	if value != "" {
		r.Header.Set("Authorization", "Bearer "+value)
	}
	as, err := in.p.Identify(r)
	if err != nil {
		t.Fatalf("identifying failed: %s", err)
	}
	return as
}

// holds is whether who holds what over over, failing the test on an error.
func (in principals) holds(t *testing.T, who api.Principal, what api.Permission, over api.Target) bool {
	t.Helper()
	ok, err := in.p.Allow(t.Context(), who, what, over)
	if err != nil {
		t.Fatalf("asking about %s: %s", who, err)
	}
	return ok
}

// withBootstrap keeps the hash of value as the bootstrap token's, as init does.
func (in principals) withBootstrap(t *testing.T, value string) {
	t.Helper()
	hash := sha256.Sum256([]byte(value))
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.SetBootstrapToken(ctx, hash[:])
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// endBootstrap ends the bootstrap token, as the first administrator's enrolment does.
func (in principals) endBootstrap(t *testing.T) {
	t.Helper()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, in.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

var (
	installationTarget = api.Target{}
	financeTarget      = api.Target{Namespace: "finance"}
	invoicingTarget    = api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}
	payrollTarget      = api.Target{Namespace: "finance", Workflow: "payroll"}
	onboardingTarget   = api.Target{Namespace: "hr", Workflow: "onboarding"}
)

// A live token names its principal and carries its scope, and its use is recorded, so that a token
// nobody uses can be seen and revoked.
func TestATokenIdentifiesItsPrincipalWithItsScopeAndRecordsItsUse(t *testing.T) {
	in := somePrincipals(t)
	value := in.token(t, "alice", []string{"run:read"}, []string{"finance/monthly-invoicing"}, in.now.Add(time.Hour))
	as := in.identified(t, value)
	if as.Principal != "alice" || as.Refused != "" {
		t.Fatalf("the token identified %+v", as)
	}
	want := access.TokenScope{Permissions: []access.Permission{access.RunRead}, Within: []access.Scope{{Namespace: "finance", Workflow: "monthly-invoicing"}}}
	if len(as.Scope.Permissions) != 1 || as.Scope.Permissions[0] != want.Permissions[0] || len(as.Scope.Within) != 1 || as.Scope.Within[0] != want.Within[0] {
		t.Errorf("the token's scope reads %+v, and it was minted %+v", as.Scope, want)
	}
	var used *time.Time
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), `select last_used_at from api_tokens where principal = 'alice'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used == nil || !used.Equal(in.now) {
		t.Errorf("the token's use was recorded as %v, and it was used at %v", used, in.now)
	}

	// A service account's, with no scope, carries none.
	if as := in.identified(t, in.token(t, "finance/nightly", nil, nil, in.now.Add(time.Hour))); as.Principal != "finance/nightly" || as.Scope.Permissions != nil || as.Scope.Within != nil {
		t.Errorf("a service account's token identified %+v", as)
	}
}

// A token that opens nothing identifies nobody, whatever the reason, and says the same sentence for
// each, which names none: expired, revoked, a suspended user's, or never issued.
func TestATokenThatOpensNothingIdentifiesNobodyWithOneSentence(t *testing.T) {
	in := somePrincipals(t)
	expired := in.token(t, "alice", nil, nil, in.now)
	suspended := in.token(t, "dave", nil, nil, in.now.Add(time.Hour))
	revoked := in.token(t, "alice", nil, nil, in.now.Add(time.Hour))
	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `update api_tokens set revoked_at = now() where hash = $1`, hashOf(revoked)); err != nil {
		t.Fatal(err)
	}
	var said string
	for what, value := range map[string]string{
		"expired": expired, "a suspended user's": suspended, "revoked": revoked,
		"never issued": "agk_test_nobody_" + ulid.New(), "a hash presented as the token": hex.EncodeToString(hashOf(expired)),
	} {
		as := in.identified(t, value)
		if as.Principal != "" || as.Refused == "" {
			t.Errorf("a token %s identified %+v", what, as)
		}
		if said != "" && as.Refused != said {
			t.Errorf("a token %s is refused saying %q, and another %q", what, as.Refused, said)
		}
		said = as.Refused
		if strings.Contains(as.Refused, "bootstrap") {
			t.Errorf("before the bootstrap token ended, a token %s is refused naming it: %q", what, as.Refused)
		}
	}
	// A request with no bearer token carries no credential, and says nothing of one.
	for _, header := range []string{"", "Basic YWxpY2U6aHVudGVyMg==", "Bearer ", "bearer " + expired} {
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/runs", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if as, err := in.p.Identify(r); err != nil || as.Principal != "" || as.Refused != "" {
			t.Errorf("%q identified %+v: %v", header, as, err)
		}
	}
}

func hashOf(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// The grants of a principal and of its groups decide, at both scopes: alice edits finance through
// team-finance, and her deny keeps run:read_data from her on monthly-invoicing alone; nothing of hr;
// a service account holds what its own grant gives; a grant that has expired gives nothing; and a
// suspended user holds nothing whatever they were granted.
func TestTheGrantsOfAPrincipalAndItsGroupsDecide(t *testing.T) {
	in := somePrincipals(t)
	for _, c := range []struct {
		who  api.Principal
		what api.Permission
		over api.Target
		want bool
	}{
		{"alice", api.WorkflowWrite, financeTarget, true},
		{"alice", api.SecretWrite, financeTarget, true},
		{"alice", api.RunReadData, payrollTarget, true},
		{"alice", api.RunReadData, invoicingTarget, false},
		{"alice", api.RunRead, invoicingTarget, true},
		{"alice", api.WorkflowDelete, financeTarget, false},
		{"alice", api.RunRead, onboardingTarget, false},
		{"finance/nightly", api.WorkflowRun, payrollTarget, true},
		{"finance/nightly", api.RunRead, payrollTarget, true},
		{"finance/nightly", api.WorkflowRead, payrollTarget, false},
		{"finance/nightly", api.WorkflowRun, invoicingTarget, false},
		{"bob", api.RunRead, financeTarget, false},
		{"dave", api.RunRead, financeTarget, false},
		{"erin", api.RunRead, financeTarget, false},
		{"", api.RunRead, financeTarget, false},
		{"alice", "workflow:everything", financeTarget, false},
	} {
		if got := in.holds(t, c.who, c.what, c.over); got != c.want {
			t.Errorf("%q holds %s over %+v: %v, want %v", c.who, c.what, c.over, got, c.want)
		}
	}
}

// "A platform administrator manages users, groups, namespaces, quotas, runners, runner policies and
// the authentication policy", which the routes that do require as grant:manage at installation
// scope, and "holds no implicit run:read_data": in a namespace an administrator holds what their
// grants give, which for carol is nothing. Nobody else administers, a namespace's owner included,
// and a suspended administrator administers nothing.
func TestAnAdministratorAdministersTheInstallationAndReadsNoPayload(t *testing.T) {
	in := somePrincipals(t)
	if !in.holds(t, "carol", api.GrantManage, installationTarget) {
		t.Error("an administrator does not administer the installation")
	}
	for _, p := range api.Permissions {
		if p != api.GrantManage && in.holds(t, "carol", p, installationTarget) {
			t.Errorf("an administrator holds %s at the installation", p)
		}
		for _, over := range []api.Target{financeTarget, invoicingTarget, onboardingTarget} {
			if in.holds(t, "carol", p, over) {
				t.Errorf("an administrator with no grant holds %s over %+v", p, over)
			}
		}
	}
	for _, who := range []api.Principal{"alice", "dave", "finance/nightly", "group:team-finance"} {
		if in.holds(t, who, api.GrantManage, installationTarget) {
			t.Errorf("%s administers the installation", who)
		}
	}
	if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), `update users set suspended = true where login = 'carol'`); err != nil {
		t.Fatal(err)
	}
	if in.holds(t, "carol", api.GrantManage, installationTarget) {
		t.Error("a suspended administrator administers the installation")
	}
}

// The bootstrap token identifies the bootstrap operator, written operator as the v0.2 operator was,
// an administrator holding owner in every namespace, until the first administrator has enrolled.
// Then it identifies nobody, and says why a token that stopped working may have, and the operator
// holds nothing any more.
func TestTheBootstrapTokenIsTheOperatorUntilTheFirstAdministratorEnrols(t *testing.T) {
	in := somePrincipals(t)
	const bootstrap = "agk_op_3q2Z7x9Kf1LmQ8vR4tYw6pBn0sDhJc5A"
	if as := in.identified(t, bootstrap); as.Principal != "" {
		t.Fatalf("with no bootstrap token kept, the token identified %+v", as)
	}
	in.withBootstrap(t, bootstrap)
	if as := in.identified(t, bootstrap); as.Principal != "operator" || as.Scope.Permissions != nil || as.Scope.Within != nil {
		t.Fatalf("the bootstrap token identified %+v", as)
	}
	for _, near := range []string{bootstrap + "x", bootstrap[:len(bootstrap)-1], strings.ToUpper(bootstrap)} {
		if as := in.identified(t, near); as.Principal != "" {
			t.Errorf("%q identified %+v", near, as)
		}
	}
	if !in.holds(t, api.BootstrapOperator, api.GrantManage, installationTarget) || in.holds(t, api.BootstrapOperator, api.RunReadData, installationTarget) {
		t.Error("the bootstrap operator does not administer the installation as an administrator does")
	}
	for _, p := range api.Permissions {
		for _, over := range []api.Target{financeTarget, invoicingTarget, onboardingTarget, {Namespace: "nowhere-yet"}} {
			if !in.holds(t, api.BootstrapOperator, p, over) {
				t.Errorf("the bootstrap operator does not hold %s over %+v", p, over)
			}
		}
	}

	in.endBootstrap(t)
	as := in.identified(t, bootstrap)
	if as.Principal != "" || !strings.Contains(as.Refused, "bootstrap token") || !strings.Contains(as.Refused, "first administrator enrolled") {
		t.Errorf("once the first administrator enrolled, the bootstrap token identified %+v", as)
	}
	for _, p := range api.Permissions {
		for _, over := range []api.Target{installationTarget, financeTarget} {
			if in.holds(t, api.BootstrapOperator, p, over) {
				t.Errorf("once the bootstrap ended, the operator holds %s over %+v", p, over)
			}
		}
	}
}

// Through the router, as serve builds it: a token's scope narrows every answer the router asks for,
// the one a handler asks about what it may reveal included, and an administrator's token narrowed to
// a namespace administers nothing. A bootstrap token after the first administrator's enrolment is a
// 401 that says why.
func TestThroughTheRouterATokenReachesOnlyWhatItsScopeKeeps(t *testing.T) {
	in := somePrincipals(t)
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	rt.MustHandle("GET", "/api/v1/{namespace}/workflows/{workflow}/runs",
		api.Needs{Permission: api.RunRead, Scope: api.Workflow, Reveals: api.RunReadData},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			data, err := api.Revealing(r)(r.Context(), over)
			if err != nil {
				t.Error(err)
			}
			if data {
				w.Header().Set("X-Data", "revealed")
			}
			w.WriteHeader(http.StatusOK)
		})
	rt.MustHandle("GET", "/api/v1/runners", api.Needs{Permission: api.GrantManage, Scope: api.Installation},
		func(w http.ResponseWriter, r *http.Request, who api.Principal, over api.Target) {
			w.WriteHeader(http.StatusOK)
		})
	ask := func(path, as string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), "GET", path, nil)
		if as != "" {
			r.Header.Set("Authorization", "Bearer "+as)
		}
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		return w
	}
	payroll, invoicing := "/api/v1/finance/workflows/payroll/runs", "/api/v1/finance/workflows/monthly-invoicing/runs"

	// alice unnarrowed: both workflows, payroll's data, not monthly-invoicing's.
	whole := in.token(t, "alice", nil, nil, in.now.Add(time.Hour))
	if w := ask(payroll, whole); w.Code != http.StatusOK || w.Header().Get("X-Data") == "" {
		t.Errorf("alice's whole token read payroll answering %d, data %q", w.Code, w.Header().Get("X-Data"))
	}
	if w := ask(invoicing, whole); w.Code != http.StatusOK || w.Header().Get("X-Data") != "" {
		t.Errorf("alice's whole token read monthly-invoicing answering %d, data %q", w.Code, w.Header().Get("X-Data"))
	}

	// Narrowed to run:read, the data she holds on payroll is not revealed to the token.
	reading := in.token(t, "alice", []string{"run:read"}, nil, in.now.Add(time.Hour))
	if w := ask(payroll, reading); w.Code != http.StatusOK || w.Header().Get("X-Data") != "" {
		t.Errorf("a token keeping run:read alone read payroll answering %d, data %q", w.Code, w.Header().Get("X-Data"))
	}
	// Narrowed to monthly-invoicing, payroll is not there, answered as an absence.
	within := in.token(t, "alice", nil, []string{"finance/monthly-invoicing"}, in.now.Add(time.Hour))
	if w := ask(invoicing, within); w.Code != http.StatusOK {
		t.Errorf("a token within monthly-invoicing read it answering %d", w.Code)
	}
	if w := ask(payroll, within); w.Code != http.StatusNotFound {
		t.Errorf("a token within monthly-invoicing read payroll answering %d", w.Code)
	}
	// Naming a permission the principal lacks grants nothing.
	deleting := in.token(t, "alice", []string{"run:read", "workflow:delete", "run:read_data"}, []string{"hr"}, in.now.Add(time.Hour))
	if w := ask("/api/v1/hr/workflows/onboarding/runs", deleting); w.Code != http.StatusNotFound {
		t.Errorf("a token naming what alice lacks read hr answering %d", w.Code)
	}

	// carol administers with a token of no scope, and with no narrowed one, a list naming
	// grant:manage included: "a token an administrator mints to run one workflow never creates a
	// user".
	for value, want := range map[string]int{
		in.token(t, "carol", nil, nil, in.now.Add(time.Hour)):                      http.StatusOK,
		in.token(t, "carol", []string{"grant:manage"}, nil, in.now.Add(time.Hour)): http.StatusForbidden,
		in.token(t, "carol", nil, []string{"finance"}, in.now.Add(time.Hour)):      http.StatusForbidden,
		in.token(t, "carol", []string{"run:read"}, nil, in.now.Add(time.Hour)):     http.StatusForbidden,
		whole: http.StatusForbidden,
	} {
		if w := ask("/api/v1/runners", value); w.Code != want {
			t.Errorf("the runner inventory answered %d, want %d", w.Code, want)
		}
	}

	// The bootstrap token, until the first administrator enrols, and then a 401 saying why.
	const bootstrap = "agk_op_3q2Z7x9Kf1LmQ8vR4tYw6pBn0sDhJc5A"
	in.withBootstrap(t, bootstrap)
	if w := ask("/api/v1/runners", bootstrap); w.Code != http.StatusOK {
		t.Errorf("the bootstrap token's inventory answered %d", w.Code)
	}
	if w := ask(invoicing, bootstrap); w.Code != http.StatusOK || w.Header().Get("X-Data") == "" {
		t.Errorf("the bootstrap token read monthly-invoicing answering %d, data %q", w.Code, w.Header().Get("X-Data"))
	}
	in.endBootstrap(t)
	for _, path := range []string{"/api/v1/runners", invoicing} {
		w := ask(path, bootstrap)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "the bootstrap token, that ended when the first administrator enrolled a passkey") {
			t.Errorf("once ended, the bootstrap token at %s answered %d: %s", path, w.Code, w.Body.String())
		}
	}
	if w := ask("/api/v1/runners", ""); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "carries no credential") {
		t.Errorf("no credential answered %d: %s", w.Code, w.Body.String())
	}
}
