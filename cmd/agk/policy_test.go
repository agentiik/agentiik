package main

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// agk auth policy against the real policy routes over PostgreSQL, behind the real Principals: carol
// administers the installation, alice holds a grant in finance, and each has an API token.
func aPolicyInstallation(t *testing.T) namespaceInstallation {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var in namespaceInstallation
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{{Login: "carol", DisplayName: "Carol", Admin: true}, {Login: "alice", DisplayName: "Alice"}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		for login, into := range map[string]*string{"carol": &in.carol, "alice": &in.alice} {
			value := "agk_test_" + login + "_" + ulid.New()
			hash := sha256.Sum256([]byte(value))
			if err := w.MintToken(ctx, db.APIToken{
				ID: ulid.New(), Hash: hash[:], Principal: login, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
			}); err != nil {
				return err
			}
			*into = value
		}
		return w.GrantAccess(ctx, access.Grant{ID: ulid.New(), Principal: "alice", Scope: access.Scope{Namespace: "finance"}, Role: access.Viewer, GrantedBy: "carol"})
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
	if _, err := api.NewPolicies(rt, api.PolicyOptions{Pool: pool, PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	in.url = srv.URL
	return in
}

// An administrator reads the installation's policy, every setting under its own name, and sets the
// settings given on top of those held; a namespace's tightening is read by whoever holds a grant in
// it and set and dropped by an administrator; whoever else sets it is told it is an administrator's,
// and a namespace loosening a setting is told which.
func TestAgkSetsTheAuthenticationPolicy(t *testing.T) {
	in := aPolicyInstallation(t)
	defaults := "  password           allowed\n  passkey            required\n  user_verification  required\n  device_bound_only  false\n  min_passkeys       2\n"
	if code, out, errs := in.as(t, in.alice, "auth", "policy"); code != exitSucceeded || out != defaults {
		t.Errorf("alice reading the policy answered %d:\n%s%s", code, out, errs)
	}
	want := "  password           allowed\n  passkey            optional\n  user_verification  required\n  device_bound_only  false\n  min_passkeys       3\n"
	if code, out, errs := in.as(t, in.carol, "auth", "policy", "--passkey", "optional", "--min-passkeys", "3"); code != exitSucceeded || out != want {
		t.Errorf("carol setting the policy answered %d:\n%s%s", code, out, errs)
	}
	if code, out, errs := in.as(t, in.carol, "auth", "policy", "--device-bound-only"); code != exitSucceeded || !strings.Contains(out, "passkey            optional") || !strings.Contains(out, "device_bound_only  true") {
		t.Errorf("carol setting one more answered %d, keeping what she set before:\n%s%s", code, out, errs)
	}
	if code, _, errs := in.as(t, in.alice, "auth", "policy", "--passkey", "required"); code != exitRefused || !strings.Contains(errs, "an administrator's") {
		t.Errorf("alice setting the policy answered %d: %s", code, errs)
	}

	if code, out, errs := in.as(t, in.carol, "auth", "policy", "--namespace", "finance", "--min-passkeys", "4"); code != exitSucceeded || out != "  min_passkeys       4\n" {
		t.Errorf("carol tightening finance answered %d:\n%s%s", code, out, errs)
	}
	if code, out, errs := in.as(t, in.alice, "auth", "policy", "--namespace", "finance", "-o", "json"); code != exitSucceeded || !strings.Contains(out, `"min_passkeys": 4`) {
		t.Errorf("alice reading finance's policy in JSON answered %d:\n%s%s", code, out, errs)
	}
	if code, _, errs := in.as(t, in.carol, "auth", "policy", "--namespace", "finance", "--device-bound-only=false"); code != exitRefused || !strings.Contains(errs, "device_bound_only is true there") {
		t.Errorf("finance loosening device_bound_only answered %d: %s", code, errs)
	}
	if code, out, errs := in.as(t, in.carol, "auth", "policy", "--namespace", "finance", "--inherit", "min_passkeys"); code != exitSucceeded || !strings.Contains(out, "tightens nothing") {
		t.Errorf("carol dropping finance's one setting answered %d:\n%s%s", code, out, errs)
	}
	if code, _, errs := in.as(t, in.carol, "auth", "policy", "--namespace", "nowhere"); code != exitRefused || !strings.Contains(errs, "no namespace nowhere") {
		t.Errorf("a namespace nobody made answered %d: %s", code, errs)
	}
}

// The command line is refused before anything is sent where it is wrong.
func TestAgkAuthPolicyRefusesACommandLineThatIsWrong(t *testing.T) {
	var asked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Store(true) }))
	t.Cleanup(srv.Close)
	for _, args := range [][]string{
		{"auth", "policy", "--password", "maybe"},
		{"auth", "policy", "--passkey", "sometimes"},
		{"auth", "policy", "--user-verification", "never"},
		{"auth", "policy", "--device-bound-only=perhaps"},
		{"auth", "policy", "--min-passkeys", "0"},
		{"auth", "policy", "--inherit", "colour", "--namespace", "finance"},
		{"auth", "policy", "--inherit", "min_passkeys"},
		{"auth", "policy", "--namespace", "finance", "--inherit", "min_passkeys", "--min-passkeys", "3"},
		{"auth", "policy", "finance"},
		{"auth", "policy", "-o", "yaml"},
	} {
		if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, args...); code != exitUsage {
			t.Errorf("agk %s answered %d: %s", strings.Join(args, " "), code, errs)
		}
	}
	if asked.Load() {
		t.Error("a command line that is wrong was sent to the installation")
	}
}

