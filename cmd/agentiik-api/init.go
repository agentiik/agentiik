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
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/config"
	"github.com/nats-io/jwt/v2"
)

// agent is the account every Agentiik image runs as, which owns what the API, the controller and
// the runner read.
const agent = 65532

// certificateLife is how long the certificate init makes is valid: 825 days, the longest a
// certificate for a server was accepted for by the browsers and the operating systems that
// capped it, so that a client strict about it still takes this one.
const certificateLife = 825 * 24 * time.Hour

// The subdirectories of AGK_INIT_DIR, one per service, each a volume the Compose file mounts in
// that service and in init. What goes in each, and who owns it, is in layout's comment.
const (
	apiDir        = "api"
	controllerDir = "controller"
	natsDir       = "nats"
	busDir        = "bus"
	runnerDir     = "runner"
	objectsDir    = "objects"
)

// layout is where init writes everything, under one directory.
//
//	api/           mounted at /agentiik in the API, owned by agent, 0700
//	  master-key, presign-key, database-password, operator-token.sha256
//	  bus/          accounts.conf and account.seed, as bus-init writes them
//	  tls/          server.pem and server.key, the certificate the API and the bus serve
//	  trust/        agentiik.pem, the certificate, trusted through SSL_CERT_DIR
//	controller/    mounted at /agentiik in the controller, owned by agent, 0700
//	  database-password, trust/agentiik.pem
//	bus/           mounted at /bus, read and write in the API, read only in the controller,
//	               owned by agent, 0700
//	  control-plane.creds, which the API renews itself
//	nats/          mounted at /nats in the bus, which runs as root, 0700
//	  nats.conf, accounts.conf, server.pem, server.key, jetstream/
//	runner/        mounted at /etc/agentiik in the runner, 0755
//	  trust/agentiik.pem, join-token, and what join writes there
//	objects/       mounted at /objects in the API and the controller, owned by agent, 0700
//
// Each service is given its own copy of what it reads rather than a directory of another's, so
// that the controller never mounts the one holding the master key, the bus never the one holding
// the account seed, and the runner nothing of the control plane's but the certificate. The control
// plane's credential is the one file two services share, in a directory holding nothing else: the
// API renews it while it runs, and a copy it wrote for the controller in the controller's own
// directory would be a directory holding the controller's database password that the API writes
// to. The controller only reads it, and reads nothing of the API's there.
type layout string

func (l layout) path(parts ...string) string {
	return filepath.Join(append([]string{string(l)}, parts...)...)
}

// preparer does one run of init, over the files alone: every step but the database's.
type preparer struct {
	dir layout
	now time.Time
	out io.Writer

	// chown gives an open file to the agent's account. It is a fchown where init runs as root,
	// as it does in its Compose service, and nothing where it does not, which only a test does,
	// since nobody else may give a file away. On the descriptor rather than a path, so that a
	// link a service put in its own volume is never followed by root.
	chown func(f *os.File) error
}

func newPreparer(dir string, now time.Time, out io.Writer) *preparer {
	p := &preparer{dir: layout(dir), now: now, out: out, chown: func(*os.File) error { return nil }}
	if os.Geteuid() == 0 {
		p.chown = func(f *os.File) error { return f.Chown(agent, agent) }
	}
	return p
}

