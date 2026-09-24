package bus

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// An installation's bus identity, held to a server that loads the configuration it wrote.
//
// The files are what the three programs are handed: the server includes accounts.conf, the API
// reads account.seed, and the API and the controller read control-plane.creds. So each is read here
// the way its program reads it, and a server is started from a configuration file that includes the
// one NewInstallation wrote, as an installation's does, rather than from options a test assembled.

// serveFrom starts a server whose own configuration enables JetStream and includes the
// accounts file, the way an installation's server configuration does, and answers its address.
func serveFrom(t *testing.T, in Installation, features ...string) string {
	t.Helper()
	// Beside the accounts file, since a server resolves an include against the directory of
	// the file that includes it.
	path := filepath.Join(filepath.Dir(in.Accounts), "nats-server.conf")
	conf := "host: 127.0.0.1\nport: -1\njetstream {\n  store_dir: " + `"` + t.TempDir() + `"` + "\n}\ninclude \"" + AccountsFile + "\"\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := natsserver.ProcessConfigFile(path)
	if err != nil {
		t.Fatalf("the server could not read the configuration: %s", err)
	}
	opts.NoLog, opts.NoSigs = true, true
	opts.FeatureFlags = map[string]bool{}
	for _, f := range features {
		opts.FeatureFlags[f] = true
	}
	server, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("the server refused the configuration: %s", err)
	}
	go server.Start()
	if !server.ReadyForConnections(10 * time.Second) {
		t.Fatal("the server did not come up")
	}
	t.Cleanup(server.Shutdown)
	return server.ClientURL()
}

// controlPlane reads the credential file as the API and the controller are given it.
func controlPlane(t *testing.T, in Installation, url string) Credentials {
	t.Helper()
	content, err := os.ReadFile(in.ControlPlane)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.ParseDecoratedJWT(content)
	if err != nil {
		t.Fatalf("the control plane's credential holds no JWT: %s", err)
	}
	user, err := jwt.ParseDecoratedUserNKey(content)
	if err != nil {
		t.Fatalf("the control plane's credential holds no user seed: %s", err)
	}
	seed, _ := user.Seed()
	return Credentials{Kind: Kind, URL: url, JWT: token, Seed: string(seed)}
}

