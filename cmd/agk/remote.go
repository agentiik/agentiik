package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentiik/agentiik/internal/tlsfloor"
)

// An installation, as the command line reaches it: where it is, the credential every request
// carries, and what its refusals mean at a terminal.
//
// Every verb that talks to an installation goes through here, agk push included, so that the
// installation is named one way, the credential is read from one place and a refusal reads the
// same whichever verb met it.

// remote is one installation and the credential presented to it.
type remote struct {
	// base is the installation's address with no trailing slash, which every path is
	// appended to.
	base  string
	token string
}

// reach reads which installation a command talks to, --server where it was given and
// AGENTIIK_SERVER where it was not, and the credential, from AGENTIIK_TOKEN alone. It answers
// false once it has said what is missing, and the command leaves with exitUsage.
//
// An address that would carry the credential in plaintext is refused before anything is sent:
// "nothing reaches the API in plaintext", and the credential is a bearer token that opens
// whatever its holder may do, which is everything for the operator of a v0.2.0 installation.
// http is taken for a loopback address alone, as a runner takes it for AGK_API, since that is
// a test server or a tunnel on this machine and never a network.
func reach(e Env, server string) (remote, bool) {
	where := server
	if where == "" {
		where = e.getenv(serverVariable)
	}
	if where == "" {
		fmt.Fprintf(e.Err, "no installation to talk to: pass --server or set %s\n", serverVariable)
		return remote{}, false
	}
	if err := checkAddress(where); err != nil {
		fmt.Fprintf(e.Err, "%s\n", err)
		return remote{}, false
	}
	token := e.getenv(tokenVariable)
	if token == "" {
		fmt.Fprintf(e.Err, "no credential: set %s. It is not a flag, because an argument is in the shell history, in the process list and in whatever recorded the terminal\n", tokenVariable)
		return remote{}, false
	}
	return remote{base: strings.TrimRight(where, "/"), token: token}, true
}

// checkAddress refuses an installation address the credential cannot be sent to.
//
// The address is not repeated in the refusal where it carries a user, since that is where a
// password is written.
func checkAddress(where string) error {
	u, err := url.Parse(where)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		shown := fmt.Sprintf("%q", where)
		switch {
		case err == nil && u.User != nil:
			shown = fmt.Sprintf("%q", u.Redacted())
		case err != nil && strings.Contains(where, "@"):
			shown = "the address given"
		case err == nil && u.Host == "" && strings.Contains(where, "@"):
			shown = "the address given"
		}
		return fmt.Errorf("%s is not an installation's address: write it https://agentiik.example.com", shown)
	}
	if u.User != nil {
		return errors.New("the installation's address carries a user, and the credential is AGENTIIK_TOKEN's alone: write it without one")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s carries a query or a fragment, and every path is appended to the address: write it without either", where)
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return fmt.Errorf("%s is plain http, and the credential in %s would cross the network readable by anyone on the way: use https, or http to a loopback address", where, tokenVariable)
	}
	return nil
}

// loopback says whether a host is this machine, by name or by address.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// url is the address of one path of the installation.
func (r remote) url(path string) string { return r.base + path }

// request builds one request carrying the credential.
func (r remote) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.url(path), body)
	if err != nil {
		return nil, fmt.Errorf("%s could not be asked: %w", r.url(path), err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// answerTimeout bounds one request that is not a stream, so that an installation that accepted
// the connection and never answered is not waited on for ever.
const answerTimeout = time.Minute

// client is how every request to an installation is sent, with the timeout given, none for a
// stream, which lasts as long as its step.
//
// It follows no redirect. Go's client carries Authorization on to a redirect whose host is the
// same, whatever its scheme, so an https address answered with a redirect to http on the same
// host, which a proxy misreading X-Forwarded-Proto gives, would send the token across the network
// in clear, and a 307 would send a run's inputs and a push's tree after it. An installation's
// address is the one it is configured with, and a redirect is answered as the refusal it is.
func client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// transport is what every request to an installation goes through, held to the TLS floor.
var transport = tlsfloor.Transport()

// errUnreachable is an installation that did not answer at all, which is no outcome rather than
// a refusal: nothing it said can be read as a verdict.
var errUnreachable = errors.New("the installation could not be reached")

// refused is an answer that says no, with its status and the sentence the installation gave.
type refused struct {
	status int
	said   string
}

func (r *refused) Error() string { return r.said }

// getJSON reads one answer into out.
func (r remote) getJSON(ctx context.Context, path string, out any) error {
	req, err := r.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return r.do(req, http.StatusOK, out)
}

// do sends one request and decodes an answer of the status expected into out, where out is not
// nil. Any other status is a *refused, and no answer at all is errUnreachable.
func (r remote) do(req *http.Request, want int, out any) error {
	answer, err := client(answerTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("%w at %s: %v", errUnreachable, r.base, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != want {
		return refusedBy(answer)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(answer.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: its answer to %s %s could not be read: %v", errUnreachable, req.Method, req.URL.Path, err)
	}
	return nil
}

// refusedBy reads why an installation said no, which it writes as {"error": ...}, and its
// status line where it wrote nothing that reads.
func refusedBy(answer *http.Response) *refused {
	var said struct {
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(answer.Body, 64<<10)).Decode(&said)
	if said.Error == "" {
		said.Error = answer.Status
	}
	if answer.StatusCode >= 300 && answer.StatusCode < 400 {
		said.Error = fmt.Sprintf("the installation answered %s, a redirect, and a request carrying the credential follows none: give --server or %s the address the installation is served at", answer.Status, serverVariable)
	}
	return &refused{status: answer.StatusCode, said: said.Error}
}

// statusOf is the status of a refusal, and 0 for anything else.
func statusOf(err error) int {
	var r *refused
	if errors.As(err, &r) {
		return r.status
	}
	return 0
}

// passing says whether a failure may pass if asked again: no answer, or an installation that
// answered it could not answer now.
func passing(err error) bool {
	if errors.Is(err, errUnreachable) {
		return true
	}
	status := statusOf(err)
	return status >= 500 || status == http.StatusTooManyRequests
}

// aboutRun says what a refusal of a request about one run means, in the command line's words.
//
// A run the caller may not read answers what one that does not exist answers, which is the
// point, so the sentence says both rather than guessing.
func aboutRun(run string, err error) string {
	switch statusOf(err) {
	case http.StatusUnauthorized:
		return fmt.Sprintf("the installation did not accept the credential in %s", tokenVariable)
	case http.StatusNotFound:
		return fmt.Sprintf("no run %s, or not yours", run)
	}
	return err.Error()
}
