package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// agk service-account against the real API over PostgreSQL, authorised by the installation's own
// Principals: alice owns finance and team-ops, each with its built-in identity, and bob holds
// nothing.

// accountsInstallation is the address of such an installation, and alice's and bob's tokens.
type accountsInstallation struct {
	url, alice, bob string
}

func anInstallationWithServiceAccounts(t *testing.T) accountsInstallation {
	t.Helper()
	pool, _ := dbtest.Open(t)
	in := accountsInstallation{alice: "agktoken_alice" + strings.Repeat("E", 40), bob: "agktoken_bob" + strings.Repeat("F", 40)}
	now := time.Now().UTC()
	err := pool.Installation(t.Context(), db.NamespaceAdministration, func(ctx context.Context, w *db.Wide) error {
		for _, ns := range []string{"finance", "team-ops"} {
			if _, err := w.CreateNamespace(ctx, db.Namespace{Name: ns}); err != nil {
				return err
			}
		}
		for login, value := range map[string]string{"alice": in.alice, "bob": in.bob} {
			if err := w.CreateUser(ctx, db.User{Login: login}); err != nil {
				return err
			}
			hash := sha256.Sum256([]byte(value))
			if err := w.MintToken(ctx, db.APIToken{ID: ulid.New(), Hash: hash[:], Principal: login, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
		}
		for _, ns := range []string{"finance", "team-ops"} {
			if err := w.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "alice", Scope: access.Scope{Namespace: ns}, Role: access.Owner, GrantedBy: "installation"}); err != nil {
				return err
			}
		}
		return nil
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
	if _, err := api.NewServiceAccounts(rt, api.ServiceAccountOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewTokens(rt, api.TokenOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	in.url = srv.URL
	return in
}

// A service account is created by its NS/NAME, listed, across the namespaces alice owns or within
// the one named, as a line each or as the installation's answer, and minted a token with the name it
// was created under; removed, it is gone from the list and its token opens nothing.
func TestAServiceAccountIsCreatedListedAndRemovedWithAgk(t *testing.T) {
	in := anInstallationWithServiceAccounts(t)
	code, out, errs := agkWithToken(t, in.url, in.alice, "service-account", "create", "finance/nightly-sync")
	if code != exitSucceeded || out != "created service account finance/nightly-sync, which holds no grant until one is given it, and no token until agk token create --for finance/nightly-sync mints one\n" {
		t.Fatalf("agk service-account create left with %d, printing %q: %s", code, out, errs)
	}
	code, out, errs = agkWithToken(t, in.url, in.alice, "service-account", "create", "team-ops/deploy", "-o", "json")
	var made api.ServiceAccount
	if code != exitSucceeded || json.Unmarshal([]byte(out), &made) != nil || made.Namespace != "team-ops" || made.Name != "deploy" || made.CreatedBy != "alice" {
		t.Fatalf("agk service-account create -o json left with %d, printing %q: %s", code, out, errs)
	}

	code, out, errs = agkWithToken(t, in.url, in.alice, "service-account", "list")
	lines := regexp.MustCompile(`(?m)^(\S+) +(.*)$`).FindAllStringSubmatch(out, -1)
	var names []string
	for _, l := range lines {
		names = append(names, l[1])
	}
	if code != exitSucceeded || strings.Join(names, " ") != "finance/agentiik finance/nightly-sync team-ops/agentiik team-ops/deploy" ||
		lines[0][2] != "built-in: the runs nobody started are attributed to it" || !strings.HasPrefix(lines[1][2], "created by alice at ") {
		t.Errorf("agk service-account list left with %d, printing:\n%s%s", code, out, errs)
	}
	code, out, _ = agkWithToken(t, in.url, in.alice, "service-account", "list", "team-ops", "-o", "json")
	var listed api.ServiceAccountList
	if code != exitSucceeded || json.Unmarshal([]byte(out), &listed) != nil || len(listed.ServiceAccounts) != 2 || listed.ServiceAccounts[1].Name != "deploy" {
		t.Errorf("agk service-account list team-ops -o json left with %d, printing:\n%s", code, out)
	}
	if code, out, errs := agkWithToken(t, in.url, in.bob, "service-account", "list"); code != exitSucceeded || out != "" || !strings.Contains(errs, "you own no namespace") {
		t.Errorf("bob's list left with %d, printing %q: %s", code, out, errs)
	}

	code, token, errs := agkWithToken(t, in.url, in.alice, "token", "create", "--for", "finance/nightly-sync")
	if code != exitSucceeded {
		t.Fatalf("agk token create --for finance/nightly-sync left with %d: %s", code, errs)
	}
	code, out, errs = agkWithToken(t, in.url, in.alice, "service-account", "delete", "finance/nightly-sync")
	if code != exitSucceeded || out != "removed service account finance/nightly-sync, with its tokens and its grants\n" {
		t.Fatalf("agk service-account delete left with %d, printing %q: %s", code, out, errs)
	}
	if code, _, errs := agkWithToken(t, in.url, strings.TrimSpace(token), "token", "list"); code != exitRefused || !strings.Contains(errs, "did not accept the credential") {
		t.Errorf("the removed service account's token listed tokens leaving with %d: %s", code, errs)
	}
	if code, out, _ := agkWithToken(t, in.url, in.alice, "service-account", "list", "finance"); code != exitSucceeded || strings.Contains(out, "nightly-sync") {
		t.Errorf("finance's list once nightly-sync is gone left with %d:\n%s", code, out)
	}
}

// What the installation refuses is said in its sentence, or the command line's where it says more,
// and left with 1; a command line naming no NS/NAME is a usage error, left with 2. A namespace listed
// that holds nothing the caller may see is said on standard error, and is no failure.
func TestAgkSaysWhyAServiceAccountVerbCameToNothing(t *testing.T) {
	in := anInstallationWithServiceAccounts(t)
	for _, c := range []struct {
		value string
		args  []string
		code  int
		says  string
	}{
		{in.bob, []string{"service-account", "create", "finance/ci"}, exitRefused, "namespace finance does not exist, or is not one you own"},
		{in.alice, []string{"service-account", "create", "finance/agentiik"}, exitRefused, "built-in identity"},
		{in.alice, []string{"service-account", "delete", "finance/agentiik"}, exitRefused, "finance/agentiik is the namespace's built-in identity"},
		{in.bob, []string{"service-account", "delete", "finance/agentiik"}, exitRefused, "no service account finance/agentiik, or not of a namespace you own"},
		{in.alice, []string{"service-account", "delete", "finance/ghost"}, exitRefused, "no service account finance/ghost, or not of a namespace you own"},
		{"agktoken_nobody" + strings.Repeat("G", 40), []string{"service-account", "list"}, exitRefused, "did not accept the credential"},
		{in.bob, []string{"service-account", "list", "finance"}, exitSucceeded, "no service account of namespace finance: it does not exist, or is not one you own"},
		{in.alice, []string{"service-account", "list", "finance", "team-ops"}, exitUsage, "one namespace at most"},
		{in.alice, []string{"service-account", "list", "-o", "yaml"}, exitUsage, "json is the one format"},
		{in.alice, []string{"service-account", "create"}, exitUsage, "names one service account"},
		{in.alice, []string{"service-account", "create", "nightly-sync"}, exitUsage, `"nightly-sync" names no service account`},
		{in.alice, []string{"service-account", "delete", "finance/a/b"}, exitUsage, "names no service account"},
		{in.alice, []string{"service-account", "delete", "/ci"}, exitUsage, "names no service account"},
	} {
		code, out, errs := agkWithToken(t, in.url, c.value, c.args...)
		if code != c.code || out != "" || !strings.Contains(errs, c.says) {
			t.Errorf("agk %s left with %d, want %d saying %q, and printed %q: %s", strings.Join(c.args, " "), code, c.code, c.says, out, errs)
		}
	}
}

// A change answered with a 5xx is no outcome, since the installation may have made it before the
// answer failed, and the sentence says how to read it back; a list answered so changed nothing and
// is a refusal. An installation that does not answer is no outcome, and one serving no such route,
// from before v0.3.0, says so.
func TestAgkServiceAccountTellsNoOutcomeFromARefusal(t *testing.T) {
	status := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(`{"error":"said by the installation"}`))
	}))
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		status int
		args   []string
		code   int
		says   string
	}{
		{http.StatusInternalServerError, []string{"service-account", "create", "finance/ci"}, exitNoOutcome, "whether service account finance/ci was changed cannot be told from it: agk service-account list reads it back"},
		{http.StatusBadGateway, []string{"service-account", "delete", "finance/ci"}, exitNoOutcome, "cannot be told"},
		{http.StatusInternalServerError, []string{"service-account", "list"}, exitRefused, "said by the installation"},
		{http.StatusConflict, []string{"service-account", "create", "finance/ci"}, exitRefused, "said by the installation"},
		{http.StatusNotFound, []string{"service-account", "create", "finance/ci"}, exitRefused, "serves no route for service accounts"},
		{http.StatusNotFound, []string{"service-account", "list"}, exitRefused, "serves no route for service accounts"},
	} {
		status.Store(int64(c.status))
		if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, c.args...); code != c.code || !strings.Contains(errs, c.says) {
			t.Errorf("agk %s answered %d left with %d, want %d saying %q: %s", strings.Join(c.args, " "), c.status, code, c.code, c.says, errs)
		}
	}
	srv.Close()
	for _, args := range [][]string{{"service-account", "create", "finance/ci"}, {"service-account", "list"}} {
		if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, args...); code != exitNoOutcome || !strings.Contains(errs, "could not be reached") {
			t.Errorf("agk %s of an installation that does not answer left with %d: %s", strings.Join(args, " "), code, errs)
		}
	}
}

