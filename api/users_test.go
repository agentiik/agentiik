package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// The administrator's routes for users and groups, through the router serve builds, authorised by
// the installation's own Principals over a real PostgreSQL: an administrator, somebody who is not
// one, and the bootstrap token until the first administrator has enrolled.

// people is an installation with one administrator, carol, one user, alice, and the namespace
// finance, whose routes for users and groups are served as serve serves them.
type people struct {
	pool      *db.Pool
	super     string
	now       time.Time
	p         *api.Principals
	h         http.Handler
	carol     string
	alice     string
	bootstrap string
}

// enrolLink is an enrolment link as openapi.json's enrolmentLink writes it, on this installation's
// public URL.
var enrolLink = regexp.MustCompile(`^https://agentiik\.example\.com/auth/enrol#(agkenrol_[A-Za-z0-9_-]{43,})$`)

func somePeople(t *testing.T) people {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	if err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{{Login: "carol", DisplayName: "Carol", Admin: true}, {Login: "alice", DisplayName: "Alice"}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := api.NewPrincipals(pool, clock)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(p, p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: pool, PublicURL: "https://agentiik.example.com/", Now: clock}); err != nil {
		t.Fatal(err)
	}
	in := people{pool: pool, super: super, now: now, p: p, h: rt, bootstrap: "agk_op_" + ulid.New()}
	in.carol = in.token(t, "carol", nil, nil)
	in.alice = in.token(t, "alice", nil, nil)
	in.withBootstrap(t)
	return in
}

// token mints a token of principal, narrowed as given, good for an hour, and answers its value.
func (in people) token(t *testing.T, principal string, permissions, within []string) string {
	t.Helper()
	value := "agk_test_" + principal + "_" + ulid.New()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		return w.MintToken(ctx, db.APIToken{
			ID: ulid.New(), Hash: hashOf(value), Principal: principal, Permissions: permissions, Within: within,
			CreatedAt: in.now.Add(-time.Hour), ExpiresAt: in.now.Add(time.Hour),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// withBootstrap keeps the hash of the bootstrap token, as init does.
func (in people) withBootstrap(t *testing.T) {
	t.Helper()
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.SetBootstrapToken(ctx, hashOf(in.bootstrap))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// wide runs fn through the installation's door, failing the test on an error.
func (in people) wide(t *testing.T, fn func(context.Context, *db.Wide) error) {
	t.Helper()
	if err := in.pool.Installation(t.Context(), db.Identity, fn); err != nil {
		t.Fatal(err)
	}
}

// exec runs statements behind every policy, as a test sets things up.
func (in people) exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := dbtest.Superuser(t, in.super).Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %s", stmt, err)
		}
	}
}

// count is one number the database answers, behind every policy.
func (in people) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %s", query, err)
	}
	return n
}

// ask sends one request as the bearer of as and decodes the answer into out, where it is given.
func (in people) ask(t *testing.T, method, path, as, body string, out any) *httptest.ResponseRecorder {
	t.Helper()
	w := sent(t, in.h, method, path, as, body)
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s answered %s", method, path, w.Body)
		}
	}
	return w
}

// codeOf is the code a link carries after its #, failing the test for a link not written as one.
func codeOf(t *testing.T, link string) string {
	t.Helper()
	m := enrolLink.FindStringSubmatch(link)
	if m == nil {
		t.Fatalf("the link %q is not an enrolment link on the public URL", link)
	}
	return m[1]
}

// openCode is the enrolment code the value hashes to where it is open at now, or the error.
func (in people) openCode(t *testing.T, value string) (db.EnrolmentCode, error) {
	t.Helper()
	var c db.EnrolmentCode
	err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		var err error
		c, err = w.EnrolmentCodeByHash(ctx, hashOf(value), in.now)
		return err
	})
	return c, err
}

// enrol gives login a passkey, as the enrolment page will.
func (in people) enrol(t *testing.T, login string) {
	t.Helper()
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		return w.AddCredential(ctx, db.Credential{ID: "cGFzc2tleS0" + login, Login: login, Type: db.CredentialPasskey,
			PublicKey: []byte{1}, AAGUID: make([]byte, 16)})
	})
}

