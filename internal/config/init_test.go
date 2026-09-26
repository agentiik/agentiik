package config_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/config"
)

// What agentiik-api init reads, and how the API reads a proxy in front of it: every setting from
// the environment alone, since a Compose file has no other place to put one.

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

	// The API is never given it.
	i := anInstallation(t)
	i.env[config.OperatorToken] = "agk_op_" + strings.Repeat("0a", 24)
	if _, err := config.ReadAPI(theAPI.environment(i)); !slices.Equal(refused(err), []string{config.OperatorToken}) {
		t.Errorf("the API took the operator token as a value: %v", err)
	}
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
	for listen, want := range map[string]string{"127.0.0.1:9000": "127.0.0.1:9000", "[::1]:9000": "[::1]:9000", "localhost:9000": "localhost:9000"} {
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