// -o json writes the installation's answer as it gave it, whatever it says beyond what agk reads,
// and with a namespace named, the service accounts of that namespace as the installation wrote each.
func TestAgkServiceAccountListWritesTheAnswerAsItWasGiven(t *testing.T) {
	const answer = `{"service_accounts":[` +
		`{"kind":"service_account","namespace":"finance","name":"agentiik","created_at":"2026-09-27T09:00:00Z","description":"built in"},` +
		`{"kind":"service_account","namespace":"hr","name":"sync","created_by":"erin","created_at":"2026-09-27T09:05:00Z"}` +
		`],"next":"abc"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	compact := func(out string) string {
		t.Helper()
		var b bytes.Buffer
		if err := json.Compact(&b, []byte(out)); err != nil {
			t.Fatalf("%q is not JSON: %s", out, err)
		}
		return b.String()
	}
	code, out, errs := against(t.Context(), t.TempDir(), srv.URL, "service-account", "list", "-o", "json")
	if code != exitSucceeded || compact(out) != answer {
		t.Errorf("agk service-account list -o json left with %d, writing %s: %s", code, out, errs)
	}
	code, out, errs = against(t.Context(), t.TempDir(), srv.URL, "service-account", "list", "finance", "-o", "json")
	if want := `{"service_accounts":[{"kind":"service_account","namespace":"finance","name":"agentiik","created_at":"2026-09-27T09:00:00Z","description":"built in"}]}`; code != exitSucceeded || compact(out) != want {
		t.Errorf("agk service-account list finance -o json left with %d, writing %s: %s", code, out, errs)
	}
	code, out, errs = against(t.Context(), t.TempDir(), srv.URL, "service-account", "list", "team-ops", "-o", "json")
	if code != exitSucceeded || compact(out) != `{"service_accounts":[]}` {
		t.Errorf("agk service-account list team-ops -o json left with %d, writing %s: %s", code, out, errs)
	}
}