// The bootstrap token creates the first administrator and is answered a first administrator's link,
// shown once, good for an hour, and its code kept as its SHA-256 with operator as its issuer. Asked
// again as it was, it answers a fresh link and the one before opens nothing; asked otherwise, or
// once the administrator has enrolled, it is refused. A first administrator's link for a mistyped
// login is revoked by the next one, and once the bootstrap has ended the token creates nothing.
func TestTheBootstrapTokenCreatesTheFirstAdministratorAndAFreshLinkUntilTheyEnrol(t *testing.T) {
	in := somePeople(t)
	const dan = `{"login":"dan","display_name":"Dan Martin","admin":true}`

	var made api.CreatedUser
	w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, dan, &made)
	if w.Code != http.StatusCreated {
		t.Fatalf("the bootstrap token creating dan answered %d: %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a link shown once was answered with Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	u := made.User
	if u.Kind != "user" || u.Login != "dan" || u.DisplayName != "Dan Martin" || !u.Admin || u.Suspended || u.CreatedAt.IsZero() || !u.LastSignInAt.IsZero() {
		t.Errorf("dan was answered %+v", u)
	}
	if !made.Enrolment.ExpiresAt.Equal(in.now.Add(time.Hour)) {
		t.Errorf("the link expires at %s, an hour after %s", made.Enrolment.ExpiresAt, in.now)
	}
	var raw map[string]map[string]any
	json.Unmarshal(w.Body.Bytes(), &raw)
	if _, ok := raw["user"]["last_sign_in_at"]; ok {
		t.Errorf("a user who never signed in was answered with last_sign_in_at: %s", w.Body)
	}
	first := codeOf(t, made.Enrolment.Link)
	code, err := in.openCode(t, first)
	if err != nil || code.Login != "dan" || code.Kind != db.EnrolmentFirstAdministrator || code.IssuedBy != "operator" || !code.ExpiresAt.Equal(in.now.Add(time.Hour)) {
		t.Errorf("the link's code reads as %+v, %v", code, err)
	}

	// Asked again as it was: a fresh link, and the one before opens nothing.
	var again api.CreatedUser
	if w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, dan, &again); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("dan asked for again answered %d: %s", w.Code, w.Body)
	}
	second := codeOf(t, again.Enrolment.Link)
	if second == first {
		t.Fatal("dan asked for again was answered the same link")
	}
	if _, err := in.openCode(t, first); !errors.Is(err, db.ErrNoEnrolmentCode) {
		t.Errorf("the link a fresh one replaced was answered %v", err)
	}
	if _, err := in.openCode(t, second); err != nil {
		t.Errorf("the fresh link was answered %v", err)
	}

	// Asked with another display name, or as no administrator: somebody else's login.
	for _, other := range []string{`{"login":"dan","display_name":"Dan"}`, `{"login":"dan","display_name":"Dan Martin","admin":false}`, `{"login":"dan","admin":false}`} {
		if w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, other, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "another display name or admin") {
			t.Errorf("dan asked for otherwise answered %d: %s", w.Code, w.Body)
		}
	}

	// A mistyped login's link is revoked by the one made next with the bootstrap token.
	var mistyped api.CreatedUser
	if w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, `{"login":"dna","display_name":"Dan Martin","admin":true}`, &mistyped); w.Code != http.StatusCreated {
		t.Fatalf("a mistyped administrator answered %d: %s", w.Code, w.Body)
	}
	if _, err := in.openCode(t, second); !errors.Is(err, db.ErrNoEnrolmentCode) {
		t.Errorf("dan's link was left open beside a first administrator's link for another login: %v", err)
	}
	// Asked for with the login alone, as agk user create dan run again sends it: what is recorded
	// is kept, and the link is a first administrator's, since dan is one.
	if w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, `{"login":"dan"}`, &again); w.Code != http.StatusOK || !again.User.Admin || again.User.DisplayName != "Dan Martin" {
		t.Fatalf("dan asked for a third time, with his login alone, answered %d: %s", w.Code, w.Body)
	}
	if c, err := in.openCode(t, codeOf(t, again.Enrolment.Link)); err != nil || c.Kind != db.EnrolmentFirstAdministrator {
		t.Errorf("dan's link asked with his login alone reads as %+v, %v", c, err)
	}
	if _, err := in.openCode(t, codeOf(t, mistyped.Enrolment.Link)); !errors.Is(err, db.ErrNoEnrolmentCode) {
		t.Errorf("the mistyped login's link was left open beside dan's: %v", err)
	}

	// A user who is not an administrator is given a new user's link, issued by operator.
	var erin api.CreatedUser
	if w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, `{"login":"erin","display_name":"Erin"}`, &erin); w.Code != http.StatusCreated || erin.User.Admin {
		t.Fatalf("the bootstrap token creating erin answered %d: %s", w.Code, w.Body)
	}
	if c, err := in.openCode(t, codeOf(t, erin.Enrolment.Link)); err != nil || c.Kind != db.EnrolmentNewUser || c.IssuedBy != "operator" {
		t.Errorf("erin's link reads as %+v, %v", c, err)
	}

	// Once dan holds a credential, no link is issued for him either way.
	in.enrol(t, "dan")
	if w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, dan, nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "enrolled already") {
		t.Errorf("dan, enrolled, asked for again answered %d: %s", w.Code, w.Body)
	}
	if w := in.ask(t, "POST", "/api/v1/users/dan/enrolment", in.bootstrap, "", nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "recovery code") {
		t.Errorf("a link for dan, enrolled, answered %d: %s", w.Code, w.Body)
	}

	// Once the bootstrap has ended, the token creates nobody, and says why.
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, in.now)
		return err
	})
	if w := in.ask(t, "POST", "/api/v1/users", in.bootstrap, `{"login":"frank","display_name":"Frank","admin":true}`, nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "first administrator enrolled") {
		t.Errorf("the ended bootstrap token creating frank answered %d: %s", w.Code, w.Body)
	}
	// And an administrator creates one, answered a new user's link that they issued: a first
	// administrator's link is the bootstrap token's alone, and it has ended.
	var frank api.CreatedUser
	if w := in.ask(t, "POST", "/api/v1/users", in.carol, `{"login":"frank","display_name":"Frank","admin":true}`, &frank); w.Code != http.StatusCreated || !frank.User.Admin {
		t.Fatalf("carol creating frank, an administrator, once the bootstrap ended answered %d: %s", w.Code, w.Body)
	}
	if c, err := in.openCode(t, codeOf(t, frank.Enrolment.Link)); err != nil || c.Kind != db.EnrolmentNewUser || c.IssuedBy != "carol" {
		t.Errorf("frank's link reads as %+v, %v", c, err)
	}

	// Recorded with who acted, a repeat as unchanged, and never a link's code.
	var got []string
	entries := audited(t, in.pool)
	for _, e := range entries {
		got = append(got, e.Actor+" "+e.Action+" "+e.Target+" "+e.Result)
		for _, c := range []string{first, second, codeOf(t, erin.Enrolment.Link)} {
			if strings.Contains(e.Detail, c) {
				t.Errorf("entry %d carries a link's code: %s", e.Seq, e.Detail)
			}
		}
	}
	want := []string{
		"operator user.create dan done", "operator enrolment.issue dan done",
		"operator user.create dan unchanged", "operator enrolment.issue dan done",
		"operator user.create dna done", "operator enrolment.issue dna done",
		"operator user.create dan unchanged", "operator enrolment.issue dan done",
		"operator user.create erin done", "operator enrolment.issue erin done",
		"carol user.create frank done", "carol enrolment.issue frank done",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the audit log reads\n%s\nand the acts were\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Each with what was done: the user as created, and the link's kind, expiry and whether it
	// replaced another.
	if len(entries) == len(want) {
		for i, d := range []string{
			`{"admin":true,"display_name":"Dan Martin"}`,
			`{"expires_at":"` + in.now.Add(time.Hour).Format(time.RFC3339Nano) + `","kind":"first-administrator","replaced":false}`,
			`{"admin":true,"display_name":"Dan Martin"}`,
			`{"expires_at":"` + in.now.Add(time.Hour).Format(time.RFC3339Nano) + `","kind":"first-administrator","replaced":true}`,
		} {
			if entries[i].Detail != d {
				t.Errorf("entry %d details %s, want %s", entries[i].Seq, entries[i].Detail, d)
			}
		}
		if d := entries[len(entries)-1].Detail; !strings.Contains(d, `"kind":"enrolment"`) {
			t.Errorf("the link carol issued frank is recorded as %s", d)
		}
	}
}

