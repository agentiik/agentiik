package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/token"
	"github.com/agentiik/agentiik/secret"
	"github.com/jackc/pgx/v5"
)

// agentiik-api init, a step at a time: each run leaves what the settings still say and changes
// what they no longer say, and keeps what is no setting, the keys, the password and the bus
// identity, from the first run on.

// prepared is one directory init prepares, and what every run of it said and gave to the agent.
type prepared struct {
	dir   string
	out   bytes.Buffer
	given map[string]bool
}

func aPreparedDirectory(t *testing.T) *prepared {
	t.Helper()
	return &prepared{dir: t.TempDir(), given: map[string]bool{}}
}

// at is a preparer over the directory at now, recording every path it gives the agent's account,
// since a test does not run as root and cannot give one away.
func (d *prepared) at(now time.Time) *preparer {
	p := newPreparer(d.dir, now, &d.out)
	p.chown = func(f *os.File) error {
		// A file is given away under its temporary name, then renamed into place.
		dir, base := filepath.Split(f.Name())
		if strings.HasPrefix(base, ".") {
			if i := strings.LastIndex(base, "-"); i > 0 {
				base = base[1:i]
			}
		}
		d.given[filepath.Join(dir, base)] = true
		return nil
	}
	return p
}

// files runs every step that needs no database, as initialize does before it migrates.
func (d *prepared) files(t *testing.T, now time.Time, host string, operator config.Secret) {
	t.Helper()
	p := d.at(now)
	if err := p.directories(); err != nil {
		t.Fatal(err)
	}
	if err := p.certificate(host); err != nil {
		t.Fatal(err)
	}
	if _, err := p.secrets(); err != nil {
		t.Fatal(err)
	}
	if err := p.operatorToken(operator); err != nil {
		t.Fatal(err)
	}
	if err := p.bus(); err != nil {
		t.Fatal(err)
	}
}

