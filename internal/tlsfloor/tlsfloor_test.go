package tlsfloor

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A server that speaks nothing newer than TLS 1.1 is refused, before a request reaches it. Go's
// default refuses one too, so this holds the behaviour rather than proving the floor, which
// TestEveryConfigurationHoldsTheFloor does.
func TestAServerOfTLS11IsRefused(t *testing.T) {
	reached := false
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}
	server.StartTLS()
	defer server.Close()

	client := &http.Client{Transport: trusting(server)}
	_, err := client.Get(server.URL)
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("a TLS 1.1 server was answered with %v, and it is under the floor", err)
	}
	if reached {
		t.Fatal("the request reached a TLS 1.1 server")
	}
}

// TLS 1.3 is what two ends that both speak it agree on, and 1.2 is still spoken to a server that
// goes no higher.
func TestTLS13IsSpokenWhereBothEndsAllowIt(t *testing.T) {
	for want, highest := range map[uint16]uint16{tls.VersionTLS13: tls.VersionTLS13, tls.VersionTLS12: tls.VersionTLS12} {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS.Version != want {
				w.WriteHeader(http.StatusTeapot)
			}
		}))
		server.TLS = &tls.Config{MaxVersion: highest}
		server.StartTLS()

		answer, err := (&http.Client{Transport: trusting(server)}).Get(server.URL)
		server.Close()
		if err != nil {
			t.Fatalf("a server speaking up to %s was not reached: %v", tls.VersionName(highest), err)
		}
		answer.Body.Close()
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("a server speaking up to %s was not spoken %s", tls.VersionName(highest), tls.VersionName(want))
		}
	}
}

// Every configuration the package hands out, or raises, holds the floor.
func TestEveryConfigurationHoldsTheFloor(t *testing.T) {
	if v := Config().MinVersion; v != tls.VersionTLS12 {
		t.Fatalf("Config's floor is %s", tls.VersionName(v))
	}
	if v := Transport().TLSClientConfig.MinVersion; v != tls.VersionTLS12 {
		t.Fatalf("Transport's floor is %s", tls.VersionName(v))
	}
	built := &tls.Config{MinVersion: tls.VersionTLS10}
	Floor(built)
	if built.MinVersion != tls.VersionTLS12 {
		t.Fatalf("Floor left a configuration at %s", tls.VersionName(built.MinVersion))
	}
	higher := &tls.Config{MinVersion: tls.VersionTLS13}
	Floor(higher)
	if higher.MinVersion != tls.VersionTLS13 {
		t.Fatalf("Floor lowered a configuration of TLS 1.3 to %s", tls.VersionName(higher.MinVersion))
	}
	Floor(nil)
}

// Loopback is this machine, by name or address, and nothing else.
func TestLoopbackIsThisMachineAlone(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LocalHost": true, "127.0.0.1": true, "127.8.9.10": true, "::1": true,
		"0.0.0.0": true, "::": true,
		"localhost.example.com": false, "10.0.0.1": false, "db": false, "": false, "169.254.0.1": false,
	} {
		if Loopback(host) != want {
			t.Errorf("Loopback(%q) is %v", host, !want)
		}
	}
}

// trusting is the package's transport, trusting the test server's certificate as well.
func trusting(server *httptest.Server) *http.Transport {
	t := Transport()
	t.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return t
}

// A request to this machine is never sent through the proxy the environment names, whatever
// spelling of this machine its URL uses, and one anywhere else still is.
func TestARequestToThisMachineGoesThroughNoProxy(t *testing.T) {
	if os.Getenv("TLSFLOOR_PROXY_CHILD") == "" {
		proxied := make(chan string, 8)
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxied <- r.URL.String()
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer proxy.Close()
		child := exec.Command(os.Args[0], "-test.run=^TestARequestToThisMachineGoesThroughNoProxy$", "-test.count=1")
		child.Env = append(os.Environ(), "TLSFLOOR_PROXY_CHILD=1", "HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
		out, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		close(proxied)
		elsewhere := false
		for u := range proxied {
			if strings.Contains(u, "agentiik.example") {
				elsewhere = true
			} else {
				t.Errorf("a request to this machine went through the proxy: %s", u)
			}
		}
		if !elsewhere {
			t.Error("a request to another machine did not go through the proxy")
		}
		return
	}
	transport := Transport()
	for _, host := range []string{"LOCALHOST", "0.0.0.0", "[::]", "127.0.0.1", "agentiik.example"} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+host+":9/objects?sig=s3cr3t", nil)
		u, err := transport.Proxy(req)
		if err != nil {
			t.Fatal(err)
		}
		if u != nil {
			// Reach the proxy, so the parent sees which request was sent to it.
			if answer, err := transport.RoundTrip(req); err == nil {
				answer.Body.Close()
			}
		}
	}
}