// Every route is an administrator's: somebody who is not one is refused with 403, as is an
// administrator's token narrowed to a namespace or to permissions; a request with no credential is a
// 401, as is one with a suspended administrator's token, which opens nothing. An administrator
// reaches each.
func TestOnlyAnAdministratorAdministersUsersAndGroups(t *testing.T) {
	in := somePeople(t)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "bob", DisplayName: "Bob"}); err != nil {
			return err
		}
		return w.CreateGroup(ctx, "team-finance")
	})
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/users", `{"login":"dan","display_name":"Dan"}`},
		{"GET", "/api/v1/users", ""},
		{"GET", "/api/v1/users/bob", ""},
		{"POST", "/api/v1/users/bob/enrolment", ""},
		{"POST", "/api/v1/groups", `{"name":"team-ops"}`},
		{"GET", "/api/v1/groups", ""},
		{"GET", "/api/v1/groups/team-finance", ""},
		{"PUT", "/api/v1/groups/team-finance/members/bob", ""},
		{"DELETE", "/api/v1/groups/team-finance/members/bob", ""},
		{"DELETE", "/api/v1/groups/team-ops", ""},
		{"DELETE", "/api/v1/users/bob", ""},
	}
	narrowed := []string{
		in.token(t, "carol", nil, []string{"finance"}),
		in.token(t, "carol", []string{"grant:manage"}, nil),
	}
	for _, r := range routes {
		for _, as := range append([]string{in.alice}, narrowed...) {
			if w := in.ask(t, r.method, r.path, as, r.body, nil); w.Code != http.StatusForbidden {
				t.Errorf("%s %s by somebody who does not administer answered %d: %s", r.method, r.path, w.Code, w.Body)
			}
		}
		if w := in.ask(t, r.method, r.path, "", r.body, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential answered %d", r.method, r.path, w.Code)
		}
	}
	if n := in.count(t, `select count(*) from audit_log`); n != 0 {
		t.Errorf("requests refused made %d entries in the audit log", n)
	}
	for _, r := range routes {
		if w := in.ask(t, r.method, r.path, in.carol, r.body, nil); w.Code >= 300 {
			t.Errorf("%s %s by an administrator answered %d: %s", r.method, r.path, w.Code, w.Body)
		}
	}
	in.exec(t, `update users set suspended = true where login = 'carol'`)
	if w := in.ask(t, "GET", "/api/v1/users", in.carol, "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a suspended administrator listing the users answered %d", w.Code)
	}
}