func (d *prepared) read(t *testing.T, parts ...string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(append([]string{d.dir}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// leaf is the certificate the API serves, as a client reads it.
func (d *prepared) leaf(t *testing.T) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(d.read(t, apiDir, "tls", "server.pem")))
	if block == nil {
		t.Fatal("api/tls/server.pem holds no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// firstRun is when a test's first run is, now rather than a fixed day, since the API refuses a
// certificate not yet valid and the bus a credential expired, on the real clock.
var firstRun = time.Now().UTC().Truncate(time.Second)

// A second run keeps every key, the database password, the bus identity and the certificate
// where the host is the same, byte for byte.
func TestInitKeepsWhatItMadeOnASecondRun(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "agentiik.example.com", "")
	kept := [][]string{
		{apiDir, "master-key"}, {apiDir, "presign-key"}, {apiDir, "database-password"},
		{controllerDir, "database-password"}, {apiDir, "operator-token.sha256"},
		{apiDir, "bus", bus.AccountsFile}, {apiDir, "bus", bus.AccountSeedFile}, {apiDir, "bus", bus.ControlPlaneFile},
		{controllerDir, "bus", bus.ControlPlaneFile}, {natsDir, bus.AccountsFile},
		{apiDir, "tls", "server.pem"}, {apiDir, "tls", "server.key"}, {natsDir, "server.pem"}, {natsDir, "server.key"},
	}
	before := map[string]string{}
	for _, path := range kept {
		before[filepath.Join(path...)] = d.read(t, path...)
	}
	d.out.Reset()
	d.files(t, firstRun.Add(time.Hour), "agentiik.example.com", "")
	for _, path := range kept {
		if d.read(t, path...) != before[filepath.Join(path...)] {
			t.Errorf("the second run changed %s", filepath.Join(path...))
		}
	}
	for _, said := range []string{"kept the certificate", "kept the master key", "kept the hash of the operator token", "kept the installation's bus identity"} {
		if !strings.Contains(d.out.String(), said) {
			t.Errorf("the second run did not say it %s:\n%s", said, d.out.String())
		}
	}
}

// Each secret is written in the format the program reading it takes, readable by its owner alone
// and given to the agent's account, and each copy the certificate and the bus identity are given
// is where its service reads it.
func TestInitWritesEachSecretAsItsReaderTakesIt(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "agentiik.example.com", "")

	if _, err := secret.ParseMaster([]byte(d.read(t, apiDir, "master-key"))); err != nil {
		t.Errorf("the master key is not one the API parses: %s", err)
	}
	if !strings.HasPrefix(d.read(t, apiDir, "master-key"), "id: "+firstRun.Format("2006-01")+"\n") {
		t.Errorf("the master key is not named after the month it was made in")
	}
	if password := strings.TrimSpace(d.read(t, apiDir, "database-password")); len(password) != 64 || strings.Trim(password, "0123456789abcdef") != "" {
		t.Errorf("the database password is not 32 bytes in hexadecimal")
	}
	if d.read(t, controllerDir, "database-password") != d.read(t, apiDir, "database-password") {
		t.Error("the controller's copy of the database password is not the API's")
	}

	// The API reads what init wrote, every file it names in the layout.
	env := map[string]string{
		config.DatabaseURL:          "postgres://agentiik@/agentiik?host=/run/postgresql",
		config.DatabasePasswordFile: filepath.Join(d.dir, apiDir, "database-password"),
		config.BusURL:               "tls://agentiik.example.com:4222",
		config.BusCredentialsFile:   filepath.Join(d.dir, apiDir, "bus", bus.ControlPlaneFile),
		config.BusAccountSeedFile:   filepath.Join(d.dir, apiDir, "bus", bus.AccountSeedFile),
		config.ObjectsDir:           filepath.Join(d.dir, objectsDir),
		config.PublicURL:            "https://agentiik.example.com:8443",
		config.PresignKeyFile:       filepath.Join(d.dir, apiDir, "presign-key"),
		config.MasterKeyFile:        filepath.Join(d.dir, apiDir, "master-key"),
		config.OperatorTokenFile:    filepath.Join(d.dir, apiDir, "operator-token.sha256"),
		config.Listen:               ":8443",
		config.TLSCertFile:          filepath.Join(d.dir, apiDir, "tls", "server.pem"),
		config.TLSKeyFile:           filepath.Join(d.dir, apiDir, "tls", "server.key"),
	}
	if _, err := readSettings(func(name string) (string, bool) { v, ok := env[name]; return v, ok }); err != nil {
		t.Errorf("the API refuses what init wrote:\n%s", err)
	}
	env[config.BusCredentialsFile] = filepath.Join(d.dir, controllerDir, "bus", bus.ControlPlaneFile)
	env[config.DatabasePasswordFile] = filepath.Join(d.dir, controllerDir, "database-password")
	delete(env, config.MasterKeyFile)
	delete(env, config.TLSCertFile)
	delete(env, config.TLSKeyFile)
	if _, err := config.ReadController(func(name string) (string, bool) { v, ok := env[name]; return v, ok }); err != nil {
		t.Errorf("the controller refuses what init wrote:\n%s", err)
	}

	for _, path := range [][]string{
		{apiDir, "master-key"}, {apiDir, "presign-key"}, {apiDir, "database-password"}, {apiDir, "operator-token.sha256"},
		{apiDir, "tls", "server.key"}, {controllerDir, "database-password"}, {controllerDir, "bus", bus.ControlPlaneFile},
		{natsDir, "server.key"}, {natsDir, bus.AccountsFile},
	} {
		info, err := os.Stat(filepath.Join(append([]string{d.dir}, path...)...))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is mode %#o, want 0600", filepath.Join(path...), info.Mode().Perm())
		}
	}
	// Given to the agent's account: what the API and the controller read. Not given: what the
	// bus reads, since it runs as root, and what the runner's join writes beside.
	for _, path := range [][]string{
		{apiDir}, {apiDir, "master-key"}, {apiDir, "presign-key"}, {apiDir, "database-password"}, {apiDir, "operator-token.sha256"},
		{apiDir, "tls", "server.pem"}, {apiDir, "tls", "server.key"}, {apiDir, "bus", bus.AccountSeedFile},
		{controllerDir, "database-password"}, {controllerDir, "bus", bus.ControlPlaneFile}, {objectsDir},
	} {
		if !d.given[filepath.Join(append([]string{d.dir}, path...)...)] {
			t.Errorf("%s was not given to uid %d", filepath.Join(path...), agent)
		}
	}
	for _, path := range [][]string{{natsDir, "server.key"}, {natsDir, bus.AccountsFile}, {runnerDir}} {
		if d.given[filepath.Join(append([]string{d.dir}, path...)...)] {
			t.Errorf("%s was given to uid %d, and it is not the agent's", filepath.Join(path...), agent)
		}
	}

	conf := d.read(t, natsDir, "nats.conf")
	for _, want := range []string{`listen: "0.0.0.0:4222"`, `cert_file: "/nats/server.pem"`, `store_dir: "/nats/jetstream"`, `include "accounts.conf"`} {
		if !strings.Contains(conf, want) {
			t.Errorf("nats.conf does not say %s:\n%s", want, conf)
		}
	}
	for _, trust := range [][]string{{apiDir, "tls", "server.pem"}, {apiDir, "trust", "agentiik.pem"}, {controllerDir, "trust", "agentiik.pem"}, {runnerDir, "trust", "agentiik.pem"}, {natsDir, "server.pem"}} {
		if d.read(t, trust...) != d.read(t, apiDir, "tls", "server.pem") {
			t.Errorf("%s is not the certificate", filepath.Join(trust...))
		}
		// Readable by everybody, since the runner's, which is root's, is read by the runner
		// once it has dropped to the agent's account.
		if info, _ := os.Stat(filepath.Join(append([]string{d.dir}, trust...)...)); info.Mode().Perm() != 0o644 {
			t.Errorf("%s is mode %#o, want 0644", filepath.Join(trust...), info.Mode().Perm())
		}
	}
}

// The certificate is issued to the host, localhost and 127.0.0.1, signed by an ECDSA P-256 key,
// valid 825 days, for a server, and is no authority; a client that trusts it reaches the host.
func TestInitMakesACertificateForTheHost(t *testing.T) {
	for host, names := range map[string][]string{
		"agentiik.example.com": {"agentiik.example.com", "localhost", "127.0.0.1"},
		"192.0.2.10":           {"192.0.2.10", "localhost", "127.0.0.1"},
		"localhost":            {"localhost", "127.0.0.1"},
	} {
		d := aPreparedDirectory(t)
		d.files(t, firstRun, host, "")
		c := d.leaf(t)
		var got []string
		got = append(got, c.DNSNames...)
		for _, ip := range c.IPAddresses {
			got = append(got, ip.String())
		}
		slices.Sort(got)
		slices.Sort(names)
		if !slices.Equal(got, names) {
			t.Errorf("the certificate for %s names %v, want %v", host, got, names)
		}
		if c.PublicKeyAlgorithm != x509.ECDSA || c.IsCA || !c.NotAfter.Equal(firstRun.Add(825*24*time.Hour)) || !c.NotBefore.Equal(firstRun.Add(-time.Hour)) || !slices.Equal(c.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
			t.Errorf("the certificate for %s is %v, an authority %v, until %s, for %v", host, c.PublicKeyAlgorithm, c.IsCA, c.NotAfter, c.ExtKeyUsage)
		}
		roots := x509.NewCertPool()
		roots.AddCert(c)
		if _, err := c.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, CurrentTime: firstRun.Add(time.Hour)}); err != nil {
			t.Errorf("a client trusting agentiik.pem refuses %s: %s", host, err)
		}
	}
}