// agk auth policy reads the policy first and sends the whole of it back with the settings given on
// top and those --inherit names dropped, so that the route, which resets what its body leaves out,
// is never sent a policy missing what nobody named; with nothing given, it sends nothing. A change
// answered with a failure that may pass is no outcome, and says how to read it back.
func TestAgkAuthPolicySendsThePolicyHeldWithTheSettingsOnTop(t *testing.T) {
	held := map[string]string{
		"/api/v1/auth/policy":         `{"password":"allowed","passkey":"required","user_verification":"required","device_bound_only":false,"min_passkeys":2}`,
		"/api/v1/finance/auth/policy": `{"min_passkeys":3}`,
	}
	var mu sync.Mutex
	var sent []string
	status := atomic.Int64{}
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			sent = append(sent, r.URL.Path+" "+string(body))
			mu.Unlock()
			w.WriteHeader(int(status.Load()))
			w.Write(body)
			return
		}
		w.Write([]byte(held[r.URL.Path]))
	}))
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--passkey", "optional"},
			`/api/v1/auth/policy {"password":"allowed","passkey":"optional","user_verification":"required","device_bound_only":false,"min_passkeys":2}`},
		{[]string{"--password", "forbidden", "--device-bound-only"},
			`/api/v1/auth/policy {"password":"forbidden","passkey":"required","user_verification":"required","device_bound_only":true,"min_passkeys":2}`},
		{[]string{"--namespace", "finance", "--device-bound-only"}, `/api/v1/finance/auth/policy {"device_bound_only":true,"min_passkeys":3}`},
		{[]string{"--namespace", "finance", "--inherit", "min_passkeys"}, `/api/v1/finance/auth/policy {}`},
	} {
		mu.Lock()
		sent = nil
		mu.Unlock()
		args := append([]string{"auth", "policy"}, c.args...)
		if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, args...); code != exitSucceeded {
			t.Fatalf("agk %s answered %d: %s", strings.Join(args, " "), code, errs)
		}
		mu.Lock()
		if len(sent) != 1 || sent[0] != c.want {
			t.Errorf("agk %s sent %q, want %s", strings.Join(args, " "), sent, c.want)
		}
		mu.Unlock()
	}
	mu.Lock()
	sent = nil
	mu.Unlock()
	if code, _, _ := against(t.Context(), t.TempDir(), srv.URL, "auth", "policy", "--namespace", "finance"); code != exitSucceeded || len(sent) != 0 {
		t.Errorf("reading finance's policy answered %d and sent %q", code, sent)
	}
	status.Store(http.StatusBadGateway)
	if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, "auth", "policy", "--min-passkeys", "3"); code != exitNoOutcome || !strings.Contains(errs, "agk auth policy reads it back") {
		t.Errorf("a change answered 502 left with %d: %s", code, errs)
	}
}
