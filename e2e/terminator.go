package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// authority is the installation's private certificate authority, and the one certificate it
// signs, for 127.0.0.1: the bus presents it, and so does the terminator in front of the API.
//
// A private authority rather than plaintext, because internal/config refuses a bus reached at
// nats:// and an API whose public URL is http, and the runners hold both to the same rule. Every
// program trusts it the way an installation with a private authority does: the API and the
// controller through SSL_CERT_FILE, and each runner through the authority mounted over the
// bundle its image carries at /etc/ssl/certs/ca-certificates.crt, where Go looks first on Linux.
type authority struct {
	pool *x509.CertPool
	leaf tls.Certificate
}

// certificates makes the authority and its certificate, and writes both where the bus and the
// runners read them: tls/ca.pem, tls/server.pem and tls/server.key.
func (in *Installation) certificates() authority {
	dir := in.mkdir(0o755, "tls")
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		in.t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "agentiik e2e " + in.id},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		in.t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		in.t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		in.t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		in.t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		in.t.Fatal(err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	for _, f := range []struct {
		name    string
		content []byte
		mode    os.FileMode
	}{
		// The authority is mounted read-only into each runner, whose agent runs as 65532.
		{"ca.pem", caPEM, 0o644},
		{"server.pem", leafPEM, 0o644},
		// Read by the bus's server, which runs as root in its container.
		{"server.key", keyPEM, 0o600},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.content, f.mode); err != nil {
			in.t.Fatal(err)
		}
	}
	in.held = append(in.held, heldValue{"the private key of the bus and of the API's terminator", strings.TrimSpace(string(keyPEM))})

	leaf, err := tls.X509KeyPair(leafPEM, keyPEM)
	if err != nil {
		in.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return authority{pool: pool, leaf: leaf}
}

// clientConfig trusts the authority and nothing else.
func (a authority) clientConfig() *tls.Config {
	return &tls.Config{RootCAs: a.pool, MinVersion: tls.VersionTLS12}
}

// terminate starts the TLS terminator in front of the API at upstream, and answers its URL, which
// is the installation's public URL.
//
// Every profile terminates TLS in front of the API, and the API listens in plain HTTP behind it,
// which is what internal/config.DefaultListen says. Here the terminator is the test's own, because
// that is where it can see every request a runner makes and hold what it sees to the closing
// fact: a runner reaches the API on the runner routes, and objects through presigned URLs and
// the signed upload policy alone.
func (in *Installation) terminate(ca authority, upstream string) string {
	target, err := url.Parse(upstream)
	if err != nil {
		in.t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
		},
		// A step's log is a stream, which the API flushes line by line and a buffer here
		// would hold back.
		FlushInterval: -1,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.leaf}, MinVersion: tls.VersionTLS12})
	if err != nil {
		in.t.Fatal(err)
	}
	server := &http.Server{
		Handler:           in.Requests.recording(proxy),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go server.Serve(ln)
	in.undo(func() { server.Close() })
	return "https://" + ln.Addr().String()
}

// Request is one request the terminator passed to the API: what was asked, who asked, and what
// the API answered.
type Request struct {
	Method string
	Path   string
	Query  url.Values

	// Caller is who the request was made as: the operator, somebody presenting another
	// bearer credential, or nobody.
	Caller Caller

	Status int
}

// Caller is who a request was made as, told from its Authorization header.
type Caller string

const (
	// CallerOperator presented the operator token, which is the test's own requests.
	CallerOperator Caller = "operator"
	// CallerBearer presented another bearer credential: a runner's, or a join token.
	CallerBearer Caller = "bearer"
	// CallerNobody presented none, which is what a presigned URL is followed with.
	CallerNobody Caller = "nobody"
	// CallerOther presented an Authorization header that is not a bearer credential.
	CallerOther Caller = "other"
)

// Requests is what the terminator has passed on, in the order the API answered.
type Requests struct {
	operator string

	mu  sync.Mutex
	all []Request
}

// All answers every request passed on so far.
func (r *Requests) All() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.all...)
}