func (p *preparer) say(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// initVerb is agentiik-api init.
func initVerb(ctx context.Context, lookup config.Lookup, stdout, stderr io.Writer) int {
	c, err := config.ReadInit(lookup)
	if err == nil {
		// Held to what the API holds a namespace to before anything is written, as namespace
		// create does, so that a name no namespace can have is refused on its own.
		if err = api.NamespaceName(c.Namespace); err != nil {
			err = config.Refuse(config.InitNamespace, err)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s init: the configuration refuses the start:\n%s\n", program, err)
		return exitFailed
	}
	if err := initialize(ctx, c, newPreparer(c.Dir, time.Now(), stdout)); err != nil {
		fmt.Fprintf(stderr, "%s init: %s\n", program, err)
		return exitFailed
	}
	return exitStopped
}

// initialize is one run of init: every file, then the database. Each step leaves what is there
// when it is what the settings say, and changes it where it is not, so that a run after a setting
// changed applies the change, and a run after a failure finishes what the failed one started.
//
// The keys, the database password and the bus identity are made once and kept: they are not
// settings, and a new one would be an installation that no longer reads what it wrote.
func initialize(ctx context.Context, c config.Init, p *preparer) error {
	if err := p.directories(); err != nil {
		return err
	}
	if err := p.certificate(c.Host); err != nil {
		return err
	}
	password, err := p.secrets()
	if err != nil {
		return err
	}
	if err := p.bus(); err != nil {
		return err
	}
	application := c.Application
	application.Password = password
	if err := p.database(ctx, config.Migration{Admin: c.Admin, Application: application}, c.Namespace); err != nil {
		return err
	}
	// Last, so that a token init mints is printed by the run that succeeds: one printed by a
	// run that then failed would be in the log of a container the next docker compose up
	// replaces, with its hash kept and nobody holding it.
	return p.operatorToken(c.OperatorToken)
}

// directories makes every directory of the layout, or puts back the mode and the owner of one that
// exists: a volume Docker made for a service is root's and open to everybody, and the programs
// refuse a secret's directory anybody else may write to.
func (p *preparer) directories() error {
	for _, d := range []struct {
		path    string
		mode    fs.FileMode
		toAgent bool
	}{
		{p.dir.path(apiDir), 0o700, true},
		{p.dir.path(apiDir, "bus"), 0o700, true},
		{p.dir.path(apiDir, "tls"), 0o700, true},
		{p.dir.path(apiDir, "trust"), 0o755, true},
		{p.dir.path(controllerDir), 0o700, true},
		{p.dir.path(controllerDir, "trust"), 0o755, true},
		{p.dir.path(busDir), 0o700, true},
		{p.dir.path(natsDir), 0o700, false},
		{p.dir.path(natsDir, "jetstream"), 0o700, false},
		{p.dir.path(runnerDir), 0o755, false},
		{p.dir.path(runnerDir, "trust"), 0o755, false},
		{p.dir.path(objectsDir), 0o700, true},
	} {
		// Parents come before their children in the list, so each is made on one that was
		// checked already, and never through a link.
		if err := os.Mkdir(d.path, d.mode); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("the directory %s could not be made: %w", d.path, err)
		}
		f, err := os.OpenFile(d.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return fmt.Errorf("%s is not a directory, and init keeps one there: remove it, and run init again: %w", d.path, err)
		}
		err = p.give(f, d.mode, d.toAgent)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// certificate makes the certificate the bus serves, and the API where no proxy is in front, and
// gives each service its copy.
//
// The one in api/tls is kept while it still names host, has not expired and pairs with its key.
// One init issued that does not, which is AGK_INIT_HOST changed or 825 days gone by, is replaced
// by a new one, signed by itself: clients then trust the new agentiik.pem. One a person put
// there, from an authority clients already trust, is never replaced: a run where it no longer
// serves is refused, saying why and what to do, since replacing it would move every client from
// an authority it trusts to a certificate it does not, without a word.
func (p *preparer) certificate(host string) error {
	certPath, keyPath := p.dir.path(apiDir, "tls", "server.pem"), p.dir.path(apiDir, "tls", "server.key")
	switch reason, issued := p.keeps(certPath, keyPath, host); {
	case reason == "":
		p.say("kept the certificate for %s", host)
	case !issued:
		return fmt.Errorf("the certificate in %s is not one init issued, and %s, so it is not replaced: put there a certificate for %s and its key, or remove both for init to issue its own", filepath.Dir(certPath), reason, host)
	default:
		certPEM, keyPEM, err := selfSigned(host, p.now)
		if err != nil {
			return err
		}
		if err := p.write(keyPath, keyPEM, 0o600, true); err != nil {
			return err
		}
		if err := p.write(certPath, certPEM, 0o644, true); err != nil {
			return err
		}
		p.say("made a certificate for %s, valid %d days, since %s", strings.Join(names(host), ", "), int(certificateLife.Hours()/24), reason)
	}
	certPEM, err := readRegular(certPath)
	if err != nil {
		return fmt.Errorf("the certificate could not be read back: %w", err)
	}
	keyPEM, err := readRegular(keyPath)
	if err != nil {
		return fmt.Errorf("the certificate's key could not be read back: %w", err)
	}
	// The API's own pair is put back to its mode and owner too, since one put there by a
	// person is theirs and the API refuses a key anybody else may read.
	if err := p.settle(keyPath, 0o600, true); err != nil {
		return err
	}
	if err := p.settle(certPath, 0o644, true); err != nil {
		return err
	}
	for _, c := range []struct {
		path    string
		data    []byte
		mode    fs.FileMode
		toAgent bool
	}{
		{p.dir.path(apiDir, "trust", "agentiik.pem"), certPEM, 0o644, true},
		{p.dir.path(controllerDir, "trust", "agentiik.pem"), certPEM, 0o644, true},
		{p.dir.path(runnerDir, "trust", "agentiik.pem"), certPEM, 0o644, false},
		// The bus runs as root and reads its own copies.
		{p.dir.path(natsDir, "server.pem"), certPEM, 0o644, false},
		{p.dir.path(natsDir, "server.key"), keyPEM, 0o600, false},
	} {
		if err := p.write(c.path, c.data, c.mode, c.toAgent); err != nil {
			return err
		}
	}
	// Printed at every run, since the certificate is no secret and a client on another machine
	// trusts it: docker compose logs init shows it whenever it is wanted.
	p.say("clients trust this certificate, which the bus serves, and the API where no proxy is in front:\n%s", strings.TrimSpace(string(certPEM)))
	return nil
}

// keeps is why the certificate at certPath is replaced, or nothing where it is kept.
func (p *preparer) keeps(certPath, keyPath, host string) (string, bool) {
	certPEM, err := readRegular(certPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "there was none", true
	}
	if err != nil {
		return "it could not be read", false
	}
	leaf := firstLeaf(certPEM)
	if leaf == nil {
		return "it holds no certificate", false
	}
	issued := issuedByInit(leaf)
	keyPEM, err := readRegular(keyPath)
	if err != nil {
		return "its key could not be read", issued
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return "its key is not the certificate's", issued
	}
	switch {
	case leaf.VerifyHostname(host) != nil:
		return "it was not issued to " + host, issued
	case !p.now.Before(leaf.NotAfter):
		return "it expired at " + leaf.NotAfter.UTC().Format(time.RFC3339), issued
	}
	return "", issued
}

// issuer is the organisation every certificate init issues names as its subject, which is how a
// later run tells one of its own from one a person put there.
const issuer = "agentiik-api init"

// firstLeaf is the first certificate a PEM file holds, or nil.
func firstLeaf(certPEM []byte) *x509.Certificate {
	for rest := certPEM; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil
			}
			return leaf
		}
	}
}