// A login is held to the grammar a namespace is named in, its reserved words and the two authors
// of the installation's rows, and shares one name space with the namespaces; a display name is one
// line of at most 256 characters; a body the route does not read is refused.
func TestALoginIsHeldToTheNamespaceGrammarAndItsNameSpace(t *testing.T) {
	in := somePeople(t)
	for _, c := range []struct {
		body  string
		want  int
		means string
	}{
		{`{"login":"Dan","display_name":"Dan"}`, 400, "is not a login"},
		{`{"login":"dan_martin","display_name":"Dan"}`, 400, "is not a login"},
		{`{"login":"-dan","display_name":"Dan"}`, 400, "is not a login"},
		{`{"login":"","display_name":"Dan"}`, 400, "a user has a login"},
		{`{"display_name":"Dan"}`, 400, "a user has a login"},
		{`{"login":"` + strings.Repeat("d", 256) + `","display_name":"Dan"}`, 400, "at most 255"},
		{`{"login":"auth","display_name":"Auth"}`, 400, "login: auth is reserved"},
		{`{"login":"runner-pools","display_name":"Pools"}`, 400, "login: runner-pools is reserved"},
		{`{"login":"operator","display_name":"Operator"}`, 400, "login: operator is reserved"},
		{`{"login":"installation","display_name":"Installation"}`, 400, "login: installation is reserved"},
		{`{"login":"finance","display_name":"Finance"}`, 409, "finance is already a namespace"},
		{`{"login":"alice","display_name":"Alice"}`, 200, ""},
		{`{"login":"dan","display_name":""}`, 400, "a user has a display name"},
		{`{"login":"dan","display_name":"   \t"}`, 400, "control character"},
		{`{"login":"dan","display_name":"` + strings.Repeat("é", 257) + `"}`, 400, "at most 256 characters and this one is 257"},
		{`{"login":"dan","display_name":"Dan\nMartin"}`, 400, "control character"},
		{`{"login":"dan","display_name":"Dan\u001b[2J"}`, 400, "control character"},
		{`{"login":"dan","display_name":"Dan","admin":"yes"}`, 400, "true or false"},
		{`{"login":"dan","display_name":"Dan","kind":"user"}`, 400, "not a field"},
		{`{"login":"dan","display_name":"Dan","login":"eve"}`, 400, "twice"},
	} {
		w := in.ask(t, "POST", "/api/v1/users", in.carol, c.body, nil)
		if w.Code != c.want || !strings.Contains(w.Body.String(), c.means) {
			t.Errorf("%s answered %d: %s, want %d saying %q", c.body, w.Code, w.Body, c.want, c.means)
		}
	}
	// 256 characters of two bytes each is a display name, as the table counts it.
	if w := in.ask(t, "POST", "/api/v1/users", in.carol, `{"login":"dan","display_name":"`+strings.Repeat("é", 256)+`"}`, nil); w.Code != http.StatusCreated {
		t.Errorf("a display name of 256 characters answered %d: %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from users`); n != 3 {
		t.Errorf("the refusals left %d users, and there were two before dan", n)
	}
}

// An administrator's users are listed and read, never with a credential, and a new user's link is
// issued again for one who has not enrolled, revoking the one before it, issued by the
// administrator.
func TestAUserIsListedReadAndGivenAFreshLink(t *testing.T) {
	in := somePeople(t)
	var bob api.CreatedUser
	if w := in.ask(t, "POST", "/api/v1/users", in.carol, `{"login":"bob-martin","display_name":"Bob Martin"}`, &bob); w.Code != http.StatusCreated {
		t.Fatalf("creating bob-martin answered %d: %s", w.Code, w.Body)
	}
	if c, err := in.openCode(t, codeOf(t, bob.Enrolment.Link)); err != nil || c.Kind != db.EnrolmentNewUser || c.IssuedBy != "carol" {
		t.Errorf("bob-martin's link reads as %+v, %v", c, err)
	}
	// Created with the login alone, a user reads as their login; asked for again with the login
	// alone, or saying what was recorded, a fresh link; saying otherwise, refused.
	var hana api.CreatedUser
	if w := in.ask(t, "POST", "/api/v1/users", in.carol, `{"login":"hana"}`, &hana); w.Code != http.StatusCreated || hana.User.DisplayName != "hana" || hana.User.Admin {
		t.Errorf("hana, created with her login alone, answered %d: %s", w.Code, w.Body)
	}
	for body, want := range map[string]int{
		`{"login":"hana"}`: http.StatusOK, `{"login":"hana","display_name":null,"admin":null}`: http.StatusOK,
		`{"login":"hana","display_name":"hana","admin":false}`: http.StatusOK,
		`{"login":"hana","admin":true}`:                        http.StatusConflict, `{"login":"hana","display_name":"Hana"}`: http.StatusConflict,
	} {
		if w := in.ask(t, "POST", "/api/v1/users", in.carol, body, nil); w.Code != want {
			t.Errorf("%s answered %d: %s", body, w.Code, w.Body)
		}
	}
	in.exec(t, `delete from principals where id = 'hana'`)

	var listing struct {
		Users []api.User `json:"users"`
	}
	if w := in.ask(t, "GET", "/api/v1/users", in.carol, "", &listing); w.Code != http.StatusOK {
		t.Fatalf("the users answered %d: %s", w.Code, w.Body)
	}
	var logins []string
	for _, u := range listing.Users {
		logins = append(logins, u.Login)
	}
	if !slices.Equal(logins, []string{"alice", "bob-martin", "carol"}) || !listing.Users[2].Admin || listing.Users[1].Admin {
		t.Errorf("the users are listed as %+v", listing.Users)
	}

	in.exec(t, `update users set last_sign_in_at = now(), suspended = true where login = 'alice'`)
	var alice api.User
	w := in.ask(t, "GET", "/api/v1/users/alice", in.carol, "", &alice)
	if w.Code != http.StatusOK || alice.Kind != "user" || alice.DisplayName != "Alice" || !alice.Suspended || alice.LastSignInAt.IsZero() {
		t.Errorf("alice was read as %d %+v", w.Code, alice)
	}
	for _, field := range []string{"credentials", "hash", "public_key", "password"} {
		if strings.Contains(w.Body.String(), field) {
			t.Errorf("a user was read with %s: %s", field, w.Body)
		}
	}
	for _, path := range []string{"/api/v1/users/nobody", "/api/v1/users/Alice", "/api/v1/users/%ff", "/api/v1/users/operator"} {
		if w := in.ask(t, "GET", path, in.carol, "", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no user by that login") {
			t.Errorf("%s answered %d: %s", path, w.Code, w.Body)
		}
	}

	var fresh api.EnrolmentLink
	w = in.ask(t, "POST", "/api/v1/users/bob-martin/enrolment", in.carol, "", &fresh)
	if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" || !fresh.ExpiresAt.Equal(in.now.Add(time.Hour)) {
		t.Fatalf("a fresh link for bob-martin answered %d: %s", w.Code, w.Body)
	}
	if _, err := in.openCode(t, codeOf(t, bob.Enrolment.Link)); !errors.Is(err, db.ErrNoEnrolmentCode) {
		t.Errorf("the link a fresh one replaced was answered %v", err)
	}
	if _, err := in.openCode(t, codeOf(t, fresh.Link)); err != nil {
		t.Errorf("the fresh link was answered %v", err)
	}
	for path, want := range map[string]int{
		"/api/v1/users/nobody/enrolment": http.StatusNotFound,
		"/api/v1/users/Bob/enrolment":    http.StatusNotFound,
	} {
		if w := in.ask(t, "POST", path, in.carol, "", nil); w.Code != want {
			t.Errorf("%s answered %d: %s", path, w.Code, w.Body)
		}
	}
	if w := in.ask(t, "POST", "/api/v1/users/bob-martin/enrolment", in.carol, `{"kind":"recovery"}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("a link asked for with a body answered %d: %s", w.Code, w.Body)
	}
}

// A user is removed with everything they held, and their personal namespace where it holds
// nothing; refused while a namespace they do not own alone names them as owner, or while their
// personal namespace holds somebody's work, saying which.
func TestAUserIsRemovedWithWhatTheyHeldAndTheirEmptyPersonalNamespace(t *testing.T) {
	in := somePeople(t)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []string{"bob", "dan", "erin"} {
			if err := w.CreateUser(ctx, db.User{Login: u, DisplayName: u}); err != nil {
				return err
			}
		}
		if err := w.CreateGroup(ctx, "team-finance"); err != nil {
			return err
		}
		_, err := w.AddMember(ctx, "team-finance", "bob")
		return err
	})
	in.exec(t,
		`insert into namespaces (name, kind, owner) values ('dan', 'personal', 'dan'), ('erin', 'personal', 'erin'), ('team-ops', 'shared', 'erin')`,
		`insert into workflows (namespace, name) values ('erin', 'sandbox')`,
		// Each personal namespace with its built-in identity, as every namespace is created.
		`insert into principals (id, kind) values ('dan/agentiik', 'service_account'), ('erin/agentiik', 'service_account')`,
		`insert into service_accounts (namespace, name) values ('dan', 'agentiik'), ('erin', 'agentiik')`,
		`insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8T000000000000000000', 'finance', 'bob', 'editor', 'carol'),
		   ('01JQ3M8T000000000000000001', 'dan', 'dan', 'owner', 'installation'),
		   ('01JQ3M8T000000000000000002', 'dan', 'dan/agentiik', 'operator', 'dan')`,
	)
	in.token(t, "bob", nil, nil)
	in.token(t, "dan/agentiik", nil, nil)
	if w := in.ask(t, "POST", "/api/v1/users/bob/enrolment", in.carol, "", nil); w.Code != http.StatusCreated {
		t.Fatalf("a link for bob answered %d: %s", w.Code, w.Body)
	}

	if w := in.ask(t, "DELETE", "/api/v1/users/bob", in.carol, "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("removing bob answered %d: %s", w.Code, w.Body)
	}
	for _, q := range []string{
		`select count(*) from users where login = 'bob'`,
		`select count(*) from principals where id = 'bob'`,
		`select count(*) from api_tokens where principal = 'bob'`,
		`select count(*) from enrolment_codes where login = 'bob'`,
		`select count(*) from group_members where login = 'bob'`,
		`select count(*) from grants where principal = 'bob'`,
	} {
		if n := in.count(t, q); n != 0 {
			t.Errorf("once bob is removed, %s answers %d", q, n)
		}
	}
	if w := in.ask(t, "GET", "/api/v1/users/bob", in.carol, "", nil); w.Code != http.StatusNotFound {
		t.Errorf("bob, removed, was read answering %d", w.Code)
	}
	if w := in.ask(t, "DELETE", "/api/v1/users/bob", in.carol, "", nil); w.Code != http.StatusNotFound {
		t.Errorf("bob, removed, was removed again answering %d", w.Code)
	}

	// dan's personal namespace holds nothing but its built-in identity, and goes with him, the
	// identity, its token and every grant in the namespace with it.
	if w := in.ask(t, "DELETE", "/api/v1/users/dan", in.carol, "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("removing dan answered %d: %s", w.Code, w.Body)
	}
	for _, q := range []string{
		`select count(*) from namespaces where name = 'dan'`,
		`select count(*) from principals where id like 'dan%'`,
		`select count(*) from grants where namespace = 'dan'`,
		`select count(*) from api_tokens where principal = 'dan/agentiik'`,
	} {
		if n := in.count(t, q); n != 0 {
			t.Errorf("once dan is removed with his empty personal namespace and its built-in identity, %s answers %d", q, n)
		}
	}

	// erin owns team-ops, and her personal namespace holds a workflow: refused, naming each, and
	// nothing of hers removed.
	w := in.ask(t, "DELETE", "/api/v1/users/erin", in.carol, "", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "erin owns namespace team-ops") {
		t.Errorf("removing erin, owner of team-ops, answered %d: %s", w.Code, w.Body)
	}
	in.exec(t, `update namespaces set owner = null where name = 'team-ops'`)
	w = in.ask(t, "DELETE", "/api/v1/users/erin", in.carol, "", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "namespace erin holds 1 workflow") {
		t.Errorf("removing erin, whose personal namespace holds a workflow, answered %d: %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from users u join namespaces n on n.name = u.login where u.login = 'erin'`); n != 1 {
		t.Error("a refused removal removed erin or her namespace")
	}

	var got []string
	for _, e := range audited(t, in.pool) {
		if e.Action != audit.EnrolmentIssue {
			got = append(got, e.Actor+" "+e.Action+" "+e.Target+" "+e.Detail)
		}
	}
	want := []string{
		`carol user.delete bob {"admin":false,"display_name":"bob"}`,
		`carol namespace.delete dan {"personal":true}`,
		`carol user.delete dan {"admin":false,"display_name":"dan"}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("the removals were recorded as\n%s", strings.Join(got, "\n"))
	}
}

// A group is created, empty or with its first members, read, listed and removed; its membership
// changes touch no grant, and what its grants give a member is theirs from the next request and
// gone the same way; it is refused removal while a namespace's record names it as owner.
func TestAGroupsMembershipChangesWhatItsGrantsReachAndTouchesNoGrant(t *testing.T) {
	in := somePeople(t)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		return w.CreateUser(ctx, db.User{Login: "bob-martin", DisplayName: "Bob Martin"})
	})

	var made api.Group
	if w := in.ask(t, "POST", "/api/v1/groups", in.carol, `{"name":"team-finance","members":["bob-martin","alice"]}`, &made); w.Code != http.StatusCreated {
		t.Fatalf("creating team-finance answered %d: %s", w.Code, w.Body)
	}
	if made.Kind != "group" || made.Name != "team-finance" || !slices.Equal(made.Members, []string{"alice", "bob-martin"}) {
		t.Errorf("team-finance was answered %+v", made)
	}
	var empty api.Group
	w := in.ask(t, "POST", "/api/v1/groups", in.carol, `{"name":"finance-leads"}`, &empty)
	if w.Code != http.StatusCreated || empty.Members == nil || len(empty.Members) != 0 || !strings.Contains(w.Body.String(), `"members":[]`) {
		t.Errorf("an empty group was answered %d: %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		body  string
		want  int
		means string
	}{
		{`{"name":"team-finance"}`, 409, "exists already"},
		{`{"name":"Team"}`, 400, "not a group's name"},
		{`{"name":"groups"}`, 400, "name: groups is reserved"},
		{`{}`, 400, "a group has a name"},
		{`{"name":"team-ops","members":["nobody"]}`, 422, "nobody is no user"},
		{`{"name":"team-ops","members":["Alice"]}`, 400, "not a login"},
		{`{"name":"team-ops","members":["alice","alice"]}`, 400, "twice"},
		{`{"name":"team-ops","members":["group:team-finance"]}`, 400, "not a login"},
	} {
		if w := in.ask(t, "POST", "/api/v1/groups", in.carol, c.body, nil); w.Code != c.want || !strings.Contains(w.Body.String(), c.means) {
			t.Errorf("%s answered %d: %s, want %d saying %q", c.body, w.Code, w.Body, c.want, c.means)
		}
	}
	if n := in.count(t, `select count(*) from groups where name = 'team-ops'`); n != 0 {
		t.Error("a group refused for a member was created all the same")
	}

	// The group edits finance, and alice edits it through the group alone.
	in.exec(t, `insert into grants (id, namespace, principal, role, granted_by) values ('01JQ3M8T000000000000000000', 'finance', 'group:team-finance', 'editor', 'carol')`)
	holds := func() bool {
		t.Helper()
		ok, err := in.p.Allow(t.Context(), "alice", api.WorkflowWrite, api.Target{Namespace: "finance"})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !holds() {
		t.Fatal("alice does not edit finance through team-finance")
	}
	var now api.Group
	if w := in.ask(t, "DELETE", "/api/v1/groups/team-finance/members/alice", in.carol, "", &now); w.Code != http.StatusOK || !slices.Equal(now.Members, []string{"bob-martin"}) {
		t.Errorf("taking alice out answered %d: %s", w.Code, w.Body)
	}
	if holds() {
		t.Error("alice still edits finance once out of team-finance")
	}
	if n := in.count(t, `select count(*) from grants where principal = 'group:team-finance'`); n != 1 {
		t.Errorf("a membership change left %d grants of the group, and there was one", n)
	}
	for range 2 {
		if w := in.ask(t, "PUT", "/api/v1/groups/team-finance/members/alice", in.carol, "", &now); w.Code != http.StatusOK || !slices.Equal(now.Members, []string{"alice", "bob-martin"}) {
			t.Errorf("putting alice back answered %d: %s", w.Code, w.Body)
		}
	}
	if !holds() {
		t.Error("alice does not edit finance once back in team-finance")
	}
	if w := in.ask(t, "DELETE", "/api/v1/groups/finance-leads/members/alice", in.carol, "", &now); w.Code != http.StatusOK || len(now.Members) != 0 {
		t.Errorf("taking out of a group somebody who is not in it answered %d: %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		method, path string
		means        string
	}{
		{"PUT", "/api/v1/groups/team-finance/members/nobody", "no user"},
		{"PUT", "/api/v1/groups/team-finance/members/Alice", "no user"},
		{"PUT", "/api/v1/groups/nothing/members/alice", "no group"},
		{"DELETE", "/api/v1/groups/nothing/members/alice", "no group"},
		{"DELETE", "/api/v1/groups/team-finance/members/Alice", "no user"},
		{"PUT", "/api/v1/groups/%ff/members/alice", "no group"},
		{"GET", "/api/v1/groups/nothing", "no group"},
		{"GET", "/api/v1/groups/Team", "no group"},
		{"DELETE", "/api/v1/groups/nothing", "no group"},
	} {
		if w := in.ask(t, c.method, c.path, in.carol, "", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), c.means) {
			t.Errorf("%s %s answered %d: %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if w := in.ask(t, "PUT", "/api/v1/groups/team-finance/members/alice", in.carol, `{"role":"owner"}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("a member put with a body answered %d: %s", w.Code, w.Body)
	}

	var one api.Group
	if w := in.ask(t, "GET", "/api/v1/groups/team-finance", in.carol, "", &one); w.Code != http.StatusOK || !slices.Equal(one.Members, []string{"alice", "bob-martin"}) {
		t.Errorf("team-finance was read as %d %+v", w.Code, one)
	}
	var listing struct {
		Groups []api.Group `json:"groups"`
	}
	if w := in.ask(t, "GET", "/api/v1/groups", in.carol, "", &listing); w.Code != http.StatusOK || len(listing.Groups) != 2 ||
		listing.Groups[0].Name != "finance-leads" || listing.Groups[1].Name != "team-finance" || len(listing.Groups[1].Members) != 2 {
		t.Errorf("the groups are listed as %d %+v", w.Code, listing.Groups)
	}

	// Removed with its grants, and its members stay; refused while it owns a namespace.
	in.exec(t, `update namespaces set owner = 'group:team-finance' where name = 'finance'`)
	if w := in.ask(t, "DELETE", "/api/v1/groups/team-finance", in.carol, "", nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "group:team-finance owns namespace finance") {
		t.Errorf("removing a group owning finance answered %d: %s", w.Code, w.Body)
	}
	in.exec(t, `update namespaces set owner = null where name = 'finance'`)
	if w := in.ask(t, "DELETE", "/api/v1/groups/team-finance", in.carol, "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("removing team-finance answered %d: %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from grants where principal = 'group:team-finance'`); n != 0 {
		t.Errorf("the group's grants outlived it: %d", n)
	}
	if n := in.count(t, `select count(*) from users where login in ('alice', 'bob-martin')`); n != 2 {
		t.Error("removing a group removed its members")
	}
	if holds() {
		t.Error("alice edits finance through a group removed")
	}

	var got []string
	for _, e := range audited(t, in.pool) {
		got = append(got, e.Action+" "+e.Target+" "+e.Result+" "+e.Detail)
	}
	want := []string{
		`group.create group:team-finance done {"members":["alice","bob-martin"]}`,
		`group.create group:finance-leads done {"members":[]}`,
		`group_member.remove group:team-finance done {"member":"alice"}`,
		`group_member.add group:team-finance done {"member":"alice"}`,
		`group_member.add group:team-finance unchanged {"member":"alice"}`,
		`group_member.remove group:finance-leads unchanged {"member":"alice"}`,
		`group.delete group:team-finance done {"members":["alice","bob-martin"]}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("the audit log reads\n%s\nand the acts were\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A request refused by the audit log is refused whole: no user, group or member is left behind by
// an act the log could not record.
func TestAnActTheAuditLogRefusesLeavesNothing(t *testing.T) {
	in := somePeople(t)
	refuseAppends(t, in.super)
	for _, r := range []struct{ method, path, body string }{
		{"POST", "/api/v1/users", `{"login":"dan","display_name":"Dan"}`},
		{"POST", "/api/v1/groups", `{"name":"team-ops","members":["alice"]}`},
		{"DELETE", "/api/v1/users/alice", ""},
	} {
		if w := in.ask(t, r.method, r.path, in.carol, r.body, nil); w.Code != http.StatusInternalServerError {
			t.Errorf("%s %s with the audit log refusing answered %d: %s", r.method, r.path, w.Code, w.Body)
		}
	}
	for q, want := range map[string]int{
		`select count(*) from users where login in ('dan', 'alice')`: 1,
		`select count(*) from enrolment_codes`:                       0,
		`select count(*) from groups`:                                0,
	} {
		if n := in.count(t, q); n != want {
			t.Errorf("%s answers %d once the audit log refused, want %d", q, n, want)
		}
	}
}

// The bootstrap token's end is asked again in the transaction of every act, so that a request
// authorised a moment before the first administrator enrolled changes nothing once they have: here
// the router is told the token is still the operator's, as it was when a request arriving at that
// moment was authorised.
func TestTheBootstrapEndingWhileARequestIsServedEndsWhatItMayDo(t *testing.T) {
	in := somePeople(t)
	rt, err := api.NewRouter(everything{who: "operator"}, func(*http.Request) (api.Identity, error) {
		return api.Identity{Principal: api.BootstrapOperator}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: in.pool, PublicURL: "https://agentiik.example.com", Now: func() time.Time { return in.now }}); err != nil {
		t.Fatal(err)
	}
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "dan", DisplayName: "Dan", Admin: true}); err != nil {
			return err
		}
		_, err := w.EndBootstrap(ctx, in.now)
		return err
	})
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "erin", DisplayName: "Erin"}); err != nil {
			return err
		}
		if err := w.CreateGroup(ctx, "old-team"); err != nil {
			return err
		}
		_, err := w.AddMember(ctx, "old-team", "alice")
		return err
	})
	for _, r := range []struct{ method, path, body string }{
		{"POST", "/api/v1/users", `{"login":"frank","display_name":"Frank"}`},
		{"POST", "/api/v1/users", `{"login":"dan","display_name":"Dan","admin":true}`},
		{"POST", "/api/v1/users/dan/enrolment", ""},
		{"POST", "/api/v1/users/erin/enrolment", ""},
		{"DELETE", "/api/v1/users/erin", ""},
		{"POST", "/api/v1/groups", `{"name":"team-ops","members":["alice"]}`},
		{"PUT", "/api/v1/groups/old-team/members/erin", ""},
		{"DELETE", "/api/v1/groups/old-team/members/alice", ""},
		{"DELETE", "/api/v1/groups/old-team", ""},
	} {
		w := sent(t, rt, r.method, r.path, "operator", r.body)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "first administrator enrolled") || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s %s %s by the operator once the bootstrap ended answered %d: %s", r.method, r.path, r.body, w.Code, w.Body)
		}
	}
	for q, want := range map[string]int{
		`select count(*) from users where login in ('frank', 'erin')`: 1,
		`select count(*) from enrolment_codes`:                        0,
		`select count(*) from groups`:                                 1,
		`select count(*) from group_members`:                          1,
		`select count(*) from audit_log`:                              0,
	} {
		if n := in.count(t, q); n != want {
			t.Errorf("once the bootstrap ended, %s answers %d, want %d", q, n, want)
		}
	}
}

