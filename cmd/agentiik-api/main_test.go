package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/secret"
)

// The verbs and what each refuses before it does anything.

func TestAVerbIsRequiredAndTakesWhatItTakes(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"serv"},
		{"run"},
		{"serve", "--listen=:9090"},
		{"migrate", "now"},
		{"bus-init"},
		{"bus-init", ""},
		{"bus-init", "/a", "/b"},
		{"bus-credential"},
		{"--version", "serve"},
		{"--help", "serve"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, empty, &stdout, &stderr); code != exitUsage {
			t.Errorf("%q exited %d, want %d", args, code, exitUsage)
		}
		if !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("%q says nothing of how it is used:\n%s", args, stderr.String())
		}
	}
	for _, args := range [][]string{{"--version"}, {"--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), args, empty, &stdout, &stderr); code != exitStopped || stdout.Len() == 0 {
			t.Errorf("%q exited %d and printed %q", args, code, stdout.String())
		}
	}
}

// With nothing configured, serve and migrate refuse their start and name every setting they need.
func TestServeAndMigrateNameEverySettingTheyNeed(t *testing.T) {
	for verb, variables := range map[string][]string{
		"serve": {
			config.DatabaseURL, config.BusURL, config.BusCredentialsFile, config.BusAccountSeedFile,
			config.ObjectsDir, config.PublicURL, config.PresignKeyFile, config.MasterKeyFile, config.OperatorTokenFile,
		},
		"migrate": {config.MigrateDatabaseURL, config.DatabaseURL},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{verb}, empty, &stdout, &stderr); code != exitFailed {
			t.Errorf("%s with nothing configured exited %d, want %d", verb, code, exitFailed)
		}
		for _, variable := range variables {
			if !strings.Contains(stderr.String(), variable) {
				t.Errorf("%s's refusal does not name %s:\n%s", verb, variable, stderr.String())
			}
		}
	}
}