// issuedByInit says whether init issued c: its subject's organisation is issuer, or it is what
// setup issued before init existed, signed by itself, no authority, with a common name it is also
// issued to.
func issuedByInit(c *x509.Certificate) bool {
	if slices.Contains(c.Subject.Organization, issuer) {
		return true
	}
	if c.IsCA || c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) != nil ||
		!bytes.Equal(c.RawIssuer, c.RawSubject) || c.Subject.CommonName == "" {
		return false
	}
	return slices.Contains(c.DNSNames, c.Subject.CommonName) ||
		slices.ContainsFunc(c.IPAddresses, func(ip net.IP) bool { return ip.String() == c.Subject.CommonName })
}

// names are what the certificate is issued to: host, and this machine's own names, so that a
// program on the host reaches the API and the bus at localhost too.
func names(host string) []string {
	all := []string{host}
	for _, local := range []string{"localhost", "127.0.0.1"} {
		if local != host {
			all = append(all, local)
		}
	}
	return all
}

// selfSigned is a certificate for host, signed by its own key, an ECDSA P-256 one, as PEM, and
// that key, as PKCS #8 PEM, which is what openssl writes and every program here reads.
//
// Not an authority: clients trust this one certificate rather than something that could sign
// others, so a copy of agentiik.pem vouches for this installation and nothing else.
func selfSigned(host string, now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("the certificate's key could not be made: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("the certificate's serial number could not be drawn: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{issuer}},
		// An hour back, so that a runner whose clock is a little behind takes a certificate
		// made a moment ago.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certificateLife),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, name := range names(host) {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("the certificate could not be signed: %w", err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("the certificate's key could not be written out: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), nil
}

// secrets writes the master key, the presign key and the database password, each once, in the
// format the program reading it takes, and gives the controller its copy of the password. It
// answers the password, for migrating to create the role with.
func (p *preparer) secrets() (config.Secret, error) {
	password := p.dir.path(apiDir, "database-password")
	made := false
	for _, s := range []struct {
		path    string
		content func() ([]byte, error)
	}{
		// 256 bits in hexadecimal, as openssl rand -hex 32 writes them: nothing a URL has to
		// escape, since the role signs in with it through one.
		{password, func() ([]byte, error) { return random(32, hex.EncodeToString) }},
		// What config reads a presign key as, and artifact.NewSigned's least.
		{p.dir.path(apiDir, "presign-key"), func() ([]byte, error) { return random(32, base64.StdEncoding.EncodeToString) }},
		// What secret.ParseMaster reads: the identifier a person tells keys apart by, the month
		// it was made, and thirty-two bytes.
		{p.dir.path(apiDir, "master-key"), func() ([]byte, error) {
			key, err := random(32, base64.StdEncoding.EncodeToString)
			return append([]byte("id: "+p.now.UTC().Format("2006-01")+"\nkey: "), key...), err
		}},
	} {
		wrote, err := p.once(s.path, s.content)
		if err != nil {
			return "", err
		}
		made = made || wrote
	}
	content, err := readRegular(password)
	if err != nil {
		return "", fmt.Errorf("the database password could not be read back: %w", err)
	}
	if err := p.write(p.dir.path(controllerDir, "database-password"), content, 0o600, true); err != nil {
		return "", err
	}
	if made {
		p.say("wrote the keys and the database password that were missing, and kept the others")
	} else {
		p.say("kept the master key, the presign key and the database password")
	}
	return config.Secret(strings.TrimRight(string(content), "\r\n")), nil
}

// random is n random bytes, encoded, on one line.
func random(n int, encode func([]byte) string) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("random bytes could not be drawn: %w", err)
	}
	return []byte(encode(b) + "\n"), nil
}

