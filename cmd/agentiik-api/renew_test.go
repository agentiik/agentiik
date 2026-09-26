package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/config"
	natsjwt "github.com/nats-io/jwt/v2"
)

// The API renews the control plane's bus credential itself, so that an installation nobody touches
// for ninety days still dispatches.

// sharedCredential writes a control plane credential expiring at until, under the account issuer
// signs for, at path, with mode, as init writes it in the bus directory the API and the controller
// share.
func sharedCredential(t *testing.T, issuer *bus.Issuer, path string, until time.Time, mode os.FileMode) bus.Credentials {
	t.Helper()
	c, err := issuer.ForControlPlane(controlPlaneName, until)
	if err != nil {
		t.Fatal(err)
	}
	content, err := natsjwt.FormatUserConfig(c.JWT, []byte(c.Seed))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	return c
}

// sharedDirectory is a directory holding nothing but the control plane's credential, as the bus
// directory init makes, and the credential's path in it.
func sharedDirectory(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), busDir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, bus.ControlPlaneFile)
}

// A serving API whose credential expires within fourteen days renews it in its file at once, in one
// step, keeping its mode, and leaves nothing else beside it; the controller, reading the same file,
// connects to the bus with the renewed one.
func TestTheAPIRenewsItsCredentialAndTheControllerConnectsWithIt(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", identity}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	natsURL := natsFrom(t, identity)
	s := servingSettings(t, database.Application, identity, natsURL)
	issuer, err := bus.NewIssuer(string(s.AccountSeed), natsURL)
	if err != nil {
		t.Fatal(err)
	}
	dir, path := sharedDirectory(t)
	old := sharedCredential(t, issuer, path, time.Now().Add(10*24*time.Hour), 0o600)
	s.Bus.JWT, s.Bus.Seed, s.Bus.Expires, s.Bus.CredentialsFile = old.JWT, config.Secret(old.Seed), old.ExpiresAt, path

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var out output
	served := make(chan error, 1)
	ctx, stop := context.WithCancel(t.Context())
	go func() { served <- serve(ctx, s, ln, logger(&out)) }()
	t.Cleanup(func() { stop(); <-served })

	var renewed config.Bus
	eventually(t, 30*time.Second, "the credential renewed in its file", func() bool {
		r, err := config.RereadBus(config.Bus{URL: natsURL, CredentialsFile: path})
		renewed = r
		return err == nil && r.JWT != old.JWT
	}, &out)
	if left := time.Until(renewed.Expires); left < controlPlaneLife-time.Minute || left > controlPlaneLife {
		t.Errorf("the renewed credential expires in %s, and it is valid ninety days", left)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the renewed credential is not mode 0600, as the one it replaced: %v %v", info.Mode(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != bus.ControlPlaneFile {
		t.Errorf("the bus directory holds %v, and the renewal leaves the credential alone there", entries)
	}
	eventually(t, 10*time.Second, "the renewal said", func() bool {
		return strings.Contains(out.String(), "renewed the control plane's bus credential")
	}, &out)
	if strings.Contains(out.String(), "expires soon") {
		t.Errorf("the API warned of a credential it renewed:\n%s", out.String())
	}

	// The controller reads the same file, and the bus takes what it holds.
	b, err := bus.Open(t.Context(), bus.Options{
		URL: natsURL, Name: "agentiik-controller",
		Credentials: &bus.Credentials{JWT: renewed.JWT, Seed: string(renewed.Seed)},
	})
	if err != nil {
		t.Fatalf("the bus refused the renewed credential: %s", err)
	}
	b.Close()
}