func TestTheControlPlanesCredentialCreatesTheStreamsAndAPoolsConsumer(t *testing.T) {
	in, err := NewInstallation(t.TempDir(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	url := serveFrom(t, in)
	control := controlPlane(t, in, url)

	b, err := Open(t.Context(), Options{URL: url, Name: "controller", Credentials: &control})
	if err != nil {
		t.Fatalf("the control plane could not connect and create the streams: %s", err)
	}
	defer b.Close()
	if err := b.Consumer(t.Context(), "dmz"); err != nil {
		t.Fatalf("the control plane could not create the pool's consumer: %s", err)
	}
	for _, name := range []string{Stream, Results} {
		if _, err := b.js.Stream(t.Context(), name); err != nil {
			t.Errorf("the stream %s is not there: %s", name, err)
		}
	}
	if _, err := b.js.Consumer(t.Context(), Stream, Durable("dmz")); err != nil {
		t.Errorf("the pool's consumer is not there: %s", err)
	}
}

// The API's half: an Issuer built on the seed file mints a runner credential this server accepts,
// and it takes from its own pool's consumer and from nothing else.
func TestARunnerMintedWithTheInstallationsSeedTakesFromItsPoolAlone(t *testing.T) {
	in, err := NewInstallation(t.TempDir(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	url := serveFrom(t, in)
	control := controlPlane(t, in, url)
	b, err := Open(t.Context(), Options{URL: url, Name: "controller", Credentials: &control})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, pool := range []string{"dmz", "lan"} {
		if err := b.Consumer(t.Context(), pool); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Publish(t.Context(), message("mine", "pool=dmz")); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context(), message("theirs", "pool=lan")); err != nil {
		t.Fatal(err)
	}

	seed, err := os.ReadFile(in.AccountSeed)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewIssuer(strings.TrimSpace(string(seed)), url)
	if err != nil {
		t.Fatalf("the account seed file does not build an Issuer: %s", err)
	}
	minted, err := issuer.ForRunner("runner-1", "dmz", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := OpenRunner(Options{URL: minted.URL, Name: "runner-1", Credentials: &minted})
	if err != nil {
		t.Fatalf("the installation's bus refused a credential its API minted: %s", err)
	}
	defer runner.Close()

	taken, err := runner.Take(t.Context(), "dmz", 8, 3*time.Second)
	if err != nil {
		t.Fatalf("the runner could not take from its own pool: %s", err)
	}
	if len(taken) != 1 || taken[0].Task.Step != "mine" {
		t.Fatalf("the runner took %+v, and its pool holds one task", taken)
	}
	if err := taken[0].Held(t.Context()); err != nil {
		t.Fatalf("acknowledging: %s", err)
	}

	short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if got, err := runner.Take(short, "lan", 8, 300*time.Millisecond); err == nil {
		t.Errorf("a runner of dmz took %d tasks from lan", len(got))
	}
	conn, err := nats.Connect(url, nats.UserJWTAndSeed(minted.JWT, minted.Seed), nats.CustomInboxPrefix(Inbox("runner-1")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdateStream(short, jetstream.StreamConfig{Name: "MINE", Subjects: []string{"mine.>"}}); err == nil {
		t.Error("a runner created a stream")
	}
	if err := js.DeleteStream(short, Stream); err == nil {
		t.Error("a runner deleted the task stream")
	}
}

func TestAnInstallationsFilesAreItsOwnersAlone(t *testing.T) {
	until := time.Now().Add(time.Hour)
	in, err := NewInstallation(filepath.Join(t.TempDir(), "bus"), until)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{in.Accounts, in.AccountSeed, in.ControlPlane} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is written %v", filepath.Base(path), info.Mode().Perm())
		}
	}
	if info, err := os.Stat(filepath.Dir(in.Accounts)); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("the directory it made is %v", info.Mode().Perm())
	}

	// What each file holds is what its program reads, and nothing more: the seed is an
	// account's, the credential is the control plane's and expires when it was told to, and
	// the configuration names both accounts and no seed.
	seed, _ := os.ReadFile(in.AccountSeed)
	if s := strings.TrimSpace(string(seed)); !strings.HasPrefix(s, "SA") || strings.Contains(s, "\n") {
		t.Errorf("the account seed file holds %q", seed)
	}
	control := controlPlane(t, in, "nats://127.0.0.1:4222")
	claims, err := jwt.DecodeUserClaims(control.JWT)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != in.Account {
		t.Errorf("the control plane's credential was signed by %s, and the account is %s", claims.Issuer, in.Account)
	}
	if claims.Expires != until.Unix() {
		t.Errorf("the control plane's credential expires at %d, and it was asked to at %d", claims.Expires, until.Unix())
	}
	conf, _ := os.ReadFile(in.Accounts)
	for _, want := range []string{in.Account, in.System} {
		if !bytes.Contains(conf, []byte(want)) {
			t.Errorf("the configuration does not name %s", want)
		}
	}
	for _, secret := range []string{strings.TrimSpace(string(seed)), control.Seed} {
		if bytes.Contains(conf, []byte(secret)) {
			t.Error("the configuration holds a seed")
		}
	}
}

// An operator created twice is one the server trusts and one the API mints with, so a second call
// is refused, the first identity is left as it was, and a directory holding any one of the files
// is refused whole.
func TestAnInstallationIsCreatedOnce(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewInstallation(dir, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range []string{AccountsFile, AccountSeedFile, ControlPlaneFile} {
		before[name], _ = os.ReadFile(filepath.Join(dir, name))
	}
	if _, err := NewInstallation(dir, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a second installation was written over the first")
	}
	for name, content := range before {
		if now, _ := os.ReadFile(filepath.Join(dir, name)); !bytes.Equal(now, content) {
			t.Errorf("%s changed", name)
		}
	}

	for _, name := range []string{AccountsFile, AccountSeedFile, ControlPlaneFile} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("kept"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewInstallation(dir, time.Now().Add(time.Hour)); err == nil {
			t.Errorf("an installation was written beside an existing %s", name)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Errorf("refused beside %s, it still wrote %d files", name, len(entries)-1)
		}
		if kept, _ := os.ReadFile(filepath.Join(dir, name)); string(kept) != "kept" {
			t.Errorf("%s was written over", name)
		}
	}
}

func TestWhatAnInstallationIsNotCreatedWith(t *testing.T) {
	if _, err := NewInstallation("", time.Now().Add(time.Hour)); err == nil {
		t.Error("an installation was written in no directory")
	}
	dir := t.TempDir()
	if _, err := NewInstallation(dir, time.Time{}); err == nil {
		t.Error("an installation was written with a control plane credential that never expires")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused installation left %d files behind", len(entries))
	}
}