// Two requests creating one new login at once are one user: the second waits on the first's
// insert, finds the user it made, and answers as the same request made again, with a fresh link
// that leaves the first one's revoked, rather than refusing what was the same request twice.
//
// The first is held on the lock that keeps logins and namespaces apart until the second is waiting
// on the first's row, the moment the two collide, so that the test does not depend on the
// scheduler happening to interleave them.
func TestTwoRequestsCreatingOneUserAtOnceMakeOneUserAndOneOpenLink(t *testing.T) {
	in := somePeople(t)
	holder := dbtest.Superuser(t, in.super)
	tx, err := holder.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `select pg_advisory_xact_lock(474080961907)`); err != nil {
		t.Fatal(err)
	}
	codes := make(chan int, 2)
	for range 2 {
		go func() {
			codes <- sent(t, in.h, "POST", "/api/v1/users", in.carol, `{"login":"dan","display_name":"Dan"}`).Code
		}()
	}
	watcher := dbtest.Superuser(t, in.super)
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := watcher.QueryRow(t.Context(),
			`select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the two requests never waited on each other: %d waiting", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := []int{<-codes, <-codes}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusOK, http.StatusCreated}) {
		t.Errorf("two requests creating dan at once answered %v", got)
	}
	if n := in.count(t, `select count(*) from enrolment_codes where login = 'dan' and revoked_at is null`); n != 1 {
		t.Errorf("two requests creating dan at once left %d links open", n)
	}
}

// waitForLocks waits until n transactions of this test's database wait on a lock, and answers an
// error after ten seconds. It fails nothing itself, since it is called inside a transaction, which
// has to end for the test to: one left open by a test that stopped there holds a connection the
// pool waits for at cleanup, for as long as the test binary is allowed to run.
func (in people) waitForLocks(t *testing.T, n int) error {
	t.Helper()
	watcher := dbtest.Superuser(t, in.super)
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := watcher.QueryRow(t.Context(),
			`select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting >= n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d transactions wait on a lock, and %d were expected to", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An act takes its rows' locks before it appends to the audit log, whose head is one row for the
// whole installation: a fresh link asked for alice, and dan removed with his personal namespace,
// while another transaction holds their rows and has yet to append its own entry, wait for it,
// rather than holding the head that transaction needs and being refused as a deadlock.
func TestAnActLocksItsRowsBeforeItAppendsToTheAuditLog(t *testing.T) {
	in := somePeople(t)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "dan", DisplayName: "Dan"}); err != nil {
			return err
		}
		return w.CreateGroup(ctx, "team-finance")
	})
	in.exec(t, `insert into namespaces (name, kind, owner) values ('dan', 'personal', 'dan')`)

	for _, r := range []struct {
		method, path, body, member string
		want                       int
	}{
		{"POST", "/api/v1/users", `{"login":"alice","display_name":"Alice"}`, "alice", http.StatusOK},
		{"DELETE", "/api/v1/users/dan", "", "dan", http.StatusNoContent},
	} {
		answered := make(chan *httptest.ResponseRecorder, 1)
		err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			// The member's row, held as a membership change holds it until its entry is
			// appended and it commits.
			if _, err := w.AddMember(ctx, "team-finance", r.member); err != nil {
				return err
			}
			go func() { answered <- sent(t, in.h, r.method, r.path, in.carol, r.body) }()
			if err := in.waitForLocks(t, 1); err != nil {
				return err
			}
			return w.Audit(ctx, audit.Record{Actor: "carol", Action: audit.GroupMemberAdd, Target: "group:team-finance", Result: audit.Done})
		})
		if err != nil {
			t.Errorf("a membership change holding %s's row could not append its entry while %s %s waited: %s", r.member, r.method, r.path, err)
		}
		if w := <-answered; w.Code != r.want {
			t.Errorf("%s %s, waiting on %s's row, answered %d: %s", r.method, r.path, r.member, w.Code, w.Body)
		}
	}
}