// once writes what content makes at path where path holds nothing, and says whether it did. What
// is there is kept, and given back its mode and its owner.
func (p *preparer) once(path string, content func() ([]byte, error)) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil && !info.Mode().IsRegular():
		// A secret is never taken from a link or a pipe put in its place, which root would
		// follow to whatever it names and give on to the others.
		return false, fmt.Errorf("%s is not a regular file, and init keeps a secret there: remove it, and run init again", path)
	case err == nil && info.Size() > 0:
		return false, p.settle(path, 0o600, true)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("%s could not be looked at: %w", path, err)
	}
	data, err := content()
	if err != nil {
		return false, err
	}
	return true, p.write(path, data, 0o600, true)
}

// operatorTokenPrefix begins the operator token init mints, as setup did, so that a person tells it
// from the other credentials of the installation at a glance.
const operatorTokenPrefix = "agk_op_"

// operatorToken writes the hash of the operator token and never the token.
//
// What it says names no variable, since the one a person sets is not always AGK_OPERATOR_TOKEN: a
// Compose file hands it on from a variable of its own, and the installation's settings are
// wherever that file reads them.
//
// A token set is hashed at every run, and a hash that differs from it is replaced, so a token
// changed in .env, or set after one init minted, is the one the API takes at its next start. With
// none set, the hash stored is kept; where none was ever stored, init mints a token, prints it
// once, and keeps its hash. Printed before the hash is written, so that a run cut off between the
// two leaves a token nobody can use rather than a hash nobody has the token of.
func (p *preparer) operatorToken(token config.Secret) error {
	path := p.dir.path(apiDir, "operator-token.sha256")
	stored, err := readRegular(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the operator token's hash could not be read: %w", err)
	}
	stored = bytes.TrimSpace(stored)
	switch {
	case token != "":
		sum := sha256.Sum256([]byte(token))
		hash := hex.EncodeToString(sum[:])
		if string(stored) == hash {
			p.say("kept the hash of the operator token set")
			return p.settle(path, 0o600, true)
		}
		if err := p.write(path, []byte(hash+"\n"), 0o600, true); err != nil {
			return err
		}
		p.say("wrote the hash of the operator token set, which the API takes from its next start")
		return nil
	case len(stored) > 0:
		p.say("kept the hash of the operator token stored, since none is set")
		return p.settle(path, 0o600, true)
	}
	minted, err := random(24, hex.EncodeToString)
	if err != nil {
		return err
	}
	plain := operatorTokenPrefix + strings.TrimSpace(string(minted))
	p.say("minted an operator token, since none is set and none was stored. It is shown this once, and only its hash is kept. To keep it, set it as the operator token where the installation's settings are; to use a token of your own, set that there instead, and run init again.\n\n  %s\n", plain)
	sum := sha256.Sum256([]byte(plain))
	return p.write(path, []byte(hex.EncodeToString(sum[:])+"\n"), 0o600, true)
}