// A host changed, a certificate expired, or a key that is not the certificate's has a new
// certificate made; one from elsewhere that still names the host is kept.
func TestInitReplacesTheCertificateWhereItNoLongerServes(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "agentiik.example.com", "")
	first := d.read(t, apiDir, "tls", "server.pem")

	d.out.Reset()
	d.files(t, firstRun.Add(time.Hour), "other.example.com", "")
	if d.read(t, apiDir, "tls", "server.pem") == first || d.leaf(t).VerifyHostname("other.example.com") != nil {
		t.Fatal("a new host kept the certificate of the old one")
	}
	if !strings.Contains(d.out.String(), "was not issued to other.example.com") {
		t.Errorf("replacing it said:\n%s", d.out.String())
	}
	for _, copy := range [][]string{{natsDir, "server.pem"}, {runnerDir, "trust", "agentiik.pem"}} {
		if d.read(t, copy...) != d.read(t, apiDir, "tls", "server.pem") {
			t.Errorf("%s was not given the new certificate", filepath.Join(copy...))
		}
	}
	if d.read(t, natsDir, "server.key") != d.read(t, apiDir, "tls", "server.key") {
		t.Error("the bus was not given the new certificate's key")
	}

	second := d.read(t, apiDir, "tls", "server.pem")
	expiry := d.leaf(t).NotAfter
	d.out.Reset()
	d.files(t, expiry, "other.example.com", "")
	if d.read(t, apiDir, "tls", "server.pem") == second || !strings.Contains(d.out.String(), "expired at") {
		t.Errorf("an expired certificate was kept:\n%s", d.out.String())
	}

	// A certificate that names the host through a wildcard names it, and is kept.
	certPEM, keyPEM, err := selfSigned("*.example.com", firstRun)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"server.pem": certPEM, "server.key": keyPEM} {
		path := filepath.Join(d.dir, apiDir, "tls", name)
		os.Remove(path)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chmod(path, 0o644)
	}
	d.given = map[string]bool{}
	d.files(t, firstRun.Add(2*time.Hour), "other.example.com", "")
	if d.read(t, apiDir, "tls", "server.pem") != string(certPEM) {
		t.Error("a certificate naming the host was replaced")
	}
	if info, err := os.Stat(filepath.Join(d.dir, apiDir, "tls", "server.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the key put there was left readable by others: %v", info.Mode())
	}
	for _, name := range []string{"server.pem", "server.key"} {
		if !d.given[filepath.Join(d.dir, apiDir, "tls", name)] {
			t.Errorf("the %s put there was not given to uid %d", name, agent)
		}
	}

	// A key that is not the certificate's.
	_, otherKey, err := selfSigned("other.example.com", firstRun)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.dir, apiDir, "tls", "server.key"), otherKey, 0o600); err != nil {
		t.Fatal(err)
	}
	d.out.Reset()
	d.files(t, firstRun.Add(3*time.Hour), "other.example.com", "")
	if _, err := tls.X509KeyPair([]byte(d.read(t, apiDir, "tls", "server.pem")), []byte(d.read(t, apiDir, "tls", "server.key"))); err != nil {
		t.Errorf("a certificate whose key is another's was kept: %s", err)
	}
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]) + "\n"
}

