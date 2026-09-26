package config_test

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/internal/config"
	"github.com/agentiik/agentiik/internal/tlstest"
)

// The certificate the API and the controller's metrics are served with where they serve TLS
// themselves, rather than in plain HTTP to a terminator in front: both files or neither, the key's
// held to every rule a secret's file is.

// withCertificate gives the installation a certificate and its key, and answers the pair.
func withCertificate(t *testing.T, i *installation) tlstest.Pair {
	t.Helper()
	pair := tlstest.NewPair(t, time.Now().Add(24*time.Hour))
	i.env[config.TLSCertFile], i.env[config.TLSKeyFile] = pair.Write(t, i.dir)
	i.secrets = append(i.secrets, string(pair.Key))
	return pair
}

// Given a certificate, the API serves its listener and the controller its metrics with it, at TLS
// 1.2 or newer and over HTTP/1.1, as a terminator speaks it; and printing either configuration
// shows nothing of the key.
func TestACertificateIsReadByTheAPIAndTheController(t *testing.T) {
	i := anInstallation(t)
	pair := withCertificate(t, i)
	// Written for anybody to read, as certbot writes one: a certificate is sent to anybody who
	// connects, so its mode is not held to a secret's.
	chmod(t, i.env[config.TLSCertFile], 0o644)

	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.ReadController(theController.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	want := config.TLS{Certificate: string(pair.Certificate), Key: config.Secret(pair.Key)}
	for what, got := range map[string]config.TLS{"the API": api.TLS, "the metrics": controller.Metrics.TLS} {
		if got != want || !got.Served() {
			t.Fatalf("%s reads the certificate as %+v", what, got)
		}
		served, err := got.Server()
		if err != nil {
			t.Fatal(err)
		}
		if served.MinVersion != tls.VersionTLS12 || len(served.Certificates) != 1 || !slices.Equal(served.NextProtos, []string{"http/1.1"}) {
			t.Errorf("%s is served at a floor of %s, with %d certificates, over %v", what, tls.VersionName(served.MinVersion), len(served.Certificates), served.NextProtos)
		}
	}
	for _, c := range []any{api, controller, &api, controller.Metrics} {
		for _, verb := range verbs {
			printsNoSecret(t, fmt.Sprintf(verb, c), string(pair.Key))
		}
		marshalled, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		printsNoSecret(t, string(marshalled), string(pair.Key))
	}
}

// Given no certificate, nothing changes: each program serves plain HTTP, as it did before either
// variable existed.
func TestWithoutACertificateBothServePlainHTTP(t *testing.T) {
	i := anInstallation(t)
	api, err := config.ReadAPI(theAPI.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.ReadController(theController.environment(i))
	if err != nil {
		t.Fatal(err)
	}
	for what, got := range map[string]config.TLS{"the API": api.TLS, "the metrics": controller.Metrics.TLS} {
		served, err := got.Server()
		if got != (config.TLS{}) || got.Served() || served != nil || err != nil {
			t.Errorf("given no certificate, %s is served with %+v", what, served)
		}
	}
}

// A certificate that cannot be served refuses the start of every program that would serve it,
// naming the variable, and repeats neither the key nor the path of its file.
func TestACertificateThatCannotBeServedRefusesTheStart(t *testing.T) {
	both := []program{theAPI, theController}
	for name, c := range map[string]struct {
		fault func(t *testing.T, i *installation)
		want  string
	}{
		"a certificate without its key": {
			fault: func(_ *testing.T, i *installation) { delete(i.env, config.TLSKeyFile) },
			want:  config.TLSKeyFile,
		},
		"a key without its certificate": {
			fault: func(_ *testing.T, i *installation) { delete(i.env, config.TLSCertFile) },
			want:  config.TLSCertFile,
		},
		"a key its group can read": {
			fault: func(t *testing.T, i *installation) { chmod(t, i.env[config.TLSKeyFile], 0o640) },
			want:  config.TLSKeyFile,
		},
		"a key anybody can read": {
			fault: func(t *testing.T, i *installation) { chmod(t, i.env[config.TLSKeyFile], 0o644) },
			want:  config.TLSKeyFile,
		},
		"a key at a relative path": {
			fault: func(t *testing.T, i *installation) {
				t.Chdir(i.dir)
				i.env[config.TLSKeyFile] = filepath.Base(i.env[config.TLSKeyFile])
			},
			want: config.TLSKeyFile,
		},
		"a key that is not there": {
			fault: func(_ *testing.T, i *installation) { i.env[config.TLSKeyFile] += ".gone" },
			want:  config.TLSKeyFile,
		},
		"a certificate that is not there": {
			fault: func(_ *testing.T, i *installation) { i.env[config.TLSCertFile] += ".gone" },
			want:  config.TLSCertFile,
		},
		"a certificate at a relative path": {
			fault: func(t *testing.T, i *installation) {
				t.Chdir(i.dir)
				i.env[config.TLSCertFile] = filepath.Base(i.env[config.TLSCertFile])
			},
			want: config.TLSCertFile,
		},
		"a key for another certificate": {
			fault: func(t *testing.T, i *installation) {
				other := tlstest.NewPair(t, time.Now().Add(time.Hour))
				i.secrets = append(i.secrets, string(other.Key))
				i.env[config.TLSKeyFile] = i.write(t, "other.key", other.Key)
			},
			want: config.TLSKeyFile,
		},
		"the key where the certificate belongs": {
			fault: func(t *testing.T, i *installation) {
				key, err := os.ReadFile(i.env[config.TLSKeyFile])
				if err != nil {
					t.Fatal(err)
				}
				i.env[config.TLSCertFile] = i.write(t, "swapped.pem", key)
			},
			want: config.TLSCertFile,
		},
		"a certificate and its key in one file": {
			fault: func(t *testing.T, i *installation) {
				chain, err := os.ReadFile(i.env[config.TLSCertFile])
				if err != nil {
					t.Fatal(err)
				}
				key, err := os.ReadFile(i.env[config.TLSKeyFile])
				if err != nil {
					t.Fatal(err)
				}
				both := i.write(t, "combined.pem", append(key, chain...))
				i.env[config.TLSCertFile], i.env[config.TLSKeyFile] = both, both
			},
			want: config.TLSCertFile,
		},
		"a key after the certificate, in the certificate's file": {
			fault: func(t *testing.T, i *installation) {
				chain, err := os.ReadFile(i.env[config.TLSCertFile])
				if err != nil {
					t.Fatal(err)
				}
				key, err := os.ReadFile(i.env[config.TLSKeyFile])
				if err != nil {
					t.Fatal(err)
				}
				i.env[config.TLSCertFile] = chmod(t, i.write(t, "combined.pem", append(chain, key...)), 0o644)
			},
			want: config.TLSCertFile,
		},
		"a certificate not valid yet": {
			fault: func(t *testing.T, i *installation) {
				early := tlstest.NewPairFrom(t, time.Now().Add(time.Hour), time.Now().Add(48*time.Hour))
				i.secrets = append(i.secrets, string(early.Key))
				i.env[config.TLSCertFile], i.env[config.TLSKeyFile] = early.Write(t, t.TempDir())
			},
			want: config.TLSCertFile,
		},
		"a certificate that expired": {
			fault: func(t *testing.T, i *installation) {
				expired := tlstest.NewPair(t, time.Now().Add(-time.Minute))
				i.secrets = append(i.secrets, string(expired.Key))
				dir := t.TempDir()
				i.env[config.TLSCertFile], i.env[config.TLSKeyFile] = expired.Write(t, dir)
			},
			want: config.TLSCertFile,
		},
	} {
		for _, p := range both {
			t.Run(fmt.Sprintf("%s, for %s", name, p.name), func(t *testing.T) {
				i := anInstallation(t)
				withCertificate(t, i)
				c.fault(t, i)
				err := p.read(p.environment(i))
				if names := refused(err); !slices.Equal(names, []string{c.want}) {
					t.Fatalf("the start was refused naming %v: %v", names, err)
				}
				saysNothingOf(t, err, append(i.secrets, i.env[config.TLSCertFile], i.env[config.TLSKeyFile])...)
			})
		}
	}
}

// The key passed as a value is refused by every program, whether or not it serves anything, as
// every secret passed as a value is.
func TestAKeyPassedAsAValueIsRefused(t *testing.T) {
	for _, p := range everyProgram {
		t.Run(p.name, func(t *testing.T) {
			i := anInstallation(t)
			pair := tlstest.NewPair(t, time.Now().Add(time.Hour))
			i.env["AGK_TLS_KEY"] = string(pair.Key)
			err := p.read(p.environment(i))
			if names := refused(err); !slices.Equal(names, []string{"AGK_TLS_KEY"}) {
				t.Fatalf("the start was refused naming %v: %v", names, err)
			}
			saysNothingOf(t, err, string(pair.Key))
		})
	}
}

// A controller whose metrics are off has no listener, so a certificate given it serves nothing, and
// is refused as a metrics token is: the sign of an installation that meant to open the port and did
// not say where.
func TestACertificateForAControllerWithNoMetricsIsRefused(t *testing.T) {
	i := anInstallation(t)
	withCertificate(t, i)
	delete(i.env, config.MetricsListen)
	delete(i.env, config.MetricsTokenFile)
	_, err := config.ReadController(theController.environment(i))
	names := refused(err)
	if !slices.Equal(names, []string{config.TLSCertFile, config.TLSKeyFile}) {
		t.Fatalf("the start was refused naming %v: %v", names, err)
	}
	if !strings.Contains(err.Error(), config.MetricsListen) {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}

	// The API serves its listener whatever the controller's metrics are.
	if _, err := config.ReadAPI(theAPI.environment(i)); err != nil {
		t.Errorf("the API refused the same certificate: %v", err)
	}
}