// bus writes the installation's bus identity once, as bus-init does, gives the control plane its
// credential where the API and the controller read it, renewing it where it is near its expiry, as
// the API does while it runs, and gives the bus its copy of the accounts, with its configuration.
func (p *preparer) bus() error {
	dir := p.dir.path(apiDir, "bus")
	accounts := filepath.Join(dir, bus.AccountsFile)
	switch {
	case p.busFiles(dir) == 0:
		if _, err := bus.NewInstallation(dir, p.now.Add(controlPlaneLife)); err != nil {
			return err
		}
		p.say("created the installation's bus identity")
	case !p.identity(dir):
		if err := p.repairBus(dir); err != nil {
			return err
		}
	default:
		p.say("kept the installation's bus identity")
	}
	if err := p.controlPlane(dir); err != nil {
		return err
	}
	for _, f := range []string{bus.AccountsFile, bus.AccountSeedFile} {
		if err := p.settle(filepath.Join(dir, f), 0o600, true); err != nil {
			return err
		}
	}
	content, err := readRegular(accounts)
	if err != nil {
		return fmt.Errorf("%s could not be read: %w", accounts, err)
	}
	if err := p.write(p.dir.path(natsDir, bus.AccountsFile), content, 0o600, false); err != nil {
		return err
	}
	return p.write(p.dir.path(natsDir, "nats.conf"), []byte(natsConf), 0o644, false)
}

// identity says whether dir holds the account and its seed, which is a whole identity once the
// control plane's credential lives in the bus directory rather than beside them.
func (p *preparer) identity(dir string) bool {
	for _, f := range []string{bus.AccountsFile, bus.AccountSeedFile} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

// controlPlane gives the control plane its credential in the bus directory, which the API and the
// controller both read and the API renews.
//
// One in the identity's directory is the newest there is, and is moved to the bus directory: it is
// where an installation prepared before that directory existed holds it, with a copy in the
// controller's, and where bus-init, bus-credential and an identity created again write theirs.
// Both are removed once the bus directory holds it, since a copy nothing renews any longer is a
// secret left on the disk for nothing, and one a person might name in a setting to find expired.
// Where there is none anywhere, one is minted under the account. One the bus directory holds is
// renewed from when the API would warn of it, as the API renews it, so that an installation whose
// API cannot renew it still has it renewed at the next docker compose up.
func (p *preparer) controlPlane(identity string) error {
	creds := p.dir.path(busDir, bus.ControlPlaneFile)
	legacy := filepath.Join(identity, bus.ControlPlaneFile)
	content, err := readRegular(legacy)
	switch {
	case err == nil:
		if err := p.write(creds, content, 0o600, true); err != nil {
			return err
		}
		p.say("moved the control plane's bus credential to %s, which the API and the controller share", filepath.Dir(creds))
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s could not be read: %w", legacy, err)
	default:
		_, err := os.Lstat(creds)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := p.mintControlPlane(identity, creds); err != nil {
				return err
			}
			p.say("minted the control plane's bus credential")
		case err != nil:
			return fmt.Errorf("%s could not be looked at: %w", creds, err)
		}
	}
	expires, err := controlPlaneExpiry(creds)
	if err != nil {
		return err
	}
	if !expires.IsZero() && expires.Sub(p.now) <= credentialWarning {
		if err := p.mintControlPlane(identity, creds); err != nil {
			return err
		}
		p.say("renewed the control plane's bus credential, which expired or was to expire at %s", expires.UTC().Format(time.RFC3339))
	}
	if err := p.settle(creds, 0o600, true); err != nil {
		return err
	}
	if err := os.Remove(legacy); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s, which the control plane's bus credential moved from, could not be removed: %w", legacy, err)
	}
	return p.removeControllerCopy()
}