// With no token set and none stored, init mints one, prints it once and stores its hash; later
// runs with none set keep that hash and print no token; a token set replaces the hash, and so does
// a token changed.
func TestInitKeepsTheOperatorTokensHashAndNeverTheToken(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "localhost", "")
	out := d.out.String()
	i := strings.Index(out, operatorTokenPrefix)
	if i < 0 {
		t.Fatalf("no token was printed:\n%s", out)
	}
	minted := strings.Fields(out[i:])[0]
	// Named by no variable, since a Compose file sets it through one of its own, and told where
	// it is kept: in the installation's settings, wherever those are.
	if strings.Contains(out, config.OperatorToken) || !strings.Contains(out, "shown this once") || !strings.Contains(out, "where the installation's settings are") {
		t.Errorf("minting the token says:\n%s", out)
	}
	if d.read(t, apiDir, "operator-token.sha256") != hashOf(minted) {
		t.Fatal("the hash stored is not the printed token's")
	}
	if _, err := newOperator(strings.TrimSpace(d.read(t, apiDir, "operator-token.sha256"))); err != nil {
		t.Errorf("the API refuses the hash: %s", err)
	}
	for _, name := range []string{"api", "controller", "runner", "nats"} {
		filepath.WalkDir(filepath.Join(d.dir, name), func(path string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if content, _ := os.ReadFile(path); bytes.Contains(content, []byte(minted)) {
					t.Errorf("%s holds the token", path)
				}
			}
			return nil
		})
	}

	d.out.Reset()
	d.files(t, firstRun.Add(time.Hour), "localhost", "")
	if strings.Contains(d.out.String(), operatorTokenPrefix) || d.read(t, apiDir, "operator-token.sha256") != hashOf(minted) {
		t.Errorf("a run with no token set did not keep the hash, or printed a token:\n%s", d.out.String())
	}

	chosen := config.Secret("a-token-of-my-own-at-least-32-characters")
	d.out.Reset()
	d.files(t, firstRun.Add(2*time.Hour), "localhost", chosen)
	if d.read(t, apiDir, "operator-token.sha256") != hashOf(string(chosen)) || strings.Contains(d.out.String(), string(chosen)) {
		t.Errorf("a token set after a minted one did not replace its hash, or was printed:\n%s", d.out.String())
	}
	changed := config.Secret("another-token-of-my-own-32-characters")
	d.files(t, firstRun.Add(3*time.Hour), "localhost", changed)
	if d.read(t, apiDir, "operator-token.sha256") != hashOf(string(changed)) {
		t.Error("a token changed did not replace the hash")
	}
	d.out.Reset()
	d.files(t, firstRun.Add(4*time.Hour), "localhost", "")
	if d.read(t, apiDir, "operator-token.sha256") != hashOf(string(changed)) || strings.Contains(d.out.String(), operatorTokenPrefix) {
		t.Error("unsetting the token minted another, rather than keeping the hash of the last one set")
	}
}