// Outside the fourteen days the API writes nothing, byte for byte; inside them it renews, keeping a
// mode that is not the one it would choose, under the same account.
func TestTheAPIRenewsTheCredentialOnlyInsideTheFourteenDays(t *testing.T) {
	identity := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", identity}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	seed, err := os.ReadFile(filepath.Join(identity, bus.AccountSeedFile))
	if err != nil {
		t.Fatal(err)
	}
	account, err := natsjwt.ParseDecoratedNKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := account.Seed()
	issuer, err := bus.NewIssuer(string(raw), "tls://nats.example.com:4222")
	if err != nil {
		t.Fatal(err)
	}
	_, path := sharedDirectory(t)
	b := config.Bus{URL: "tls://nats.example.com:4222", CredentialsFile: path}
	once := func(context.Context, time.Duration) bool { return false }

	sharedCredential(t, issuer, path, time.Now().Add(credentialWarning+time.Hour), 0o400)
	before, _ := os.ReadFile(path)
	var logged bytes.Buffer
	watchCredential(t.Context(), expiry(b), renewer(issuer, path, time.Now), slog.New(slog.NewTextHandler(&logged, nil)), time.Now, once)
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) || logged.Len() != 0 {
		t.Errorf("a credential expiring in %s was written over, or said something:\n%s", credentialWarning+time.Hour, logged.String())
	}

	sharedCredential(t, issuer, path, time.Now().Add(credentialWarning-time.Hour), 0o400)
	watchCredential(t.Context(), expiry(b), renewer(issuer, path, time.Now), slog.New(slog.NewTextHandler(&logged, nil)), time.Now, once)
	if !issuedUnder(t, path, filepath.Join(identity, bus.AccountSeedFile)) {
		t.Error("the renewed credential is not under the installation's account")
	}
	if left := time.Until(credentialExpiry(t, path)); left < controlPlaneLife-time.Minute {
		t.Errorf("a credential expiring in %s was not renewed: it expires in %s\n%s", credentialWarning-time.Hour, left, logged.String())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o400 {
		t.Errorf("the renewed credential is mode %#o, and the one it replaced was 0400", info.Mode().Perm())
	}
}

// The API looks at the credential at start and every day after, renews it the first day it is
// inside the fourteen days, and where the renewal fails says why and tries again the next day,
// warning meanwhile.
func TestTheAPILooksEveryDayAndTriesAFailedRenewalAgain(t *testing.T) {
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := start
	expires := start.Add(16 * 24 * time.Hour)
	var tried []time.Time
	renew := func() (time.Time, error) {
		tried = append(tried, clock)
		if len(tried) < 3 {
			return time.Time{}, errors.New("the bus directory is read only")
		}
		expires = clock.Add(controlPlaneLife)
		return expires, nil
	}
	var logged bytes.Buffer
	var waited []time.Duration
	watchCredential(t.Context(), func() time.Time { return expires }, renew, slog.New(slog.NewTextHandler(&logged, nil)), func() time.Time { return clock }, func(_ context.Context, d time.Duration) bool {
		waited = append(waited, d)
		clock = clock.Add(d)
		return len(waited) < 10
	})
	want := []time.Time{start.Add(2 * 24 * time.Hour), start.Add(3 * 24 * time.Hour), start.Add(4 * 24 * time.Hour)}
	if !slices.Equal(tried, want) {
		t.Errorf("it tried to renew at %v, and it tries the first day inside the fourteen days and each day after until it succeeds", tried)
	}
	if slices.Max(waited) != 24*time.Hour {
		t.Errorf("it waited %v, and it looks at the credential every day", waited)
	}
	if n := strings.Count(logged.String(), "could not be renewed"); n != 2 || !strings.Contains(logged.String(), "read only") {
		t.Errorf("it said %d times that the renewal failed, and why:\n%s", n, logged.String())
	}
	if n := strings.Count(logged.String(), "expires soon"); n != 2 {
		t.Errorf("it warned %d times, once for each day the renewal failed:\n%s", n, logged.String())
	}

	// At start inside the fourteen days, it renews before it waits at all.
	clock, expires, tried, waited = start, start.Add(time.Hour), nil, nil
	renew = func() (time.Time, error) {
		tried = append(tried, clock)
		expires = clock.Add(controlPlaneLife)
		return expires, nil
	}
	watchCredential(t.Context(), func() time.Time { return expires }, renew, slog.New(slog.DiscardHandler), func() time.Time { return clock }, func(context.Context, time.Duration) bool { return false })
	if !slices.Equal(tried, []time.Time{start}) {
		t.Errorf("at start it tried to renew at %v", tried)
	}
}

// A renewal that cannot write beside the file, the bus directory being read only to the API,
// leaves the credential as it was and says so.
func TestARenewalThatCannotWriteLeavesTheCredential(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a directory whatever its mode")
	}
	identity := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", identity}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	seed, _ := os.ReadFile(filepath.Join(identity, bus.AccountSeedFile))
	account, err := natsjwt.ParseDecoratedNKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := account.Seed()
	issuer, err := bus.NewIssuer(string(raw), "tls://nats.example.com:4222")
	if err != nil {
		t.Fatal(err)
	}
	dir, path := sharedDirectory(t)
	sharedCredential(t, issuer, path, time.Now().Add(time.Hour), 0o600)
	before, _ := os.ReadFile(path)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if _, err := renewer(issuer, path, time.Now)(); err == nil || !strings.Contains(err.Error(), "could not be created") {
		t.Errorf("renewing in a directory the API may not write to answered %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the credential changed")
	}
}