// removeControllerCopy removes the copy of the control plane's credential an installation
// prepared before the bus directory existed gave the controller, and the directory that held it.
//
// Never through a link, since the controller may write to its own volume and init runs as root:
// a link in place of that directory is removed itself, and never followed to the file of the same
// name the bus directory holds. A directory holding anything else is left, which is not init's.
func (p *preparer) removeControllerCopy() error {
	dir := p.dir.path(controllerDir, "bus")
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%s could not be looked at: %w", dir, err)
	case info.IsDir():
		if err := os.Remove(filepath.Join(dir, bus.ControlPlaneFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s, which the control plane's bus credential moved from, could not be removed: %w", filepath.Join(dir, bus.ControlPlaneFile), err)
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("%s could not be removed: %w", dir, err)
	}
	return nil
}

// mintControlPlane writes at creds a new control plane credential under the account identity holds,
// valid controlPlaneLife.
func (p *preparer) mintControlPlane(identity, creds string) error {
	content, _, err := bus.MintControlPlane(identity, p.now.Add(controlPlaneLife))
	if err != nil {
		return err
	}
	return p.write(creds, content, 0o600, true)
}

// busFiles is how many of the three files of the bus identity dir holds, the control plane's
// credential as bus-init writes it among them.
func (p *preparer) busFiles(dir string) int {
	n := 0
	for _, f := range []string{bus.AccountsFile, bus.AccountSeedFile, bus.ControlPlaneFile} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			n++
		}
	}
	return n
}

// repairBus finishes a bus identity whose creation was cut off part way, by a crash or a power cut
// between two of its files, which bus.NewInstallation cannot undo, leaving the account or its seed
// without the other. A credential missing beside both is no repair: controlPlane mints one.
//
// Where the bus was never given the accounts, nothing ever trusted what is there, and it is created
// again. Where the bus was given them, the streams and the tasks on them are under that account,
// and a new one would lose them without a word, so init refuses and says what a person decides.
func (p *preparer) repairBus(dir string) error {
	if _, err := os.Lstat(p.dir.path(natsDir, bus.AccountsFile)); err == nil {
		return fmt.Errorf("%s holds part of the bus identity, and the bus was given its accounts already: an identity made again is one on which every stream and every task queued is gone, so it is a person's to decide: remove %s and %s to make it again", dir, dir, p.dir.path(natsDir, bus.AccountsFile))
	}
	for _, f := range []string{bus.AccountsFile, bus.AccountSeedFile, bus.ControlPlaneFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("what a creation cut off part way left in %s could not be removed: %w", dir, err)
		}
	}
	if _, err := bus.NewInstallation(dir, p.now.Add(controlPlaneLife)); err != nil {
		return err
	}
	p.say("created the installation's bus identity again, since a creation cut off part way had left part of it, which the bus was never given")
	return nil
}

// controlPlaneExpiry is when the control plane's credential in path expires, or the zero time for one
// that never does.
func controlPlaneExpiry(path string) (time.Time, error) {
	content, err := readRegular(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("the control plane's bus credential could not be read: %w", err)
	}
	token, err := jwt.ParseDecoratedJWT(content)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s holds no NATS user JWT: %w", path, err)
	}
	claims, err := jwt.DecodeUserClaims(token)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s holds no NATS user JWT: %w", path, err)
	}
	if claims.Expires == 0 {
		return time.Time{}, nil
	}
	return time.Unix(claims.Expires, 0), nil
}