// The control plane's credential is renewed once the API would warn of it, under the same account,
// and the controller is given the renewed one; before that, it is kept.
func TestInitRenewsTheControlPlanesCredentialBeforeItExpires(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "localhost", "")
	creds := filepath.Join(d.dir, apiDir, "bus", bus.ControlPlaneFile)
	first, err := controlPlaneExpiry(creds)
	if err != nil {
		t.Fatal(err)
	}
	seed := d.read(t, apiDir, "bus", bus.AccountSeedFile)

	d.files(t, first.Add(-credentialWarning-time.Hour), "localhost", "")
	if kept, _ := controlPlaneExpiry(creds); !kept.Equal(first) {
		t.Fatalf("the credential was renewed %s before it expires", credentialWarning+time.Hour)
	}

	later := first.Add(-credentialWarning + time.Hour)
	d.out.Reset()
	d.files(t, later, "localhost", "")
	renewed, err := controlPlaneExpiry(creds)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Sub(later) < controlPlaneLife-time.Second || !strings.Contains(d.out.String(), "renewed") {
		t.Errorf("inside the warning, the credential now expires at %s:\n%s", renewed, d.out.String())
	}
	if d.read(t, apiDir, "bus", bus.AccountSeedFile) != seed {
		t.Error("renewing changed the account")
	}
	if d.read(t, controllerDir, "bus", bus.ControlPlaneFile) != d.read(t, apiDir, "bus", bus.ControlPlaneFile) {
		t.Error("the controller was not given the renewed credential")
	}
}

// Directories a volume came with, open to everybody, are closed to what their programs accept.
func TestInitClosesTheDirectoriesItIsGiven(t *testing.T) {
	d := aPreparedDirectory(t)
	for _, name := range []string{apiDir, natsDir, objectsDir} {
		if err := os.Mkdir(filepath.Join(d.dir, name), 0o777); err != nil {
			t.Fatal(err)
		}
		os.Chmod(filepath.Join(d.dir, name), 0o777)
	}
	d.files(t, firstRun, "localhost", "")
	for _, name := range []string{apiDir, natsDir, objectsDir} {
		if info, _ := os.Stat(filepath.Join(d.dir, name)); info.Mode().Perm() != 0o700 {
			t.Errorf("%s is left mode %#o", name, info.Mode().Perm())
		}
	}
}

