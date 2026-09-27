package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// agk share, agk grants and agk whoami against the real API over PostgreSQL: alice owns finance,
// which holds monthly-invoicing, bob is in team-ops and holds nothing, carol administers the
// installation, and ops holds no grant. The bootstrap token has not ended.

type shareInstallation struct {
	url, alice, bob, carol, bootstrap string
}

func anInstallationToShare(t *testing.T) shareInstallation {
	t.Helper()
	pool, super := dbtest.Open(t)
	for _, stmt := range []string{
		`insert into namespaces (name) values ('finance'), ('ops')`,
		`insert into workflows (namespace, name) values ('finance', 'monthly-invoicing')`,
	} {
		if _, err := dbtest.Superuser(t, super).Exec(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	in := shareInstallation{alice: "agktoken_alice" + strings.Repeat("E", 40), bob: "agktoken_bob" + strings.Repeat("F", 40), carol: "agktoken_carol" + strings.Repeat("G", 40), bootstrap: "agk_op_share"}
	now := time.Now().UTC()
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{{Login: "alice", DisplayName: "Alice"}, {Login: "bob", DisplayName: "Bob"}, {Login: "carol", DisplayName: "Carol", Admin: true}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := w.CreateGroup(ctx, "team-ops"); err != nil {
			return err
		}
		if _, err := w.AddMember(ctx, "team-ops", "bob"); err != nil {
			return err
		}
		bootstrap := sha256.Sum256([]byte(in.bootstrap))
		if _, err := w.SetBootstrapToken(ctx, bootstrap[:]); err != nil {
			return err
		}
		for login, value := range map[string]string{"alice": in.alice, "bob": in.bob, "carol": in.carol} {
			hash := sha256.Sum256([]byte(value))
			if err := w.MintToken(ctx, db.APIToken{ID: ulid.New(), Hash: hash[:], Principal: login, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.In(t.Context(), "finance", func(ctx context.Context, n *db.NS) error {
		return n.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "alice", Scope: access.Scope{Namespace: "finance"}, Role: access.Owner, GrantedBy: "operator"})
	}); err != nil {
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
	if _, err := api.NewSharing(rt, api.SharingOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMe(rt, api.MeOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	in.url = srv.URL
	return in
}

// sharedSaying is what agk share says of a grant it wrote.
var sharedSaying = regexp.MustCompile(`^granted operator on finance/monthly-invoicing to group:team-ops until (\S+): grant ([0-9A-HJKMNP-TV-Z]{26})\n$`)

// An owner shares a workflow with a group, lists who can do what there and from which scope, and
// revokes it; a member of the group reads what they hold with agk whoami before and after.
func TestAWorkflowIsSharedListedAndRevokedWithAgk(t *testing.T) {
	in := anInstallationToShare(t)
	code, out, errs := agkWithToken(t, in.url, in.alice, "share", "finance/monthly-invoicing", "--group", "team-ops", "--role", "operator", "--expires", "30d")
	said := sharedSaying.FindStringSubmatch(out)
	if code != exitSucceeded || said == nil {
		t.Fatalf("agk share left with %d, printing %q: %s", code, out, errs)
	}
	until, id := said[1], said[2]
	if ends, err := time.Parse(time.RFC3339, until); err != nil || ends.Sub(time.Now()) < 29*24*time.Hour || ends.Sub(time.Now()) > 31*24*time.Hour {
		t.Errorf("the grant ends at %s, which is not 30 days from now", until)
	}

	code, out, errs = agkWithToken(t, in.url, in.alice, "grants", "finance/monthly-invoicing")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if code != exitSucceeded || len(lines) != 2 {
		t.Fatalf("agk grants left with %d, printing\n%s%s", code, out, errs)
	}
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}  alice           owner     namespace finance` + strings.Repeat(" ", 19) + `workflow:read, workflow:run, workflow:write, workflow:delete, run:read, run:read_data, secret:use, secret:write, grant:manage$`).MatchString(lines[0]) {
		t.Errorf("agk grants printed the namespace's owner as %q", lines[0])
	}
	if want := id + "  group:team-ops  operator  workflow finance/monthly-invoicing  workflow:run, run:read  until " + until; lines[1] != want {
		t.Errorf("agk grants printed the workflow's grant as\n%q\nwant\n%q", lines[1], want)
	}

	if code, out, errs := agkWithToken(t, in.url, in.bob, "whoami", "finance/monthly-invoicing"); code != exitSucceeded ||
		out != "bob, in group:team-ops\non finance/monthly-invoicing: workflow:run, run:read\n" {
		t.Errorf("agk whoami on the workflow left with %d, printing %q: %s", code, out, errs)
	}
	if code, out, _ := agkWithToken(t, in.url, in.bob, "whoami", "finance"); code != exitSucceeded || out != "bob, in group:team-ops\non finance: nothing\n" {
		t.Errorf("agk whoami on the namespace printed %q", out)
	}
	// A workflow with no grant of its own holds what its namespace gives, unless denies there take
	// all of it, which the installation does not say, since it names no workflow its caller cannot
	// read.
	if code, out, _ := agkWithToken(t, in.url, in.alice, "whoami", "finance/monthly-invoicing"); code != exitSucceeded ||
		out != "alice\non finance/monthly-invoicing: workflow:read, workflow:run, workflow:write, workflow:delete, run:read, run:read_data, secret:use, secret:write, grant:manage, from finance, unless denies there take all of it\n" {
		t.Errorf("agk whoami on a workflow alice owns through its namespace printed %q", out)
	}
	// A namespace nobody shares lists nothing, and says so.
	if code, out, errs := agkWithToken(t, in.url, in.bootstrap, "grants", "ops"); code != exitSucceeded || out != "" || errs != "no grant on ops\n" {
		t.Errorf("agk grants on a namespace with none left with %d, printing %q and saying %q", code, out, errs)
	}
	if code, out, _ := agkWithToken(t, in.url, in.bob, "whoami"); code != exitSucceeded || out != "bob, in group:team-ops\non finance/monthly-invoicing: workflow:run, run:read\n" {
		t.Errorf("agk whoami printed %q", out)
	}
	code, out, _ = agkWithToken(t, in.url, in.bob, "whoami", "-o", "json")
	var me api.Me
	if code != exitSucceeded || json.Unmarshal([]byte(out), &me) != nil || me.Principal != "bob" || me.User == nil {
		t.Errorf("agk whoami -o json left with %d, printing %s", code, out)
	}

	code, out, errs = agkWithToken(t, in.url, in.alice, "share", "finance/monthly-invoicing", "--revoke", id)
	if code != exitSucceeded || out != "revoked grant "+id+" on finance/monthly-invoicing: it gives nothing from the next request and the next run\n" {
		t.Fatalf("agk share --revoke left with %d, printing %q: %s", code, out, errs)
	}
	if code, out, _ := agkWithToken(t, in.url, in.bob, "whoami", "finance/monthly-invoicing"); code != exitSucceeded || out != "bob, in group:team-ops\non finance/monthly-invoicing: nothing\n" {
		t.Errorf("once revoked, agk whoami printed %q", out)
	}
	code, _, errs = agkWithToken(t, in.url, in.alice, "share", "finance/monthly-invoicing", "--revoke", id)
	if code != exitRefused || errs != "no grant "+id+" written on finance/monthly-invoicing, or not yours to revoke\n" {
		t.Errorf("revoking it again left with %d: %q", code, errs)
	}

	// A deny, and an administrator's grant to themselves, which alice, owning finance, is told.
	if code, out, _ := agkWithToken(t, in.url, in.alice, "share", "finance", "--user", "bob", "--deny", "run:read"); code != exitSucceeded ||
		!regexp.MustCompile(`^denied run:read on finance to bob: grant [0-9A-HJKMNP-TV-Z]{26}\n$`).MatchString(out) {
		t.Errorf("agk share --deny left with %d, printing %q", code, out)
	}
	if code, _, errs := agkWithToken(t, in.url, in.carol, "share", "finance", "--user", "carol", "--role", "viewer"); code != exitSucceeded {
		t.Fatalf("an administrator sharing finance with herself left with %d: %s", code, errs)
	}
	code, out, _ = agkWithToken(t, in.url, in.alice, "whoami")
	if code != exitSucceeded || !regexp.MustCompile(`\ntold [0-9A-HJKMNP-TV-Z]{26} at \S+: carol, an administrator, granted viewer on finance to carol\n$`).MatchString(out) {
		t.Errorf("alice's agk whoami printed\n%s", out)
	}
	if code, out, _ := agkWithToken(t, in.url, in.carol, "whoami"); code != exitSucceeded || out != "carol, an administrator\non finance: workflow:read, run:read\n" {
		t.Errorf("carol's agk whoami printed %q", out)
	}
}

// What agk share refuses before it asks, exit 2, and what the installation refuses, exit 1 in its
// words or the command line's.
func TestAgkShareRefuses(t *testing.T) {
	in := anInstallationToShare(t)
	for _, c := range []struct {
		what string
		args []string
		code int
		says string
	}{
		{"no scope", []string{"share", "--user", "bob", "--role", "viewer"}, exitUsage, "names one namespace or workflow"},
		{"a scope no grant names", []string{"share", "Finance", "--user", "bob", "--role", "viewer"}, exitUsage, "is not a namespace"},
		{"no principal", []string{"share", "finance", "--role", "viewer"}, exitUsage, "names one principal"},
		{"two principals", []string{"share", "finance", "--user", "bob", "--group", "team-ops", "--role", "viewer"}, exitUsage, "names one principal"},
		{"a service account with no namespace", []string{"share", "finance", "--service-account", "nightly", "--role", "viewer"}, exitUsage, "written NS/NAME"},
		{"a role and a deny", []string{"share", "finance", "--user", "bob", "--role", "viewer", "--deny", "run:read"}, exitUsage, "one of the two"},
		{"neither", []string{"share", "finance", "--user", "bob"}, exitUsage, "one of the two"},
		{"a revocation saying more", []string{"share", "finance", "--revoke", "01M2AC5K0N2Q4S6T8W0Y2A4C6E", "--role", "viewer"}, exitUsage, "and nothing else"},
		{"an expiry of no time", []string{"share", "finance", "--user", "bob", "--role", "viewer", "--expires", "0d"}, exitUsage, "a grant ends after it is written"},
		{"a role that is not one", []string{"share", "finance", "--user", "bob", "--role", "admin"}, exitRefused, "is not a role"},
		{"nobody", []string{"share", "finance", "--user", "nobody", "--role", "viewer"}, exitRefused, "principal nobody names nobody"},
		{"a namespace not there", []string{"share", "nowhere", "--user", "bob", "--role", "viewer"}, exitRefused, "no nowhere you may share"},
		{"grants of two scopes", []string{"grants", "finance", "hr"}, exitUsage, "names one namespace or workflow"},
		{"whoami on two", []string{"whoami", "finance", "hr"}, exitUsage, "at most"},
	} {
		code, _, errs := agkWithToken(t, in.url, in.alice, c.args...)
		if code != c.code || !strings.Contains(errs, c.says) {
			t.Errorf("%s left with %d, saying %q", c.what, code, errs)
		}
	}
	// bob shares nothing, and is told so as the absence it is.
	if code, _, errs := agkWithToken(t, in.url, in.bob, "grants", "finance"); code != exitRefused || errs != "no finance you may share: it is not there, or you hold no grant:manage on it\n" {
		t.Errorf("bob listing finance's grants left with %d: %q", code, errs)
	}
	if code, _, errs := agkWithToken(t, in.url, "agktoken_nobody"+strings.Repeat("H", 40), "whoami"); code != exitRefused || !strings.Contains(errs, "did not accept the credential") {
		t.Errorf("an unknown token's whoami left with %d: %q", code, errs)
	}
}