// The master key and the env prefixes are read by internal/config and finished here, since only
// the API may parse the one and check the other. A key or a prefix it cannot use refuses the start
// beside every other refusal, naming the variable it came from.
func TestAMasterKeyOrAPrefixTheAPICannotUseRefusesTheStart(t *testing.T) {
	env := configured(t)
	s, err := readSettings(env.lookup)
	if err != nil {
		t.Fatalf("a configuration with every setting right was refused: %s", err)
	}
	if s.keys == nil || s.keys.Current().ID() != "2026-09" {
		t.Fatalf("the master key was read as %v", s.keys)
	}

	env.write(t, config.MasterKeyFile, "id: 2026-09\nkey: "+base64.StdEncoding.EncodeToString(make([]byte, 16))+"\n")
	env.set(config.EnvPrefixes, "finance=FINANCE_")
	env.set(config.Listen, "8080")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"serve"}, env.lookup, &stdout, &stderr); code != exitFailed {
		t.Fatalf("serve exited %d, want %d", code, exitFailed)
	}
	for _, variable := range []string{config.MasterKeyFile, config.EnvPrefixes, config.Listen} {
		if !strings.Contains(stderr.String(), variable) {
			t.Errorf("the refusal does not name %s:\n%s", variable, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "serving") {
		t.Errorf("refused, it served all the same:\n%s", stderr.String())
	}
}

// From fourteen days before the control plane's credential expires, the API says so once a day,
// then says it has expired, and nothing before those fourteen days.
func TestTheAPIWarnsFromFourteenDaysBeforeTheCredentialExpires(t *testing.T) {
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	expires := start.Add(20 * 24 * time.Hour)
	clock := start
	var waited []time.Duration
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	watchCredential(t.Context(), expires, log, func() time.Time { return clock }, func(_ context.Context, d time.Duration) bool {
		waited = append(waited, d)
		clock = clock.Add(d)
		return true
	})

	if len(waited) == 0 || waited[0] != 6*24*time.Hour {
		t.Fatalf("it waited %v first, and the first warning is fourteen days before the expiry, six days on", waited)
	}
	if strings.Count(logged.String(), "level=WARN") != 14 {
		t.Errorf("it warned %d times over fourteen days, once a day:\n%s", strings.Count(logged.String(), "level=WARN"), logged.String())
	}
	if first, _, _ := strings.Cut(logged.String(), "\n"); !strings.Contains(first, "left=336h0m0s") {
		t.Errorf("the first warning reads %q, fourteen days before the expiry", first)
	}
	if strings.Count(logged.String(), "level=ERROR") != 1 || !strings.Contains(logged.String(), "has expired") {
		t.Errorf("it did not say once that the credential expired:\n%s", logged.String())
	}
	if !clock.Equal(expires) {
		t.Errorf("it stopped at %s, and the credential expires at %s", clock, expires)
	}

	// A credential nearer its expiry than that is warned about at once, and one that never
	// expires is never warned about.
	for _, c := range []struct {
		expires time.Time
		warns   bool
	}{{start.Add(time.Hour), true}, {time.Time{}, false}} {
		logged.Reset()
		clock = start
		watchCredential(t.Context(), c.expires, log, func() time.Time { return clock }, func(context.Context, time.Duration) bool { return false })
		if got := strings.Contains(logged.String(), "level=WARN"); got != c.warns {
			t.Errorf("a credential expiring at %s warned %v at once:\n%s", c.expires, got, logged.String())
		}
	}
}

// bus-init writes the identity and says what to name in the settings; bus-credential renews the
// credential it wrote and leaves the rest. A directory others may write to refuses both.
func TestBusInitWritesANinetyDayCredentialThatBusCredentialRenews(t *testing.T) {
	now := time.Now()
	dir := filepath.Join(t.TempDir(), "bus")
	var stdout, stderr bytes.Buffer
	if code := busInit(dir, now, &stdout, &stderr); code != exitStopped {
		t.Fatalf("bus-init exited %d: %s", code, stderr.String())
	}
	for _, want := range []string{
		config.BusAccountSeedFile + "=" + filepath.Join(dir, bus.AccountSeedFile),
		config.BusCredentialsFile + "=" + filepath.Join(dir, bus.ControlPlaneFile),
		filepath.Join(dir, bus.AccountsFile),
		now.Add(90 * 24 * time.Hour).UTC().Format("2006-01-02T15:04"),
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("bus-init does not say %q:\n%s", want, stdout.String())
		}
	}
	expires := credentialExpiry(t, filepath.Join(dir, bus.ControlPlaneFile))
	if got := expires.Sub(now); got < 90*24*time.Hour-time.Second || got > 90*24*time.Hour {
		t.Errorf("bus-init wrote a credential valid for %s, and it is valid ninety days", got)
	}

	later := now.Add(80 * 24 * time.Hour)
	stdout.Reset()
	if code := busCredential(dir, later, &stdout, &stderr); code != exitStopped {
		t.Fatalf("bus-credential exited %d: %s", code, stderr.String())
	}
	if renewed := credentialExpiry(t, filepath.Join(dir, bus.ControlPlaneFile)); renewed.Sub(later) < 90*24*time.Hour-time.Second {
		t.Errorf("bus-credential wrote a credential expiring %s after it ran, and it is valid ninety days", renewed.Sub(later))
	}
	if !strings.Contains(stdout.String(), "restart") {
		t.Errorf("bus-credential does not say the programs read it when they start:\n%s", stdout.String())
	}

	// Run twice, bus-init is refused, since an operator created twice is two sets of keys.
	stderr.Reset()
	if code := busInit(dir, now, io.Discard, &stderr); code != exitFailed || !strings.Contains(stderr.String(), "created once") {
		t.Errorf("a second bus-init exited %d: %s", code, stderr.String())
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := busCredential(dir, later, io.Discard, &stderr); code != exitFailed || !strings.Contains(stderr.String(), "chmod 700") {
		t.Errorf("bus-credential in a directory its group may write to exited %d: %s", code, stderr.String())
	}
}

// credentialExpiry reads a credential file as the API and the controller read it.
func credentialExpiry(t *testing.T, path string) time.Time {
	t.Helper()
	var env environment
	env.values = map[string]string{config.BusURL: "tls://nats:4222", config.BusCredentialsFile: path}
	c, _ := config.ReadController(env.lookup)
	if c.Bus.Expires.IsZero() {
		t.Fatalf("%s holds no credential that expires", path)
	}
	return c.Bus.Expires
}

// environment is a set of variables a test hands the program instead of its own.
type environment struct {
	dir    string
	values map[string]string
}

func (e *environment) lookup(name string) (string, bool) {
	v, ok := e.values[name]
	return v, ok
}

func (e *environment) set(name, value string) { e.values[name] = value }

// write puts content in a file of its owner alone and names it in the variable.
func (e *environment) write(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(e.dir, strings.ToLower(name))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	e.set(name, path)
}

// empty is an environment with nothing in it.
func empty(string) (string, bool) { return "", false }

// configured is an environment with every setting of serve right, reaching nothing.
func configured(t *testing.T) *environment {
	t.Helper()
	env := &environment{dir: t.TempDir(), values: map[string]string{}}
	in, err := bus.NewInstallation(filepath.Join(env.dir, "bus"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	master, err := secret.NewMaster("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	env.set(config.DatabaseURL, "postgres://agentiik@db.example.com/agentiik?sslmode=verify-full")
	env.set(config.BusURL, "tls://nats.example.com:4222")
	env.set(config.BusCredentialsFile, in.ControlPlane)
	env.set(config.BusAccountSeedFile, in.AccountSeed)
	env.set(config.ObjectsDir, t.TempDir())
	env.set(config.PublicURL, "https://agentiik.example.com")
	env.write(t, config.PresignKeyFile, base64.StdEncoding.EncodeToString(key)+"\n")
	env.write(t, config.MasterKeyFile, string(master.Write()))
	env.write(t, config.OperatorTokenFile, theHash+"\n")
	return env
}

// audit-verify reads an export and nothing else, says how far it holds, and fails on a break.
func TestAuditVerifyChecksAnExport(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	prev := audit.Genesis
	for i := range 3 {
		e := audit.Entry{
			Seq: int64(i + 1), At: time.Date(2026, 9, 25, 10, 0, i, 0, time.UTC), Actor: "operator",
			Action: audit.RunCancel, Namespace: "finance", Target: "run", Result: audit.Done, Detail: "{}", PrevHash: prev,
		}
		e.Hash = e.Sum()
		prev = e.Hash
		line, err := e.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	whole := filepath.Join(dir, "whole.ndjson")
	edited := filepath.Join(dir, "edited.ndjson")
	os.WriteFile(whole, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	os.WriteFile(edited, []byte(strings.Replace(strings.Join(lines, "\n"), `"operator"`, `"somebody"`, 1)), 0o600)

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"audit-verify", whole}, empty, &stdout, &stderr); code != exitStopped || !strings.Contains(stdout.String(), "entries 1 to 3 hold") {
		t.Fatalf("a whole export exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run(t.Context(), []string{"audit-verify", edited}, empty, &stdout, &stderr); code != exitFailed || !strings.Contains(stderr.String(), "breaks at entry 1") {
		t.Fatalf("an edited export exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{{"audit-verify"}, {"audit-verify", ""}, {"audit-verify", whole, edited}} {
		if code := run(t.Context(), args, empty, &stdout, &stderr); code != exitUsage {
			t.Errorf("%q exited %d", args, code)
		}
	}
}