// Every setting init reads is refused by name before anything is written, a namespace the API
// would refuse among them.
func TestInitRefusesItsSettingsBeforeWritingAnything(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		config.InitDir:            dir,
		config.InitHost:           "https://agentiik.example.com",
		config.InitNamespace:      "runs",
		config.OperatorToken:      "short",
		config.MigrateDatabaseURL: "postgres://postgres@/agentiik?host=/run/postgresql",
		config.DatabaseURL:        "postgres://agentiik@/agentiik?host=/run/postgresql",
	}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"init"}, lookup, &stdout, &stderr); code != exitFailed {
		t.Fatalf("init exited %d:\n%s", code, stderr.String())
	}
	for _, name := range []string{config.InitHost, config.OperatorToken} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("the refusal does not name %s:\n%s", name, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "short") {
		t.Errorf("the refusal repeats the token:\n%s", stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) > 0 || stdout.Len() > 0 {
		t.Errorf("init wrote %d entries, and said %q", len(entries), stdout.String())
	}

	env[config.InitHost] = "agentiik.example.com"
	env[config.OperatorToken] = strings.Repeat("a", 32)
	stderr.Reset()
	if code := run(t.Context(), []string{"init"}, lookup, &stdout, &stderr); code != exitFailed || !strings.Contains(stderr.String(), config.InitNamespace) || !strings.Contains(stderr.String(), "routes on") {
		t.Errorf("a namespace the API refuses exited %d:\n%s", code, stderr.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("init wrote %d entries for a namespace it refuses", len(entries))
	}
}

// Against a database: the migration, the namespace and the runner's join token, each run, and a
// second run with another namespace creating it, with a fresh join token and the password kept.
func TestInitMigratesCreatesTheNamespaceAndIssuesAJoinTokenAtEveryRun(t *testing.T) {
	database := freshDatabase(t)
	d := aPreparedDirectory(t)
	c := config.Init{
		Dir: d.dir, Host: "localhost", Namespace: "demo",
		Admin: database.Admin, Application: config.Database{URL: database.Application.URL, Role: database.Application.Role},
	}
	if err := initialize(t.Context(), c, d.at(firstRun)); err != nil {
		t.Fatalf("%s\n%s", err, d.out.String())
	}
	firstToken := strings.TrimSpace(d.read(t, runnerDir, "join-token"))
	if info, err := os.Stat(filepath.Join(d.dir, runnerDir, "join-token")); err != nil || info.Mode().Perm() != 0o600 || !d.given[filepath.Join(d.dir, runnerDir, "join-token")] {
		t.Errorf("the join token is not the runner's alone: %v", err)
	}

	admin, err := pgx.Connect(t.Context(), database.Admin.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.WithoutCancel(t.Context()))
	joinToken := func(clear string) (pool, by string, expires time.Time, redeemed bool) {
		t.Helper()
		var at *time.Time
		if err := admin.QueryRow(t.Context(), `select pool, issued_by, expires_at, redeemed_at from join_tokens where hash = $1`, token.Hash(clear)).Scan(&pool, &by, &expires, &at); err != nil {
			t.Fatalf("the token in the file is no join token of the installation: %s", err)
		}
		return pool, by, expires, at != nil
	}
	if pool, by, expires, redeemed := joinToken(firstToken); pool != "default" || by != "operator" || !expires.Equal(firstRun.Add(time.Hour)) || redeemed {
		t.Errorf("the join token is of %s, by %s, until %s, redeemed %v", pool, by, expires, redeemed)
	}
	if !exists(t, admin, "demo") {
		t.Error("the namespace was not created")
	}
	if got := audited(t, admin); !slices.Contains(got, "operator namespace.create demo done") || !strings.HasPrefix(got[len(got)-1], "operator join_token.issue ") {
		t.Errorf("the audit log holds %q", got)
	}

	password := d.read(t, apiDir, "database-password")
	c.Namespace = "team"
	d.out.Reset()
	if err := initialize(t.Context(), c, d.at(firstRun.Add(time.Minute))); err != nil {
		t.Fatalf("the second run: %s\n%s", err, d.out.String())
	}
	if !exists(t, admin, "team") || !exists(t, admin, "demo") {
		t.Error("the second run did not create the namespace it was given")
	}
	if !strings.Contains(d.out.String(), "every migration was already applied") {
		t.Errorf("the second run migrated again:\n%s", d.out.String())
	}
	if d.read(t, apiDir, "database-password") != password {
		t.Error("the second run changed the database password")
	}
	second := strings.TrimSpace(d.read(t, runnerDir, "join-token"))
	if second == firstToken {
		t.Error("the second run left the runner the same join token")
	}
	joinToken(second)
	// The first is left to expire, since a runner may be presenting it.
	if _, _, _, redeemed := joinToken(firstToken); redeemed {
		t.Error("the first join token was spent")
	}
}

// init goes through run, as its Compose service calls it.
func TestInitIsAVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"init", "now"}, empty, &stdout, &stderr); code != exitUsage {
		t.Errorf("init with an argument exited %d", code)
	}
	if code := run(t.Context(), []string{"init"}, empty, io.Discard, &stderr); code != exitFailed || !strings.Contains(stderr.String(), config.InitDir) {
		t.Errorf("init with nothing set exited %d:\n%s", code, stderr.String())
	}
}

// A token init mints is printed, and its hash kept, only by a run that got to the end: one printed
// by a run that failed would be in the log of a container the next run replaces.
func TestInitPrintsAMintedTokenOnlyOnceTheRunSucceeds(t *testing.T) {
	d := aPreparedDirectory(t)
	c := config.Init{
		Dir: d.dir, Host: "localhost", Namespace: "demo",
		Admin:       config.Database{URL: "postgres://postgres@/agentiik?host=" + filepath.Join(d.dir, "no-socket"), Role: "postgres"},
		Application: config.Database{URL: "postgres://agentiik@/agentiik?host=" + filepath.Join(d.dir, "no-socket"), Role: "agentiik"},
	}
	if err := initialize(t.Context(), c, d.at(firstRun)); err == nil {
		t.Fatal("a run with no database reached succeeded")
	}
	if strings.Contains(d.out.String(), operatorTokenPrefix) {
		t.Errorf("a run that failed printed a token:\n%s", d.out.String())
	}
	if _, err := os.Stat(filepath.Join(d.dir, apiDir, "operator-token.sha256")); err == nil {
		t.Error("a run that failed kept a token's hash")
	}
}