// A link asked for a user whose enrolment is committing at that moment waits for it and is refused,
// rather than issued beside the passkey it made: whether the user holds a credential is read once
// their row is locked.
func TestALinkAskedWhileTheUserEnrolsIsRefused(t *testing.T) {
	in := somePeople(t)
	answered := make(chan *httptest.ResponseRecorder, 1)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		if err := w.AddCredential(ctx, db.Credential{ID: "cGFzc2tleS1hbGljZQ", Login: "alice", Type: db.CredentialPasskey,
			PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
			return err
		}
		go func() { answered <- sent(t, in.h, "POST", "/api/v1/users/alice/enrolment", in.carol, "") }()
		return in.waitForLocks(t, 1)
	})
	if w := <-answered; w.Code != http.StatusConflict {
		t.Errorf("a link asked while alice enrolled answered %d: %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from enrolment_codes where login = 'alice'`); n != 0 {
		t.Errorf("a link was issued beside alice's passkey: %d", n)
	}
}

// Once the bootstrap token has ended, the last administrator who can sign in, not suspended and
// holding a credential, is not removed, since nothing makes an administrator after that; before it
// has ended, the token makes another and the removal goes through. Two administrators removing each
// other at once take turns, and the second finds itself the last.
func TestTheLastAdministratorWhoCanSignInIsNotRemoved(t *testing.T) {
	in := somePeople(t)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{
			{Login: "dan", DisplayName: "Dan", Admin: true}, {Login: "erin", DisplayName: "Erin", Admin: true},
			{Login: "frank", DisplayName: "Frank", Admin: true, Suspended: true}, {Login: "gina", DisplayName: "Gina", Admin: true},
		} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		return nil
	})
	if w := in.ask(t, "DELETE", "/api/v1/users/dan", in.carol, "", nil); w.Code != http.StatusNoContent {
		t.Errorf("removing dan, the bootstrap token still live, answered %d: %s", w.Code, w.Body)
	}

	for _, login := range []string{"carol", "frank", "gina"} {
		in.enrol(t, login)
	}
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, in.now)
		return err
	})
	gina := in.token(t, "gina", nil, nil)

	// carol and gina remove each other at once, while carol's row is held: one goes, and the
	// other is then the last who can sign in, frank being suspended and erin holding nothing.
	holder := dbtest.Superuser(t, in.super)
	tx, err := holder.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `select from users where login = 'carol' for update`); err != nil {
		t.Fatal(err)
	}
	codes := make(chan int, 2)
	go func() { codes <- sent(t, in.h, "DELETE", "/api/v1/users/gina", in.carol, "").Code }()
	go func() { codes <- sent(t, in.h, "DELETE", "/api/v1/users/carol", gina, "").Code }()
	// Released before anything fails, so that the two requests end with the test.
	waited := in.waitForLocks(t, 2)
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if waited != nil {
		t.Fatal(waited)
	}
	got := []int{<-codes, <-codes}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusNoContent, http.StatusConflict}) {
		t.Errorf("two administrators removing each other at once answered %v", got)
	}
	var left string
	if err := dbtest.Superuser(t, in.super).QueryRow(t.Context(),
		`select string_agg(login, ',' order by login) from users where admin and login in ('carol', 'gina')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	remover := in.carol
	if left == "gina" {
		remover = gina
	}
	w := in.ask(t, "DELETE", "/api/v1/users/"+left, remover, "", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), left+" is the last administrator who can sign in") {
		t.Errorf("%s removing themselves, the last who can sign in, answered %d: %s", left, w.Code, w.Body)
	}

	// Anybody else goes, and an administrator who could sign in counts once enrolled.
	if w := in.ask(t, "DELETE", "/api/v1/users/frank", remover, "", nil); w.Code != http.StatusNoContent {
		t.Errorf("removing frank, suspended, answered %d: %s", w.Code, w.Body)
	}
	in.enrol(t, "erin")
	if w := in.ask(t, "DELETE", "/api/v1/users/"+left, remover, "", nil); w.Code != http.StatusNoContent {
		t.Errorf("%s removing themselves beside erin, enrolled, answered %d: %s", left, w.Code, w.Body)
	}
}

// An act of the bootstrap token and the enrolment that ends the token take turns: a user created
// with the token while the first administrator's enrolment is committing waits for it, and is then
// refused, rather than reading the token live a moment before it ends and committing after.
func TestAnActOfTheBootstrapTokenWaitsForTheEnrolmentThatEndsIt(t *testing.T) {
	in := somePeople(t)
	answered := make(chan *httptest.ResponseRecorder, 1)
	in.wide(t, func(ctx context.Context, w *db.Wide) error {
		if _, err := w.EndBootstrap(ctx, in.now); err != nil {
			return err
		}
		go func() {
			answered <- sent(t, in.h, "POST", "/api/v1/users", in.bootstrap, `{"login":"erin","display_name":"Erin"}`)
		}()
		return in.waitForLocks(t, 1)
	})
	if w := <-answered; w.Code != http.StatusUnauthorized {
		t.Errorf("a user created with the bootstrap token as it ended answered %d: %s", w.Code, w.Body)
	}
	if n := in.count(t, `select count(*) from users where login = 'erin'`); n != 0 {
		t.Error("the bootstrap token created erin as it ended")
	}
}

// A listing with nothing in it is an empty list, as the wire writes one, and never null: the
// bootstrap token's first requests find no user and no group.
func TestAnEmptyListingIsAnEmptyList(t *testing.T) {
	in := somePeople(t)
	in.exec(t, `delete from principals where kind = 'user'`)
	for path, want := range map[string]string{"/api/v1/users": `{"users":[]}`, "/api/v1/groups": `{"groups":[]}`} {
		if w := in.ask(t, "GET", path, in.bootstrap, "", nil); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
			t.Errorf("%s with nothing to list answered %d: %s", path, w.Code, w.Body)
		}
	}
}