// recording records each request next passes on, with the status it answered.
func (r *Requests) recording(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Recorded as it arrives, so that a request still being answered, or one whose answer
		// was cut and ended the handler with a panic, is held to the rule all the same. Its
		// status is filled in when the handler ends, however it ends, and a request never
		// answered keeps none, which the rule refuses.
		r.mu.Lock()
		r.all = append(r.all, Request{
			Method: req.Method, Path: req.URL.Path, Query: req.URL.Query(),
			Caller: callerOf(req.Header.Get("Authorization"), r.operator),
		})
		at := len(r.all) - 1
		r.mu.Unlock()
		status := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		returned := false
		defer func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			// A handler that returned having written nothing answered 200, as net/http
			// answers for it; one that panicked first answered nothing.
			if status.wrote || returned {
				r.all[at].Status = status.status
			}
		}()
		next.ServeHTTP(status, req)
		returned = true
	})
}

// callerOf tells who a request was made as from its Authorization header.
func callerOf(authorization, operator string) Caller {
	token, bearer := strings.CutPrefix(authorization, "Bearer ")
	switch {
	case authorization == "":
		return CallerNobody
	case !bearer:
		return CallerOther
	case token == operator:
		return CallerOperator
	}
	return CallerBearer
}

// statusWriter keeps the status a handler answered with. It passes Flush on, which a log stream
// needs.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	// An informational answer, such as 100 Continue, is followed by the one that counts.
	if !s.wrote && code >= 200 {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status, s.wrote = http.StatusOK, true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap is what http.ResponseController reaches the connection through.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// runnerRoutes are the routes a runner calls the API on, and who it calls each as: join presents
// its join token in the body and nothing in its header, and every other route the runner's
// credential. They are #registering-a-runner and the runner rows of #channels, and nothing else of
// /api/v1 is a runner's.
var runnerRoutes = map[string]Caller{
	"POST /api/v1/runners":           CallerNobody,
	"POST /api/v1/runners/heartbeat": CallerBearer,
	"POST /api/v1/runners/rotate":    CallerBearer,
	"POST /api/v1/tasks/redeem":      CallerBearer,
	"POST /api/v1/tasks/logs":        CallerBearer,
	"POST /api/v1/bus/token":         CallerBearer,
}

// Objects counts what a runner did with the object store: the objects it read and the ones it
// wrote.
type Objects struct {
	Reads, Writes int
}

// outsideTheOperator holds every request the operator did not make to what a runner may ask: a
// runner route, or an object through a presigned URL or a signed policy, carrying no credential.
// It answers each request that broke that rule, in a sentence, and what was done with objects.
func outsideTheOperator(requests []Request) ([]string, Objects) {
	var broken []string
	var objects Objects
	for _, r := range requests {
		if r.Caller == CallerOperator {
			continue
		}
		route := r.Method + " " + r.Path
		switch {
		case runnerRoutes[route] != "":
			if want := runnerRoutes[route]; r.Caller != want {
				broken = append(broken, fmt.Sprintf("%s was asked as %s, and a runner asks it as %s", route, r.Caller, want))
			}
		case strings.HasPrefix(r.Path, "/objects/"):
			switch {
			case r.Caller != CallerNobody:
				broken = append(broken, fmt.Sprintf("%s carried a credential, and an object is reached through a presigned URL, which is its own authorisation", route))
			case r.Status == 0:
				broken = append(broken, fmt.Sprintf("%s was never answered, so nothing says the API took its signature", route))
			case r.Status >= 300:
				broken = append(broken, fmt.Sprintf("%s was answered %d, and every presigned URL a runner follows is one the API signed", route, r.Status))
			case r.Method == http.MethodPost:
				// A form posted to its namespace, whose signed policy travels in the body.
				objects.Writes++
			case r.Query.Get("run") == "" || r.Query.Get("expires") == "" || r.Query.Get("signature") == "":
				broken = append(broken, fmt.Sprintf("%s carried no run, expires and signature, which is what a presigned URL is", route))
			case r.Method == http.MethodGet:
				objects.Reads++
			default:
				objects.Writes++
			}
		default:
			broken = append(broken, fmt.Sprintf("%s was asked as %s, and it is neither a runner route nor an object", route, r.Caller))
		}
	}
	return broken, objects
}
