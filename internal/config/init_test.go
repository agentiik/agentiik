package config_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/config"
)

// What agentiik-api init reads, the bootstrap token migrate reads as init does, and how the API
// reads a proxy in front of it: every setting from the environment alone, since a Compose file has
// no other place to put one.

func anInit(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		config.InitDir:            t.TempDir(),
		config.InitHost:           "agentiik.example.com",
		config.InitNamespace:      "demo",
		config.OperatorToken:      "agk_op_" + strings.Repeat("0a", 24),
		config.MigrateDatabaseURL: "postgres://postgres@/agentiik?host=/run/postgresql",
		config.DatabaseURL:        "postgres://agentiik@/agentiik?host=/run/postgresql",
	}
}

// Every setting init reads is read, the operator token as a value among them, which no other
// program takes.
func TestInitIsReadFromItsSettings(t *testing.T) {
	env := anInit(t)
	c, err := config.ReadInit(lookupIn(env, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir != env[config.InitDir] || c.Host != "agentiik.example.com" || c.Namespace != "demo" ||
		string(c.OperatorToken) != env[config.OperatorToken] || c.Admin.Role != "postgres" ||
		c.Application.Role != "agentiik" || c.Application.Password != "" {
		t.Errorf("init read %+v", c)
	}
	for _, host := range []string{"localhost", "192.0.2.10", "a-b.example.com", "EXAMPLE.com"} {
		env[config.InitHost] = host
		if c, err := config.ReadInit(lookupIn(env, nil)); err != nil || c.Host != host {
			t.Errorf("the host %s is read as %q: %v", host, c.Host, err)
		}
	}
	delete(env, config.OperatorToken)
	if c, err := config.ReadInit(lookupIn(env, nil)); err != nil || c.OperatorToken != "" {
		t.Errorf("with no token set, init read %q: %v", c.OperatorToken, err)
	}

	// No other program is given it but migrate, and none is told to put it in a file instead:
	// there is no file any program reads it from, since the API reads its hash from the database.
	for _, p := range []program{theAPI, theController} {
		i := anInstallation(t)
		token := "agk_op_" + strings.Repeat("0a", 24)
		i.env[config.OperatorToken] = token
		err := p.read(p.environment(i))
		if !slices.Equal(refused(err), []string{config.OperatorToken}) {
			t.Errorf("%s took the bootstrap token as a value: %v", p.name, err)
			continue
		}
		if strings.Contains(err.Error(), config.OperatorTokenFile) || strings.Contains(err.Error(), token) {
			t.Errorf("%s refuses the bootstrap token pointing at a file nothing reads, or repeating it: %v", p.name, err)
		}
	}
}

// migrate takes the bootstrap token as init does, held to the same rules, since an installation
// that runs no init, Homebrew's or one put together by hand, runs migrate in its place.
func TestMigrateTakesTheBootstrapTokenAsInitDoes(t *testing.T) {
	i := anInstallation(t)
	token := "agk_op_" + strings.Repeat("0a", 24)
	i.env[config.OperatorToken] = token
	c, err := config.ReadMigration(migrating.environment(i))
	if err != nil || string(c.OperatorToken) != token {
		t.Errorf("migrate read the bootstrap token as %q: %v", c.OperatorToken, err)
	}
	for what, value := range map[string]string{
		"a short token":        "agk_op_short",
		"a token with a space": "Zm9yIGEgdGVzdCwgYSB0b2tlbiB3aXRoIGEgc3BhY2UgaW4= it",
	} {
		i.env[config.OperatorToken] = value
		_, err := config.ReadMigration(migrating.environment(i))
		if !slices.Equal(refused(err), []string{config.OperatorToken}) {
			t.Errorf("%s: migrate's start was refused naming %v: %v", what, refused(err), err)
			continue
		}
		saysNothingOf(t, err, value)
	}
}

// migrate reads the path AGK_OPERATOR_TOKEN_FILE holds and nothing of the file at its start: the
// file is opened where there is a hash to import and never again, so one that is not there, or
// holds anything, or that anybody may read, starts it.
func TestMigrateOpensNoOperatorTokenFileAtItsStart(t *testing.T) {
	i := anInstallation(t)
	for what, path := range map[string]string{
		"not there":         filepath.Join(i.dir, "gone.sha256"),
		"holding anything":  chmod(t, i.write(t, "anything", []byte("not a hash")), 0o644),
		"a relative path":   "operator-token.sha256",
		"a directory":       i.dir,
		"the token instead": "agk_op_" + strings.Repeat("0a", 24),
	} {
		i.env[config.OperatorTokenFile] = path
		if c, err := config.ReadMigration(migrating.environment(i)); err != nil || c.OperatorTokenFile != path {
			t.Errorf("with the operator token's file %s, migrate read %q: %v", what, c.OperatorTokenFile, err)
		}
	}
}

// migrate reads the object store's directory where it is set, held to what the API holds it to,
// since it reads the envelopes of the runs v0.2 finished from there; and starts without it, since
// the migrations and the bootstrap token need no store.
func TestMigrateReadsTheObjectStoreWhereItIsSet(t *testing.T) {
	i := anInstallation(t)
	if c, err := config.ReadMigration(migrating.environment(i)); err != nil || c.Objects != i.env[config.ObjectsDir] {
		t.Errorf("migrate read the object store as %q: %v", c.Objects, err)
	}
	delete(i.env, config.ObjectsDir)
	if c, err := config.ReadMigration(migrating.environment(i)); err != nil || c.Objects != "" {
		t.Errorf("with no object store set, migrate read %q: %v", c.Objects, err)
	}
	i.env[config.ObjectsDir] = "objects"
	if _, err := config.ReadMigration(migrating.environment(i)); !slices.Equal(refused(err), []string{config.ObjectsDir}) {
		t.Errorf("with a relative object store, migrate's start was refused naming %v: %v", refused(err), err)
	}
}

// The v0.2 operator token's hash is read as v0.2's API read it: 64 lowercase hexadecimal
// characters, a newline after them or not, in a file its owner alone may read. A file that is not
// there is none, and is no refusal; any other is refused naming the variable, and the refusal
// repeats neither the path nor what the file holds.
func TestTheV02OperatorTokensHashIsReadAsV02WroteIt(t *testing.T) {
	i := &installation{dir: t.TempDir()}
	token := "agk_op_" + strings.Repeat("0a", 24)
	sum := sha256.Sum256([]byte(token))
	hashed := hex.EncodeToString(sum[:])
	for what, content := range map[string]string{
		"with a newline, as v0.2's init and agentiik-setup wrote it": hashed + "\n",
		"with none": hashed,
	} {
		path := i.write(t, strings.ReplaceAll(what, " ", "-"), []byte(content))
		if hash, err := (config.Migration{OperatorTokenFile: path}).OperatorTokenHash(); err != nil || !bytes.Equal(hash, sum[:]) {
			t.Errorf("a hash written %s reads as %x: %v", what, hash, err)
		}
	}
	for what, path := range map[string]string{
		"no file named":                "",
		"a file that is not there":     filepath.Join(i.dir, "gone.sha256"),
		"in a directory that is not":   filepath.Join(i.dir, "gone", "operator-token.sha256"),
		"a link to a file that is not": link(t, filepath.Join(i.dir, "gone.sha256"), filepath.Join(i.dir, "dangling")),
	} {
		if hash, err := (config.Migration{OperatorTokenFile: path}).OperatorTokenHash(); err != nil || hash != nil {
			t.Errorf("%s reads as %x: %v", what, hash, err)
		}
	}
	for what, path := range map[string]string{
		"readable by its group": chmod(t, i.write(t, "group", []byte(hashed+"\n")), 0o640),
		"readable by anybody":   chmod(t, i.write(t, "anybody", []byte(hashed+"\n")), 0o644),
		"holding the token":     i.write(t, "token", []byte(token+"\n")),
		"in uppercase":          i.write(t, "uppercase", []byte(strings.ToUpper(hashed)+"\n")),
		"one character short":   i.write(t, "short", []byte(hashed[:63]+"\n")),
		"holding two hashes":    i.write(t, "two", []byte(hashed+"\n"+hashed+"\n")),
		"empty":                 i.write(t, "empty", nil),
		"a relative path":       "operator-token.sha256",
		"a directory":           i.dir,
	} {
		_, err := (config.Migration{OperatorTokenFile: path}).OperatorTokenHash()
		if !slices.Equal(refused(err), []string{config.OperatorTokenFile}) {
			t.Errorf("a file %s was refused naming %v: %v", what, refused(err), err)
			continue
		}
		saysNothingOf(t, err, append(i.secrets, hashed, token, path)...)
	}
}

// link makes a symbolic link at name to target, and answers name.
func link(t *testing.T, target, name string) string {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
	return name
}

// A setting init needs, missing or malformed, refuses the start and names its variable, without
// repeating the operator token.
func TestInitRefusesASettingMissingOrMalformed(t *testing.T) {
	token := "Zm9yIGEgdGVzdCwgYSB0b2tlbiB3aXRoIGEgc3BhY2UgaW4= it"
	for what, c := range map[string]struct {
		variable, value string
	}{
		"no directory":                   {config.InitDir, ""},
		"a relative directory":           {config.InitDir, "data"},
		"no host":                        {config.InitHost, ""},
		"a host with a scheme":           {config.InitHost, "https://agentiik.example.com"},
		"a host with a port":             {config.InitHost, "agentiik.example.com:8443"},
		"an IPv6 host":                   {config.InitHost, "2001:db8::1"},
		"a host beginning with a hyphen": {config.InitHost, "-agentiik.example.com"},
		"a host with an empty label":     {config.InitHost, "agentiik..example.com"},
		"no namespace":                   {config.InitNamespace, ""},
		"a short token":                  {config.OperatorToken, "agk_op_short"},
		"a token with a space":           {config.OperatorToken, token},
		"a token of = alone":             {config.OperatorToken, strings.Repeat("=", 40)},
		"no migration role":              {config.MigrateDatabaseURL, ""},
		"no application role":            {config.DatabaseURL, ""},
		"a database in plaintext":        {config.DatabaseURL, "postgres://agentiik@db/agentiik"},
		"a password file for the role":   {config.DatabasePasswordFile, "/run/secrets/database-password"},
	} {
		env := anInit(t)
		if c.value == "" {
			delete(env, c.variable)
		} else {
			env[c.variable] = c.value
		}
		_, err := config.ReadInit(lookupIn(env, nil))
		if names := refused(err); !slices.Equal(names, []string{c.variable}) {
			t.Errorf("%s: the start was refused naming %v: %v", what, names, err)
		}
		saysNothingOf(t, err, token, "agk_op_short")
	}
}

// Init reads what it needs and nothing of what the API or the controller alone read.
func TestInitReadsOnlyWhatItNeeds(t *testing.T) {
	env := anInit(t)
	var asked []string
	if _, err := config.ReadInit(lookupIn(env, &asked)); err != nil {
		t.Fatal(err)
	}
	for _, name := range asked {
		if slices.Contains([]string{config.MasterKeyFile, config.PresignKeyFile, config.OperatorTokenFile, config.PublicURL, config.ProxyURL, config.BusURL, config.Listen}, name) {
			t.Errorf("init asked for %s", name)
		}
	}
}

// Behind a proxy, the API is reached at the proxy's URL and serves plain HTTP on the loopback, at
// the port AGK_LISTEN names, with the TLS pair and the public URL a Compose file sets for the other
// way left unread.
func TestTheAPIBehindAProxyServesPlainHTTPOnTheLoopback(t *testing.T) {
	i := anInstallation(t)
	i.env[config.ProxyURL] = "https://agentiik.example.com/"
	i.env[config.PublicURL] = "https://agentiik.example.com:8443"
	i.env[config.Listen] = ":8443"
	i.env[config.TLSCertFile] = "/agentiik/tls/nowhere.pem"
	i.env[config.TLSKeyFile] = "/agentiik/tls/nowhere.key"
	c, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://agentiik.example.com" || c.Listen != "127.0.0.1:8443" || c.TLS.Served() {
		t.Errorf("behind a proxy, the API is at %s, listens on %s, serves TLS %v", c.PublicURL, c.Listen, c.TLS.Served())
	}
	for listen, want := range map[string]string{"127.0.0.1:9000": "127.0.0.1:9000", "[::1]:9000": "[::1]:9000", "localhost:9000": "127.0.0.1:9000"} {
		i.env[config.Listen] = listen
		if c, err := config.ReadAPI(theAPI.environment(i)); err != nil || c.Listen != want {
			t.Errorf("behind a proxy, %s is listened on as %s: %v", listen, c.Listen, err)
		}
	}
	env := maps.Clone(i.env)
	delete(env, config.PublicURL)
	delete(env, config.Listen)
	if c, err := config.ReadAPI(lookupIn(env, nil)); err != nil || c.PublicURL != "https://agentiik.example.com" || c.Listen != "127.0.0.1:8080" {
		t.Errorf("behind a proxy with no public URL and no address, the API is at %s on %s: %v", c.PublicURL, c.Listen, err)
	}

	for what, c := range map[string]struct{ variable, value string }{
		"another host to listen on": {config.Listen, "0.0.0.0:8443"},
		"a proxy in plaintext":      {config.ProxyURL, "http://agentiik.example.com"},
		"a proxy with a user":       {config.ProxyURL, "https://admin:hunter5@agentiik.example.com"},
		"a proxy with a query":      {config.ProxyURL, "https://agentiik.example.com/?a"},
	} {
		env := maps.Clone(i.env)
		env[config.Listen] = ":8443"
		env[c.variable] = c.value
		_, err := config.ReadAPI(lookupIn(env, nil))
		if names := refused(err); !slices.Equal(names, []string{c.variable}) {
			t.Errorf("%s: the start was refused naming %v: %v", what, names, err)
		}
		saysNothingOf(t, err, "hunter5")
	}

	// With none, the TLS pair is read, and one that is not there refuses the start as before.
	delete(i.env, config.ProxyURL)
	if _, err := config.ReadAPI(theAPI.environment(i)); !slices.Contains(refused(err), config.TLSCertFile) {
		t.Errorf("with no proxy, a certificate that is not there was not refused: %v", err)
	}
}
