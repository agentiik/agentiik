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
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// agk token against the real API over PostgreSQL, authorised by the installation's own Principals:
// alice owns finance, whose service account is finance/nightly, and bob holds nothing.

// tokenInstallation is the address of such an installation, and alice's and bob's tokens.
type tokenInstallation struct {
	url, alice, bob string
}

func anInstallationWithTokens(t *testing.T) tokenInstallation {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	in := tokenInstallation{alice: "agktoken_alice" + strings.Repeat("C", 40), bob: "agktoken_bob" + strings.Repeat("D", 40)}
	now := time.Now().UTC()
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{{Login: "alice", DisplayName: "Alice"}, {Login: "bob", DisplayName: "Bob"}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		if err := w.CreateServiceAccount(ctx, db.ServiceAccount{Namespace: "finance", Name: "nightly", CreatedBy: "alice"}); err != nil {
			return err
		}
		for login, value := range map[string]string{"alice": in.alice, "bob": in.bob} {
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
	if _, err := api.NewTokens(rt, api.TokenOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	in.url = srv.URL
	return in
}

// agkWithToken runs one command line against the installation at url with token in AGENTIIK_TOKEN.
func agkWithToken(t *testing.T, url, token string, args ...string) (int, string, string) {
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

var (
	// aMintedToken is a token alone on its line, as agk token create prints it.
	aMintedToken = regexp.MustCompile(`^agktoken_[A-Za-z0-9_-]{43}\n$`)
	// mintedSaying is what agk token create says of it on standard error.
	mintedSaying = regexp.MustCompile(`^minted token ([0-9A-HJKMNP-TV-Z]{26}) of (\S+), expiring at (\S+): it is shown this once, and kept as its hash alone\n$`)
)

// A token is minted and printed alone on standard output, so that a script keeps it; the token
// opens the installation at once and is listed with its label and its use; revoked by the
// identifier the list prints, it opens nothing at its next request.
func TestATokenIsMintedListedAndRevokedWithAgk(t *testing.T) {
	in := anInstallationWithTokens(t)
	code, out, errs := agkWithToken(t, in.url, in.alice, "token", "create", "--label", "alice-laptop")
	if code != exitSucceeded || !aMintedToken.MatchString(out) {
		t.Fatalf("agk token create left with %d, printing %q: %s", code, out, errs)
	}
	said := mintedSaying.FindStringSubmatch(errs)
	if said == nil || said[2] != "alice" {
		t.Fatalf("agk token create said %q", errs)
	}
	minted, id := strings.TrimSpace(out), said[1]

	code, out, errs = agkWithToken(t, in.url, minted, "token", "list")
	if code != exitSucceeded {
		t.Fatalf("agk token list left with %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	var mine string
	for _, line := range lines {
		if strings.HasPrefix(line, id+" ") {
			mine = line
		}
	}
	if len(lines) != 2 || !regexp.MustCompile(`^`+id+`  alice  expires \S+  last used \S+  alice-laptop$`).MatchString(mine) {
		t.Errorf("agk token list printed:\n%s", out)
	}
	if strings.Contains(out, strings.TrimPrefix(minted, "agktoken_")) {
		t.Error("agk token list printed a token's value")
	}

	code, out, errs = agkWithToken(t, in.url, in.alice, "token", "revoke", id)
	if code != exitSucceeded || out != "revoked token "+id+": it opens nothing from its next request\n" {
		t.Fatalf("agk token revoke left with %d, printing %q: %s", code, out, errs)
	}
	code, _, errs = agkWithToken(t, in.url, minted, "token", "list")
	if code != exitRefused || !strings.Contains(errs, "did not accept the credential in AGENTIIK_TOKEN") {
		t.Errorf("the revoked token was answered %d: %s", code, errs)
	}
}

// A token for a service account of a namespace one owns, for a length of days and narrowed by a
// scope written as permissions and places together, as the installation answers it with -o json.
func TestATokenForAServiceAccountIsMintedWithAgk(t *testing.T) {
	in := anInstallationWithTokens(t)
	before := time.Now().UTC()
	code, out, errs := agkWithToken(t, in.url, in.alice, "token", "create", "--for", "finance/nightly", "--expires", "30d",
		"--scope", "workflow:run,finance/monthly-invoicing", "--scope", "run:read", "--label", "terraform", "-o", "json")
	if code != exitSucceeded {
		t.Fatalf("agk token create --for finance/nightly left with %d: %s", code, errs)
	}
	var issued api.IssuedToken
	if err := json.Unmarshal([]byte(out), &issued); err != nil {
		t.Fatalf("-o json printed %q: %s", out, err)
	}
	got := issued.APIToken
	if got.Principal != "finance/nightly" || got.DeviceLabel != "terraform" || got.Scope == nil ||
		strings.Join(got.Scope.Permissions, " ") != "workflow:run run:read" || strings.Join(got.Scope.Within, " ") != "finance/monthly-invoicing" {
		t.Errorf("the token minted reads %+v", got)
	}
	if want := before.Add(30 * 24 * time.Hour); got.ExpiresAt.Before(want.Add(-2*time.Second)) || got.ExpiresAt.After(want.Add(time.Minute)) {
		t.Errorf("a token asked to last 30d expires at %s", got.ExpiresAt)
	}

	code, out, _ = agkWithToken(t, in.url, in.alice, "token", "list", "-o", "json")
	var listed api.TokenList
	if code != exitSucceeded || json.Unmarshal([]byte(out), &listed) != nil || len(listed.Tokens) != 2 || listed.Tokens[0].ID != got.ID {
		t.Errorf("agk token list -o json left with %d, printing %s", code, out)
	}
	code, out, _ = agkWithToken(t, in.url, issued.Token, "token", "list")
	if code != exitSucceeded || !strings.Contains(out, "keeps workflow:run,run:read within finance/monthly-invoicing  terraform") {
		t.Errorf("the service account's narrowed token lists itself as %q", out)
	}
}

// What agk refuses before it asks, what the installation refuses, and an installation that does not
// answer, each with its own exit code and the sentence that says why.
func TestWhatAgkTokenRefuses(t *testing.T) {
	in := anInstallationWithTokens(t)
	for _, c := range []struct {
		name  string
		token string
		args  []string
		code  int
		says  string
	}{
		{"an expiry of no time", in.alice, []string{"token", "create", "--expires", "0d"}, exitUsage, "no time at all"},
		{"an expiry of fewer days than none", in.alice, []string{"token", "create", "--expires", "-213503d"}, exitUsage, "no time at all"},
		{"an expiry of more days than a length holds", in.alice, []string{"token", "create", "--expires", "213600d"}, exitUsage, "more days than a length holds"},
		{"an expiry that does not read", in.alice, []string{"token", "create", "--expires", "soon"}, exitUsage, "neither a length"},
		{"an empty scope entry", in.alice, []string{"token", "create", "--scope", "workflow:run,"}, exitUsage, "an entry is empty"},
		{"a format there is not", in.alice, []string{"token", "list", "-o", "yaml"}, exitUsage, "json is the one format"},
		{"a word after create", in.alice, []string{"token", "create", "finance/nightly"}, exitUsage, "whose token it is is --for"},
		{"no token to revoke", in.alice, []string{"token", "revoke"}, exitUsage, "names one token"},
		{"no credential", "", []string{"token", "list"}, exitUsage, "no credential"},
		{"a principal bob does not own", in.bob, []string{"token", "create", "--for", "finance/nightly"}, exitRefused, "neither you nor a service account of a namespace you own"},
		{"a token of alice's revoked by bob", in.bob, []string{"token", "revoke", "01M2AD1R3T5W7Y9A1C3E5G7J9M"}, exitRefused, "no token 01M2AD1R3T5W7Y9A1C3E5G7J9M, or not yours to revoke"},
		{"an expiry past a year", in.alice, []string{"token", "create", "--expires", "400d"}, exitRefused, "more than a year away"},
		{"a credential the installation does not know", "agktoken_" + strings.Repeat("Z", 43), []string{"token", "list"}, exitRefused, "did not accept the credential"},
	} {
		code, out, errs := agkWithToken(t, in.url, c.token, c.args...)
		if code != c.code || !strings.Contains(errs, c.says) || out != "" {
			t.Errorf("%s: agk %s left with %d, printing %q and saying %q; want %d saying %q", c.name, strings.Join(c.args, " "), code, out, errs, c.code, c.says)
		}
	}

	// A narrowed token mints nothing, which the installation says.
	code, out, _ := agkWithToken(t, in.url, in.alice, "token", "create", "--scope", "finance")
	narrowed := strings.TrimSpace(out)
	if code != exitSucceeded {
		t.Fatalf("minting a narrowed token left with %d", code)
	}
	if code, _, errs := agkWithToken(t, in.url, narrowed, "token", "create"); code != exitRefused || !strings.Contains(errs, "narrowed by a scope mints no token") {
		t.Errorf("a narrowed token minting another left with %d: %s", code, errs)
	}

	// An installation that answers nothing is no outcome.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := "http://" + l.Addr().String()
	l.Close()
	if code, _, errs := agkWithToken(t, gone, in.alice, "token", "list"); code != exitNoOutcome || !strings.Contains(errs, "could not be reached") {
		t.Errorf("an installation that answers nothing left agk with %d: %s", code, errs)
	}
}

// A token minted or revoked answered with a 5xx is no outcome, since the installation may have
// minted or revoked it before the answer failed, and the sentence says how to tell; the list
// answered so changed nothing and is a refusal, and so is a change the installation refused.
func TestAgkTokenTellsNoOutcomeFromARefusal(t *testing.T) {
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
		{http.StatusInternalServerError, []string{"token", "create"}, exitNoOutcome, "whether a token was minted cannot be told from it: agk token list shows the tokens minted"},
		{http.StatusBadGateway, []string{"token", "create", "--for", "finance/nightly"}, exitNoOutcome, "cannot be told"},
		{http.StatusServiceUnavailable, []string{"token", "revoke", "01M2AD1R3T5W7Y9A1C3E5G7J9M"}, exitNoOutcome, "whether token 01M2AD1R3T5W7Y9A1C3E5G7J9M was revoked cannot be told from it: agk token list reads it back"},
		{http.StatusInternalServerError, []string{"token", "list"}, exitRefused, "said by the installation"},
		{http.StatusConflict, []string{"token", "create"}, exitRefused, "said by the installation"},
		{http.StatusForbidden, []string{"token", "revoke", "01M2AD1R3T5W7Y9A1C3E5G7J9M"}, exitRefused, "said by the installation"},
	} {
		status.Store(int64(c.status))
		code, out, errs := against(t.Context(), t.TempDir(), srv.URL, c.args...)
		if code != c.code || out != "" || !strings.Contains(errs, c.says) {
			t.Errorf("agk %s answered %d left with %d, printing %q; want %d saying %q: %s", strings.Join(c.args, " "), c.status, code, out, c.code, c.says, errs)
		}
	}
}