// A bus identity whose creation was cut off part way is finished where the bus never had it, and
// refused where it did.
func TestInitFinishesABusIdentityCutOffPartWay(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "localhost", "")
	dir := filepath.Join(d.dir, apiDir, "bus")
	seed := d.read(t, apiDir, "bus", bus.AccountSeedFile)

	os.Remove(filepath.Join(dir, bus.ControlPlaneFile))
	d.files(t, firstRun.Add(time.Hour), "localhost", "")
	if d.read(t, apiDir, "bus", bus.AccountSeedFile) != seed || d.read(t, controllerDir, "bus", bus.ControlPlaneFile) != d.read(t, apiDir, "bus", bus.ControlPlaneFile) {
		t.Error("a missing credential was not minted under the same account and given to the controller")
	}

	os.Remove(filepath.Join(dir, bus.ControlPlaneFile))
	os.Remove(filepath.Join(dir, bus.AccountSeedFile))
	p := d.at(firstRun.Add(2 * time.Hour))
	if err := p.bus(); err == nil || !strings.Contains(err.Error(), "the bus was given its accounts") {
		t.Errorf("an identity the bus had was made again: %v", err)
	}
	os.Remove(filepath.Join(d.dir, natsDir, bus.AccountsFile))
	d.files(t, firstRun.Add(3*time.Hour), "localhost", "")
	if d.read(t, apiDir, "bus", bus.AccountSeedFile) == seed {
		t.Error("an identity the bus never had was not made again")
	}
}

// A link or a pipe a service put in the volume it shares with init, which runs as root, is never
// followed or waited on.
func TestInitFollowsNoLinkAServicePutInItsVolume(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "localhost", "")
	elsewhere := filepath.Join(t.TempDir(), "superuser-password")
	if err := os.WriteFile(elsewhere, []byte("SUPERUSER-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(d.dir, apiDir, "database-password")
	os.Remove(password)
	if err := os.Symlink(elsewhere, password); err != nil {
		t.Fatal(err)
	}
	if _, err := d.at(firstRun).secrets(); err == nil || strings.Contains(d.read(t, controllerDir, "database-password"), "SUPERUSER") {
		t.Errorf("a link in place of the database password was followed: %v", err)
	}
	os.Remove(password)

	trust := filepath.Join(d.dir, controllerDir, "trust")
	os.RemoveAll(trust)
	if err := os.Symlink(filepath.Join(d.dir, apiDir), trust); err != nil {
		t.Fatal(err)
	}
	if err := d.at(firstRun).directories(); err == nil {
		t.Error("a link in place of a directory was taken for it")
	}
	if info, _ := os.Stat(filepath.Join(d.dir, apiDir)); info.Mode().Perm() != 0o700 {
		t.Errorf("the API's directory was opened to mode %#o through a link", info.Mode().Perm())
	}
	os.Remove(trust)

	pipe := filepath.Join(d.dir, controllerDir, "database-password")
	os.Remove(pipe)
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.at(firstRun).secrets()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a pipe in place of the controller's password held init")
	}
	if info, err := os.Lstat(pipe); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the pipe was not replaced by the password: %v", err)
	}
}

// A file init keeps, a secret or a copy, whose mode or owner changed since, is given them back at
// the next run, since the API refuses a secret anybody else may read.
func TestInitPutsBackTheModeAndOwnerOfWhatItKeeps(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "localhost", "")
	kept := [][]string{{apiDir, "master-key"}, {apiDir, "presign-key"}, {controllerDir, "database-password"}, {apiDir, "operator-token.sha256"}}
	for _, path := range append(kept, []string{runnerDir, "trust", "agentiik.pem"}) {
		os.Chmod(filepath.Join(append([]string{d.dir}, path...)...), 0o640)
	}
	d.given = map[string]bool{}
	d.files(t, firstRun.Add(time.Hour), "localhost", "")
	for _, path := range kept {
		full := filepath.Join(append([]string{d.dir}, path...)...)
		if info, _ := os.Stat(full); info.Mode().Perm() != 0o600 || !d.given[full] {
			t.Errorf("%s is left mode %#o, given to uid %d: %v", filepath.Join(path...), info.Mode().Perm(), agent, d.given[full])
		}
	}
	if info, _ := os.Stat(filepath.Join(d.dir, runnerDir, "trust", "agentiik.pem")); info.Mode().Perm() != 0o644 {
		t.Errorf("the runner's certificate is left mode %#o", info.Mode().Perm())
	}
}