// natsConf is the bus's configuration, as setup wrote it, at the paths the bus mounts the nats
// directory at.
const natsConf = `# Written by agentiik-api init. The bus listens on every interface, since a runner on another
# machine reaches it at AGK_INIT_HOST as the API and the controller do, and always over TLS, with a
# credential the API mints: it is never reached in plaintext. Its health check stays on the loopback.
listen: "0.0.0.0:4222"
http: "127.0.0.1:8222"
tls {
  cert_file: "/nats/server.pem"
  key_file: "/nats/server.key"
}
jetstream {
  store_dir: "/nats/jetstream"
}
include "accounts.conf"
`

// database migrates as the role that may change the schema, creates the namespace, and issues the
// runner beside the installation a join token of the pool default, written where it reads it.
//
// A join token at every run, rather than only before the runner first joins, because a runner that
// joined may have to join again after a setting changed, the API's address above all, and it
// decides that itself. Each token lives the hour a join token lives by default and is spent by
// the one join that uses it; the one it replaces in the file is left to expire, since a runner
// that read it a moment ago may be presenting it.
func (p *preparer) database(ctx context.Context, m config.Migration, name string) error {
	if err := migrate(ctx, m, p.out); err != nil {
		return err
	}
	if err := namespace(ctx, m.Application, "create", name, p.out); err != nil {
		return err
	}
	pool, err := db.Open(ctx, m.Application.ConnString())
	if err != nil {
		return fmt.Errorf("the database %s names could not be reached as %s: %w", config.DatabaseURL, m.Application.Role, err)
	}
	defer pool.Close()
	now := p.now.UTC()
	issued, _, err := api.IssueJoinToken(ctx, pool, defaultPool, nil, theOperator, now, now.Add(api.TokenDefaultLife))
	if err != nil {
		return fmt.Errorf("the runner's join token could not be issued: %w", err)
	}
	path := p.dir.path(runnerDir, "join-token")
	if err := p.write(path, []byte(issued.Clear+"\n"), 0o600, true); err != nil {
		return err
	}
	p.say("wrote a join token of the pool %s for the runner, which it uses where it has to join, until %s", defaultPool, issued.ExpiresAt.UTC().Format(time.RFC3339))
	return nil
}

// defaultPool is the pool every installation is migrated with, where a step naming no label goes.
const defaultPool = "default"

// write puts data at path in one step, written beside it and renamed over it, with mode, and owned
// by the agent's account where toAgent is true: a program starting at that moment reads the old
// file or the new one, never half of each.
func (p *preparer) write(path string, data []byte, mode fs.FileMode, toAgent bool) error {
	// Whatever is not a regular file there, a link or a pipe, is replaced by the rename rather
	// than followed or read.
	current, err := readRegular(path)
	if err == nil && bytes.Equal(current, data) {
		return p.settle(path, mode, toAgent)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("a file beside %s could not be created: %w", path, err)
	}
	temporary := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(temporary)
		return fmt.Errorf("%s could not be written: %w", path, err)
	}
	// The mode before the content, so that a secret is never readable by anybody else, even
	// for the moment between the two.
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if toAgent {
		if err := p.chown(f); err != nil {
			return fail(err)
		}
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("%s could not be written: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return fmt.Errorf("%s could not be written: %w", path, err)
	}
	return nil
}

// settle gives a file that exists its mode, and its owner where toAgent is true.
//
// Through a descriptor opened without following a link, so that root changes the file there and
// never one a link a service put there names.
func (p *preparer) settle(path string, mode fs.FileMode, toAgent bool) error {
	f, err := openRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return p.give(f, mode, toAgent)
}

// give gives an open file or directory its mode, and to the agent's account where toAgent is true.
func (p *preparer) give(f *os.File, mode fs.FileMode, toAgent bool) error {
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("%s could not be given mode %#o: %w", f.Name(), mode, err)
	}
	if toAgent {
		if err := p.chown(f); err != nil {
			return fmt.Errorf("%s could not be given to uid %d: %w", f.Name(), agent, err)
		}
	}
	return nil
}

// openRegular opens the regular file at path, never through a link and never waiting on a pipe,
// since a service may have put either in the volume it shares with init, which runs as root.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return f, nil
}

// readRegular is what the regular file at path holds, read as openRegular opens it. A path where
// there is nothing is fs.ErrNotExist.
func readRegular(path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
