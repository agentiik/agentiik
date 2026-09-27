package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// agk user and agk group against the real API over PostgreSQL, authorised by the installation's own
// Principals: the first administrator created with the bootstrap token, then users and groups
// managed by an administrator, and what agk says when it is refused or reaches nothing.

// administered is an installation with one administrator, carol, and one user, alice, each holding
// an API token, and a bootstrap token that has not ended.
type administered struct {
	url       string
	pool      *db.Pool
	bootstrap string
	carol     string
	alice     string
}

func anAdministeredInstallation(t *testing.T) administered {
	t.Helper()
	pool, _ := dbtest.Open(t)
	in := administered{
		pool: pool, bootstrap: "3f1c8a0e" + strings.Repeat("9", 56),
		carol: "agktoken_carol" + strings.Repeat("A", 40), alice: "agktoken_alice" + strings.Repeat("B", 40),
	}
	now := time.Now().UTC()
	hash := func(v string) []byte { sum := sha256.Sum256([]byte(v)); return sum[:] }
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{{Login: "carol", DisplayName: "Carol", Admin: true}, {Login: "alice", DisplayName: "Alice"}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		for login, value := range map[string]string{"carol": in.carol, "alice": in.alice} {
			if err := w.MintToken(ctx, db.APIToken{ID: ulid.New(), Hash: hash(value), Principal: login, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		_, err := w.SetBootstrapToken(ctx, hash(in.bootstrap))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	principals, err := api.NewPrincipals(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(principals, principals.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewUsers(rt, api.UserOptions{Pool: pool, PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	in.url = srv.URL
	return in
}

// agk runs one command line against the installation with token in AGENTIIK_TOKEN.
func (in administered) agk(t *testing.T, token string, args ...string) (int, string, string) {
	t.Helper()
	return agkAt(t, in.url, token, args...)
}

// agkAt runs one command line against the installation at url.
func agkAt(t *testing.T, url, token string, args ...string) (int, string, string) {
	t.Helper()
	out, errs := &strings.Builder{}, &strings.Builder{}
	code := run(t.Context(), Env{
		Out: out, Err: errs, Dir: t.TempDir(),
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return token
			case serverVariable:
				return url
			}
			return ""
		},
	}, args)
	return code, out.String(), errs.String()
}

// aLink is an enrolment link on the installation's public URL, alone on its line.
var aLink = regexp.MustCompile(`(?m)^https://agentiik\.example\.com/auth/enrol#agkenrol_[A-Za-z0-9_-]{43,}$`)

// The first administrator is created with the bootstrap token and the link printed, with the line
// that says the token ends at their enrolment; run again, a fresh link, saying the one before no
// longer works. A user who is not an administrator, or an administrator created with an API token,
// is printed the link alone.
func TestTheFirstAdministratorIsCreatedWithTheBootstrapTokenAndPrintedTheirLink(t *testing.T) {
	in := anAdministeredInstallation(t)
	code, out, errs := in.agk(t, in.bootstrap, "user", "create", "dan", "--admin", "--display-name", "Dan Martin")
	if code != exitSucceeded {
		t.Fatalf("agk user create dan --admin left with %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	before := regexp.MustCompile(`^dan is an administrator with no credential yet\. Open this link once, before \d\d:\d\d UTC, to enrol a passkey, or a password where the installation allows one:$`)
	if len(lines) != 3 || !before.MatchString(lines[0]) || !aLink.MatchString(lines[1]) ||
		lines[2] != "the bootstrap token works until dan has enrolled; then sign in as dan with agk login" {
		t.Fatalf("agk user create dan --admin printed:\n%s", out)
	}

	code, again, errs := in.agk(t, in.bootstrap, "user", "create", "dan", "--admin", "--display-name", "Dan Martin")
	if code != exitSucceeded || !strings.HasPrefix(again, "dan is an administrator who has not enrolled yet, and the link issued before no longer works. Open this one once") {
		t.Fatalf("agk user create dan --admin again left with %d:\n%s%s", code, again, errs)
	}
	if aLink.FindString(again) == lines[1] || aLink.FindString(again) == "" {
		t.Errorf("agk user create dan --admin again printed the same link, or none:\n%s", again)
	}

	for _, c := range []struct {
		token string
		args  []string
		first string
	}{
		{in.bootstrap, []string{"user", "create", "erin"}, "erin is a user with no credential yet."},
		{in.carol, []string{"user", "create", "frank", "--admin"}, "frank is an administrator with no credential yet."},
	} {
		code, out, errs := in.agk(t, c.token, c.args...)
		if code != exitSucceeded || !strings.HasPrefix(out, c.first) || !aLink.MatchString(out) || strings.Contains(out, "bootstrap token") {
			t.Errorf("agk %s left with %d:\n%s%s", strings.Join(c.args, " "), code, out, errs)
		}
	}

	// Run again with the login alone, or with --admin alone, dan is kept as he was created and
	// printed a fresh link; with another display name, refused in the installation's words.
	for _, args := range [][]string{{"user", "create", "dan"}, {"user", "create", "dan", "--admin"}} {
		code, out, errs := in.agk(t, in.bootstrap, args...)
		if code != exitSucceeded || !strings.HasPrefix(out, "dan is an administrator who has not enrolled yet") || !strings.Contains(out, "the bootstrap token works until dan has enrolled") {
			t.Errorf("agk %s left with %d:\n%s%s", strings.Join(args, " "), code, out, errs)
		}
	}
	code, _, errs = in.agk(t, in.bootstrap, "user", "create", "dan", "--display-name", "Dan")
	if code != exitRefused || !strings.Contains(errs, "another display name or admin") {
		t.Errorf("dan with another display name left with %d: %s", code, errs)
	}
	if code, _, errs := in.agk(t, in.bootstrap, "user", "create", "dan", "--display-name", ""); code != exitUsage {
		t.Errorf("an empty --display-name left with %d: %s", code, errs)
	}

	// A user created with no display name reads as their login.
	var erin api.User
	if code, out, _ := in.agk(t, in.carol, "user", "show", "erin", "-o", "json"); code != exitSucceeded || json.Unmarshal([]byte(out), &erin) != nil || erin.DisplayName != "erin" {
		t.Errorf("erin, created with no display name, reads as %d %s", code, out)
	}
}

// An administrator issues a user a recovery code, printed as the link that carries it with the
// minute it lapses at, and a fresh one each time. Their own account is refused in the installation's
// sentence, which says who issues it instead, rather than as somebody who does not administer; so is
// a service account, which is no user; a user who does not administer is told that; and a login no
// user has is no user.
func TestAgkUserRecoverPrintsARecoveryCodesLink(t *testing.T) {
	in := anAdministeredInstallation(t)
	code, out, errs := in.agk(t, in.carol, "user", "recover", "alice")
	if code != exitSucceeded {
		t.Fatalf("agk user recover alice left with %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	before := regexp.MustCompile(`^alice may open this link once, before \d\d:\d\d UTC, to enrol a new passkey, or a password where the installation allows one; any recovery code issued them before no longer works\. Hand it over yourself:$`)
	if len(lines) != 2 || !before.MatchString(lines[0]) || !aLink.MatchString(lines[1]) {
		t.Fatalf("agk user recover alice printed:\n%s", out)
	}
	if _, again, _ := in.agk(t, in.carol, "user", "recover", "alice"); aLink.FindString(again) == lines[1] || aLink.FindString(again) == "" {
		t.Errorf("agk user recover alice again printed the same link, or none:\n%s", again)
	}

	for _, c := range []struct {
		token string
		login string
		says  string
		not   string
	}{
		{in.carol, "carol", api.SelfRecovery, "an administrator's to manage"},
		{in.carol, "finance/robot", "a service account is no user", "no user finance/robot"},
		{in.alice, "carol", "users and groups are an administrator's to manage", ""},
		{in.carol, "nobody", "no user nobody", ""},
	} {
		code, out, errs := in.agk(t, c.token, "user", "recover", c.login)
		if code != exitRefused || out != "" || !strings.Contains(errs, c.says) || (c.not != "" && strings.Contains(errs, c.not)) {
			t.Errorf("agk user recover %s left with %d, printed %q and said %q, want it to say %q", c.login, code, out, errs, c.says)
		}
	}
	for _, args := range [][]string{{"user", "recover"}, {"user", "recover", "alice", "bob"}} {
		if code, _, errs := in.agk(t, in.carol, args...); code != exitUsage {
			t.Errorf("agk %s left with %d: %s", strings.Join(args, " "), code, errs)
		}
	}
}

// An administrator lists, reads and removes users, and creates, reads, fills, empties and removes
// groups, each verb saying what it did in one line.
func TestAnAdministratorManagesUsersAndGroupsWithAgk(t *testing.T) {
	in := anAdministeredInstallation(t)
	if code, _, errs := in.agk(t, in.carol, "user", "create", "bob-martin", "--display-name", "Bob Martin"); code != exitSucceeded {
		t.Fatalf("creating bob-martin left with %d: %s", code, errs)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"user", "list"}, "alice       Alice\nbob-martin  Bob Martin\ncarol       Carol       administrator\n"},
		{[]string{"group", "list"}, "no group yet\n"},
		{[]string{"group", "create", "team-finance", "alice"}, "created group team-finance, with alice\n"},
		{[]string{"group", "create", "finance-leads"}, "created group finance-leads, with no member yet\n"},
		{[]string{"group", "add", "team-finance", "bob-martin"}, "bob-martin is in team-finance, which now holds alice, bob-martin\n"},
		{[]string{"group", "add", "team-finance", "bob-martin"}, "bob-martin is in team-finance, which now holds alice, bob-martin\n"},
		{[]string{"group", "remove", "team-finance", "alice"}, "alice is not in team-finance, which now holds bob-martin\n"},
		{[]string{"group", "list"}, "finance-leads  no member\nteam-finance   bob-martin\n"},
		{[]string{"group", "show", "team-finance"}, "group:team-finance: bob-martin\n"},
		{[]string{"group", "delete", "team-finance"}, "removed group team-finance, with its memberships and grants; its members stay\n"},
		{[]string{"user", "delete", "bob-martin"}, "removed user bob-martin, with their credentials, tokens, sessions, enrolment links, memberships and grants, and their personal namespace where it was empty\n"},
	} {
		code, out, errs := in.agk(t, in.carol, c.args...)
		if code != exitSucceeded || out != c.want {
			t.Errorf("agk %s left with %d and printed:\n%s%s\nwant:\n%s", strings.Join(c.args, " "), code, out, errs, c.want)
		}
	}

	code, out, errs := in.agk(t, in.carol, "user", "show", "alice")
	shown := regexp.MustCompile(`^alice \(Alice\): a user\ncreated at \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ, never signed in\n$`)
	if code != exitSucceeded || !shown.MatchString(out) {
		t.Errorf("agk user show alice left with %d and printed:\n%s%s", code, out, errs)
	}
	var listing struct {
		Users []api.User `json:"users"`
	}
	if code, out, _ := in.agk(t, in.carol, "user", "list", "-o", "json"); code != exitSucceeded || json.Unmarshal([]byte(out), &listing) != nil || len(listing.Users) != 2 {
		t.Errorf("agk user list -o json left with %d and printed:\n%s", code, out)
	}

	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"user", "show", "bob-martin"}, "no user bob-martin"},
		{[]string{"user", "delete", "bob-martin"}, "no user bob-martin"},
		{[]string{"group", "show", "team-finance"}, "no group team-finance"},
		{[]string{"group", "add", "finance-leads", "nobody"}, "no user nobody"},
		{[]string{"group", "add", "nothing", "alice"}, "no group nothing"},
		{[]string{"group", "create", "finance-leads"}, "exists already"},
		{[]string{"group", "create", "team-ops", "nobody"}, "nobody is no user"},
		{[]string{"user", "create", "Frank"}, "is not a login"},
	} {
		code, out, errs := in.agk(t, in.carol, c.args...)
		if code != exitRefused || out != "" || !strings.Contains(errs, c.says) {
			t.Errorf("agk %s left with %d, printed %q and said %q, want it to say %q", strings.Join(c.args, " "), code, out, errs, c.says)
		}
	}
}

// A verb is refused in the installation's words and exit 1 when the token is not an
// administrator's or the bootstrap token has ended; a command line naming the wrong number of
// things, or no installation, is exit 2; an installation that answers nothing is exit 4.
func TestAgkSaysWhyAUserOrGroupVerbCameToNothing(t *testing.T) {
	in := anAdministeredInstallation(t)
	code, _, errs := in.agk(t, in.alice, "user", "list")
	if code != exitRefused || !strings.Contains(errs, "you do not hold what this needs: users and groups are an administrator's to manage") {
		t.Errorf("a user who does not administer listing the users left with %d: %s", code, errs)
	}

	for _, args := range [][]string{
		{"user", "create"},
		{"user", "create", "a", "b"},
		{"user", "show"},
		{"user", "list", "carol"},
		{"user", "list", "-o", "yaml"},
		{"group", "add", "team-finance"},
		{"group", "create"},
		{"group", "remove", "a", "b", "c"},
		{"user", "frobnicate"},
		{"group"},
	} {
		if code, _, errs := in.agk(t, in.carol, args...); code != exitUsage {
			t.Errorf("agk %s left with %d: %s", strings.Join(args, " "), code, errs)
		}
	}
	if _, _, errs := in.agk(t, in.carol, "user", "frobnicate"); !strings.Contains(errs, "user frobnicate: there is no such command") {
		t.Errorf("an unknown verb of user was named as %q", errs)
	}
	if code, _, errs := agkAt(t, "", in.carol, "user", "list"); code != exitUsage || !strings.Contains(errs, "no installation") {
		t.Errorf("no installation left with %d: %s", code, errs)
	}

	// An address nothing answers at.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := "http://" + ln.Addr().String()
	ln.Close()
	if code, _, errs := agkAt(t, gone, in.carol, "user", "create", "dan"); code != exitNoOutcome || !strings.Contains(errs, "could not be reached") {
		t.Errorf("an installation that answers nothing left with %d: %s", code, errs)
	}

	// Once the first administrator has enrolled, the bootstrap token creates nobody, and the
	// installation says why.
	if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		_, err := w.EndBootstrap(ctx, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := in.agk(t, in.bootstrap, "user", "create", "dan", "--admin"); code != exitRefused || !strings.Contains(errs, "first administrator signed in") {
		t.Errorf("the ended bootstrap token creating dan left with %d: %s", code, errs)
	}
}

// An installation, or a gateway in front of it, that answers it could not answer now is no outcome,
// exit 4, and never a refusal: a gateway's 502 in front of an API that had already acted would
// otherwise tell a script, "refused, and nothing ran", that a user removed was still there.
func TestAnAnswerThatCouldNotBeGivenIsNoOutcome(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		for _, args := range [][]string{
			{"user", "create", "dan", "--admin"}, {"user", "delete", "dan"}, {"user", "list"},
			{"group", "add", "team-finance", "dan"}, {"group", "delete", "team-finance"},
		} {
			code, out, errs := agkAt(t, srv.URL, "agktoken_"+strings.Repeat("C", 43), args...)
			if code != exitNoOutcome || out != "" || !strings.Contains(errs, "whether it did what was asked is not known") {
				t.Errorf("agk %s answered %d left with %d, printed %q and said %q", strings.Join(args, " "), status, code, out, errs)
			}
		}
		srv.Close()
	}
}

// An installation that serves no route for users and groups, one from before v0.3.0, is said to be
// one, where a verb naming nothing on the path would otherwise read its 404 as no such user; and a
// 429 is a refusal, nothing having been done.
func TestAnInstallationWithNoSuchRouteOrTooManyRequestsIsARefusal(t *testing.T) {
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	for _, args := range [][]string{
		{"user", "create", "alice", "--admin"}, {"group", "create", "team-finance", "alice"}, {"user", "list"}, {"group", "list"},
	} {
		code, out, errs := agkAt(t, missing.URL, "agktoken_"+strings.Repeat("C", 43), args...)
		if code != exitRefused || out != "" || !strings.Contains(errs, "predates v0.3.0") || strings.Contains(errs, "no user") || strings.Contains(errs, "no group") {
			t.Errorf("agk %s against no such route left with %d, printed %q and said %q", strings.Join(args, " "), code, out, errs)
		}
	}
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	if code, _, errs := agkAt(t, limited.URL, "agktoken_"+strings.Repeat("C", 43), "user", "delete", "dan"); code != exitRefused {
		t.Errorf("a 429 left with %d: %s", code, errs)
	}
}