// A link in place of the certificate's key, which the API owns, is never followed by root: the
// file it names keeps its mode, and the link is replaced by a key.
func TestInitFollowsNoLinkInPlaceOfTheCertificatesKey(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "localhost", "")
	// The very key, so that a run that followed the link would find the pair whole and keep
	// it, and give the file the link names the key's mode and owner.
	elsewhere := filepath.Join(t.TempDir(), "root-only")
	if err := os.WriteFile(elsewhere, []byte(d.read(t, apiDir, "tls", "server.key")), 0o400); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(d.dir, apiDir, "tls", "server.key")
	os.Remove(key)
	if err := os.Symlink(elsewhere, key); err != nil {
		t.Fatal(err)
	}
	d.given = map[string]bool{}
	d.files(t, firstRun.Add(time.Hour), "localhost", "")
	if info, _ := os.Stat(elsewhere); info.Mode().Perm() != 0o400 || d.given[elsewhere] {
		t.Errorf("the file the link names was changed through it: mode %#o", info.Mode().Perm())
	}
	if info, err := os.Lstat(key); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the link was not replaced by a key: %v", err)
	}
}

// aCertificate is a certificate for host valid from notBefore to notAfter and its key, as PEM:
// signed by an authority of its own where byAuthority is true, as a person's from elsewhere is,
// and otherwise signed by itself with a common name and nothing else, as setup issued them.
func aCertificate(t *testing.T, host string, notBefore, notAfter time.Time, byAuthority bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: []string{host},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
	}
	parent, signer := leaf, any(key)
	if byAuthority {
		authorityKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		parent = &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "An authority", Organization: []string{"Example"}},
			NotBefore: notBefore, NotAfter: notAfter.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign,
		}
		signer = authorityKey
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}

// put puts a certificate and its key in the API's tls directory, in place of what is there.
func (d *prepared) put(t *testing.T, certPEM, keyPEM []byte) {
	t.Helper()
	for name, content := range map[string][]byte{"server.pem": certPEM, "server.key": keyPEM} {
		path := filepath.Join(d.dir, apiDir, "tls", name)
		os.Remove(path)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A certificate init did not issue is never replaced: kept while it serves, and a run where it no
// longer names the host, has expired or is not its key's is refused, saying why and what to do.
// One init issued, or setup before it, is replaced.
func TestInitReplacesOnlyACertificateItIssued(t *testing.T) {
	d := aPreparedDirectory(t)
	d.files(t, firstRun, "agentiik.example.com", "")
	if !slices.Contains(d.leaf(t).Subject.Organization, issuer) {
		t.Fatalf("the certificate init issued is not marked as init's: %v", d.leaf(t).Subject)
	}

	theirs, theirKey := aCertificate(t, "agentiik.example.com", firstRun.Add(-time.Hour), firstRun.Add(90*24*time.Hour), true)
	d.put(t, theirs, theirKey)
	d.files(t, firstRun.Add(time.Hour), "agentiik.example.com", "")
	if d.read(t, apiDir, "tls", "server.pem") != string(theirs) {
		t.Fatal("a certificate from an authority, naming the host, was not kept")
	}

	_, otherKey := aCertificate(t, "agentiik.example.com", firstRun, firstRun.Add(time.Hour), true)
	for what, c := range map[string]struct {
		host    string
		at      time.Time
		key     []byte
		saysWhy string
	}{
		"another host":  {"other.example.com", firstRun.Add(time.Hour), theirKey, "not issued to other.example.com"},
		"expired":       {"agentiik.example.com", firstRun.Add(91 * 24 * time.Hour), theirKey, "expired at"},
		"another's key": {"agentiik.example.com", firstRun.Add(time.Hour), otherKey, "its key is not the certificate's"},
	} {
		d.put(t, theirs, c.key)
		err := d.at(c.at).certificate(c.host)
		if err == nil || !strings.Contains(err.Error(), "not one init issued") || !strings.Contains(err.Error(), c.saysWhy) || !strings.Contains(err.Error(), "remove both") {
			t.Errorf("%s: a person's certificate was not refused as such: %v", what, err)
		}
		if d.read(t, apiDir, "tls", "server.pem") != string(theirs) || d.read(t, apiDir, "tls", "server.key") != string(c.key) {
			t.Errorf("%s: a person's certificate was replaced", what)
		}
	}

	// One setup issued before init existed: signed by itself, its common name among its names.
	setups, setupKey := aCertificate(t, "agentiik.example.com", firstRun.Add(-time.Hour), firstRun.Add(825*24*time.Hour), false)
	d.put(t, setups, setupKey)
	d.out.Reset()
	d.files(t, firstRun.Add(2*time.Hour), "other.example.com", "")
	if d.read(t, apiDir, "tls", "server.pem") == string(setups) || d.leaf(t).VerifyHostname("other.example.com") != nil {
		t.Errorf("a certificate setup issued, for another host, was not replaced:\n%s", d.out.String())
	}
}
