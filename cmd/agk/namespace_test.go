package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/dbtest"
	"github.com/agentiik/agentiik/internal/ulid"
)

// agk namespace against the real namespace routes over PostgreSQL, behind the real Principals:
// carol administers the installation and alice does not, and each has an API token.

type namespaceInstallation struct {
	url          string
	carol, alice string
}

func aNamespaceInstallation(t *testing.T) namespaceInstallation {
	t.Helper()
	pool, super := dbtest.Open(t)
	if _, err := dbtest.Superuser(t, super).Exec(t.Context(), `insert into namespaces (name) values ('finance')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	token := func(w *db.Wide, ctx context.Context, login string) (string, error) {
		value := "agk_test_" + login + "_" + ulid.New()
		hash := sha256.Sum256([]byte(value))
		return value, w.MintToken(ctx, db.APIToken{
			ID: ulid.New(), Hash: hash[:], Principal: login, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		})
	}
	var in namespaceInstallation
	err := pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		for _, u := range []db.User{{Login: "carol", Profile: db.Profile{GivenName: "Carol"}, Admin: true}, {Login: "alice", Profile: db.Profile{GivenName: "Alice"}}} {
			if err := w.CreateUser(ctx, u); err != nil {
				return err
			}
		}
		var err error
		if in.carol, err = token(w, ctx, "carol"); err != nil {
			return err
		}
		in.alice, err = token(w, ctx, "alice")
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
	if _, err := api.NewNamespaces(rt, api.NamespaceOptions{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	in.url = srv.URL
	return in
}

// as runs one command line as the bearer of token.
func (in namespaceInstallation) as(t *testing.T, token string, args ...string) (int, string, string) {
	t.Helper()
	out, errs := &strings.Builder{}, &strings.Builder{}
	code := run(t.Context(), Env{
		Out: out, Err: errs, Dir: t.TempDir(),
		Getenv: func(k string) string {
			switch k {
			case tokenVariable:
				return token
			case serverVariable:
				return in.url
			}
			return ""
		},
	}, args)
	return code, out.String(), errs.String()
}

// An administrator creates a namespace with its owner and quotas, bounds it and removes it; its
// owner lists and reads it and nothing else, and is told it may not change it; and what each verb
// prints is what the installation answered, each quota under its own name.
func TestAgkAdministersANamespace(t *testing.T) {
	in := aNamespaceInstallation(t)

	code, out, errs := in.as(t, in.carol, "namespace", "create", "team-ops", "--owner", "alice", "--max-runs-per-hour", "500", "--allowed-runner-pools", "default")
	want := "created namespace team-ops: shared, owned by alice\n" +
		"  max_concurrent_tasks  20\n" +
		"  max_runs_per_hour     500\n" +
		"  max_retention_days    90\n" +
		"  allowed_runner_pools  default\n"
	if code != exitSucceeded || out != want {
		t.Fatalf("agk namespace create answered %d:\n%s%s\nwant\n%s", code, out, errs, want)
	}

	// Reading the quotas sets nothing, whatever other flag is given.
	var read api.Quotas
	if code, out, errs := in.as(t, in.alice, "namespace", "quotas", "team-ops", "-o", "json"); code != exitSucceeded || json.Unmarshal([]byte(out), &read) != nil || read.MaxRunsPerHour != 500 {
		t.Errorf("the owner reading the quotas in JSON answered %d:\n%s%s", code, out, errs)
	}

	if code, out, errs := in.as(t, in.carol, "namespace", "list"); code != exitSucceeded || out != "finance   shared  owned by nobody\nteam-ops  shared  owned by alice\n" {
		t.Errorf("an administrator's list answered %d:\n%s%s", code, out, errs)
	}
	if code, out, errs := in.as(t, in.alice, "namespace", "list"); code != exitSucceeded || out != "team-ops  shared  owned by alice\n" {
		t.Errorf("the owner's list answered %d:\n%s%s", code, out, errs)
	}
	if code, out, _ := in.as(t, in.alice, "namespace", "show", "team-ops"); code != exitSucceeded || !strings.HasPrefix(out, "team-ops: shared, owned by alice\n") {
		t.Errorf("the owner reading the namespace answered %d:\n%s", code, out)
	}
	if code, _, errs := in.as(t, in.alice, "namespace", "show", "finance"); code != exitRefused || !strings.Contains(errs, "no namespace finance, or not yours") {
		t.Errorf("reading a namespace alice holds nothing in answered %d: %s", code, errs)
	}
	if code, _, errs := in.as(t, in.alice, "namespace", "quotas", "team-ops", "--max-runs-per-hour", "5"); code != exitRefused || !strings.Contains(errs, "an administrator's") {
		t.Errorf("the owner bounding the namespace answered %d: %s", code, errs)
	}

	// One quota given is that one set, and every bound nobody named is kept, allowed_runner_pools
	// among them; a bound goes only where --lift names it.
	code, out, errs = in.as(t, in.carol, "namespace", "quotas", "team-ops", "--max-concurrent-tasks", "50")
	want = "  max_concurrent_tasks  50\n  max_runs_per_hour     500\n  max_retention_days    90\n  allowed_runner_pools  default\n"
	if code != exitSucceeded || out != want {
		t.Errorf("setting one quota answered %d:\n%s%s\nwant\n%s", code, out, errs, want)
	}
	code, out, errs = in.as(t, in.carol, "namespace", "quotas", "team-ops", "--lift", "allowed_runner_pools", "--lift", "max_runs_per_hour", "--max-run-duration", "4h")
	want = "  max_concurrent_tasks  50\n  max_retention_days    90\n  max_run_duration      4h\n"
	if code != exitSucceeded || out != want {
		t.Errorf("lifting two quotas answered %d:\n%s%s\nwant\n%s", code, out, errs, want)
	}
	if code, again, _ := in.as(t, in.alice, "namespace", "quotas", "team-ops"); code != exitSucceeded || again != out {
		t.Errorf("the quotas read back as %q, and were set to %q", again, out)
	}
	code, out, _ = in.as(t, in.carol, "namespace", "show", "team-ops", "-o", "json")
	var record api.NamespaceRecord
	if err := json.Unmarshal([]byte(out), &record); code != exitSucceeded || err != nil || record.Quotas == nil || record.Quotas.MaxConcurrentTasks != 50 {
		t.Errorf("-o json answered %d: %s", code, out)
	}

	if code, out, errs := in.as(t, in.carol, "namespace", "delete", "team-ops"); code != exitSucceeded || out != "removed namespace team-ops\n" {
		t.Errorf("removing the namespace answered %d:\n%s%s", code, out, errs)
	}
	if code, _, errs := in.as(t, in.carol, "namespace", "delete", "team-ops"); code != exitRefused || !strings.Contains(errs, "no namespace team-ops") {
		t.Errorf("removing it again answered %d: %s", code, errs)
	}
	if code, _, errs := in.as(t, in.carol, "namespace", "create", "finance", "--owner", "alice"); code != exitRefused || !strings.Contains(errs, "finance is already a namespace") {
		t.Errorf("creating a namespace that exists answered %d: %s", code, errs)
	}
}

// The command line is refused before anything is sent where it is wrong: no namespace, no owner, a
// quota written as zero, which the wire refuses and which would otherwise be dropped as a flag
// nobody passed, and an output format there is not.
func TestAgkNamespaceRefusesACommandLineThatIsWrong(t *testing.T) {
	var asked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Store(true) }))
	t.Cleanup(srv.Close)
	for _, args := range [][]string{
		{"namespace", "create", "team-ops"},
		{"namespace", "create", "--owner", "alice"},
		{"namespace", "create", "team-ops", "--owner", "alice", "--max-runs-per-hour", "0"},
		{"namespace", "quotas", "team-ops", "--allowed-runner-pools", ","},
		{"namespace", "quotas", "team-ops", "--max-run-duration", ""},
		{"namespace", "show", "team-ops", "hr"},
		{"namespace", "show", "team-ops", "-o", "yaml"},
		{"namespace", "list", "team-ops"},
		{"namespace", "quotas", "team-ops", "--lift", "max_concurrent_tasks"},
		{"namespace", "quotas", "team-ops", "--lift", "max_retention_days"},
		{"namespace", "quotas", "team-ops", "--lift", "max-runs-per-hour"},
		{"namespace", "quotas", "team-ops", "--lift", "max_runs_per_hour", "--max-runs-per-hour", "5"},
		{"namespace", "create", "team-ops", "--owner", "alice", "--lift", "max_runs_per_hour"},
	} {
		if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, args...); code != exitUsage {
			t.Errorf("agk %s answered %d: %s", strings.Join(args, " "), code, errs)
		}
	}
	if asked.Load() {
		t.Error("a command line that is wrong was sent to the installation")
	}

	// A verb of the family nobody knows is named as typed.
	if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, "namespace", "rename", "finance"); code != exitUsage || !strings.Contains(errs, "namespace rename: there is no such command") {
		t.Errorf("an unknown namespace verb answered %d: %s", code, errs)
	}
}

// An installation that does not answer is no outcome, not a refusal.
func TestAgkNamespaceOfAnInstallationThatDoesNotAnswerIsNoOutcome(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if code, _, errs := against(t.Context(), t.TempDir(), url, "namespace", "list"); code != exitNoOutcome {
		t.Errorf("an installation that does not answer answered %d: %s", code, errs)
	}
}

// A change answered with a failure that may pass is no outcome, since a gateway answering after the
// API committed says nothing of whether the change was made, and the sentence says how to read it
// back; a refusal is refused. A listing answered 404 says what the installation said, since it
// names no namespace to be absent.
func TestAgkNamespaceTellsNoOutcomeFromARefusal(t *testing.T) {
	status := atomic.Int64{}
	// A read is answered, so that the quotas' change, which reads them first, is what meets the
	// status; the listing below is a read, and is answered the status too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/quotas") {
			w.Write([]byte(`{"max_concurrent_tasks":20,"max_retention_days":90}`))
			return
		}
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(`{"error":"said by the installation"}`))
	}))
	t.Cleanup(srv.Close)
	for _, args := range [][]string{
		{"namespace", "create", "team-ops", "--owner", "alice"},
		{"namespace", "delete", "team-ops"},
		{"namespace", "quotas", "team-ops", "--max-runs-per-hour", "5"},
	} {
		for _, c := range []struct {
			status int
			want   int
			says   string
		}{
			{http.StatusGatewayTimeout, exitNoOutcome, "agk namespace show team-ops reads it back"},
			{http.StatusServiceUnavailable, exitNoOutcome, "cannot be told"},
			{http.StatusConflict, exitRefused, "said by the installation"},
		} {
			status.Store(int64(c.status))
			if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, args...); code != c.want || !strings.Contains(errs, c.says) {
				t.Errorf("agk %s answered %d by the installation left with %d: %s", strings.Join(args, " "), c.status, code, errs)
			}
		}
	}
	status.Store(http.StatusNotFound)
	if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, "namespace", "list"); code != exitRefused || strings.TrimSpace(errs) != "said by the installation" {
		t.Errorf("a listing answered 404 left with %d: %s", code, errs)
	}
}

// agk namespace quotas reads the quotas first and sends the whole set back with the flags given on
// top and the quotas --lift names taken away, so that the route, which lifts every optional bound
// its body leaves out, is never sent a set missing one nobody named.
func TestAgkNamespaceQuotasSendsTheQuotasHeldWithTheFlagsOnTop(t *testing.T) {
	held := `{"max_concurrent_tasks":20,"max_runs_per_hour":500,"max_artifact_bytes":1024,"max_retention_days":90,"max_run_duration":"24h","allowed_runner_pools":["default","dmz"]}`
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/team-ops/quotas" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			sent = append(sent, string(body))
			mu.Unlock()
			w.Write(body)
			return
		}
		w.Write([]byte(held))
	}))
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--max-runs-per-hour", "60"},
			`{"max_concurrent_tasks":20,"max_runs_per_hour":60,"max_artifact_bytes":1024,"max_retention_days":90,"max_run_duration":"24h","allowed_runner_pools":["default","dmz"]}`},
		{[]string{"--allowed-runner-pools", "gpu", "--lift", "max_artifact_bytes", "--lift", "max_run_duration"},
			`{"max_concurrent_tasks":20,"max_runs_per_hour":500,"max_retention_days":90,"allowed_runner_pools":["gpu"]}`},
		{[]string{"--lift", "allowed_runner_pools"},
			`{"max_concurrent_tasks":20,"max_runs_per_hour":500,"max_artifact_bytes":1024,"max_retention_days":90,"max_run_duration":"24h"}`},
	} {
		mu.Lock()
		sent = nil
		mu.Unlock()
		args := append([]string{"namespace", "quotas", "team-ops"}, c.args...)
		if code, _, errs := against(t.Context(), t.TempDir(), srv.URL, args...); code != exitSucceeded {
			t.Fatalf("agk %s answered %d: %s", strings.Join(args, " "), code, errs)
		}
		mu.Lock()
		if len(sent) != 1 || sent[0] != c.want {
			t.Errorf("agk %s sent %q, want the set held with the flags on top, %s", strings.Join(args, " "), sent, c.want)
		}
		mu.Unlock()
	}

	// With nothing given or lifted, nothing is sent.
	mu.Lock()
	sent = nil
	mu.Unlock()
	if code, out, _ := against(t.Context(), t.TempDir(), srv.URL, "namespace", "quotas", "team-ops"); code != exitSucceeded || !strings.Contains(out, "allowed_runner_pools  default, dmz") || len(sent) != 0 {
		t.Errorf("reading the quotas answered %d, sent %q:\n%s", code, sent, out)
	}
}
