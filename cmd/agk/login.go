package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agentiik/agentiik/api"
)

// agk login and agk logout: a person's API token for an installation, signed in for in a browser
// and kept in the local profile (profile.go).
//
// "agk login listens on a port of the loopback, makes a one-time verifier and opens the browser at
// the sign-in page, handing it the loopback address and the verifier's SHA-256. The passkey
// ceremony runs in the browser, against the same Relying Party as every other sign-in. The page
// redirects to the loopback address with a one-time code: single use, good for a minute. agk sends
// the code and the verifier to POST /api/v1/auth/exchange and is answered an API token, which it
// stores in the local profile."
//
// The loopback address is http on 127.0.0.1, or [::1] where the machine has no IPv4 loopback, by
// number and never by the name localhost, as RFC 8252 advises and the sign-in page requires, on a
// port the system chooses. It answers one request to /callback, whatever that request carries, and
// nothing anywhere else: the first answer the browser brings is the one agk trades, and a second is
// told the page can be closed. A request a page of another site makes to the port, a fetch or a
// frame, is told apart by what the browser says of it, Sec-Fetch-Mode and Sec-Fetch-Dest, and not
// taken. A process of the machine that reached the port first could still hand agk a code of its
// own: one minted against another challenge is refused at the exchange, but the challenge is no
// secret, printed here and handed to the browser on its command line, so somebody else with an
// account could sign in with it as themselves and have agk keep their token. That takes somebody
// on the same machine, is what every loopback redirect is open to, and is why agk prints who it
// signed in as.
//
// The verifier is 32 bytes of the system's generator in base64url, 43 characters, and never leaves
// agk until the exchange. agk waits five minutes for the browser, the time a person takes to find
// an authenticator, and a sign-in that has not come back by then is signed in for again.
//
// agk logout revokes the token agk login kept for an installation, presenting that token, and takes
// it out of the profile. A token the installation no longer accepts is taken out all the same, and
// one whose revocation cannot be told, the installation not answering, is kept for agk logout to
// revoke once it does.

// loginWait is how long agk login waits for the browser to come back.
const loginWait = 5 * time.Minute

// callbackPath is the path of the loopback address the sign-in page sends the browser back to.
const callbackPath = "/callback"

// labelMost is how long a token's device label may be, in characters, as the installation holds it.
const labelMost = 256

// loginCode is openapi.json's exchangeCode, which the loopback address takes and nothing else.
var loginCode = regexp.MustCompile(`^agkcode_[A-Za-z0-9_-]{43,}$`)

// login is agk login [--server URL] [--label TEXT].
func login(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk login", "agk login [--server <url>] [--label <text>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	label := fs.String("label", "", "What the token is for, listed beside it so that the one on a lost machine can be revoked. Defaults to agk on this machine's name.")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) != 0 {
		fmt.Fprintf(e.Err, "agk login names nothing after it, and was given %q: the installation is --server\n", named[0])
		return exitUsage
	}
	device := *label
	if device == "" {
		device = defaultLabel()
	}
	if err := checkLabel(device); err != nil {
		fmt.Fprintf(e.Err, "--label: %s\n", err)
		return exitUsage
	}
	where, ok := installationOf(e, *server)
	if !ok {
		return exitUsage
	}
	base := strings.TrimRight(where, "/")
	// The profile is read, and its directory written in, before anything is asked of anybody, so
	// that one that cannot be kept is said before a person has signed in for nothing. A profile
	// agk cannot use is exit 2, as a credential missing from the environment is.
	path, kept, err := profilePath(e)
	if err == nil && !kept {
		err = errors.New("this agk keeps no local profile to store a token in: set " + tokenVariable + " to a token made with agk token create")
	}
	if err == nil {
		_, err = readProfile(path)
	}
	if err == nil {
		err = profileWritable(path)
	}
	if err != nil {
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitUsage
	}

	verifier, challenge, err := pkce()
	if err != nil {
		fmt.Fprintf(e.Err, "agk login could not make its verifier: %s\n", err)
		return exitNoOutcome
	}
	back, err := listenBack()
	if err != nil {
		fmt.Fprintf(e.Err, "agk login could not listen on this machine's loopback, where the browser hands it its code: %s\n", err)
		return exitNoOutcome
	}
	defer back.close()
	page := base + "/auth/sign-in?" + url.Values{"redirect_uri": {back.redirect}, "code_challenge": {challenge}}.Encode()
	tellWhere(e, base, page, back)

	got, err := back.await(ctx, loginWait)
	switch {
	case errors.Is(err, errNoCode):
		fmt.Fprintf(e.Err, "the browser came back to agk login with no code it can use, and nothing was signed in for: run agk login again\n")
		return exitRefused
	case errors.Is(err, errNoSignIn):
		fmt.Fprintf(e.Err, "no sign-in came back to agk login within %s, and nothing was signed in for: run agk login again\n", minutes(loginWait))
		return exitRefused
	case err != nil:
		fmt.Fprintf(e.Err, "agk login stopped before a sign-in came back to it, and nothing was signed in for\n")
		return exitRefused
	}

	issued, err := exchangeCode(ctx, base, got, verifier, device)
	// The code is spent by the exchange's first presentation, and a token may have been minted for
	// it before the answer was lost.
	const untold = "whether a token was minted cannot be told from it: run agk login again, and a token minted and never received expires on its own, or is revoked from agk token list"
	switch status := statusOf(err); {
	case err == nil:
	case errors.Is(err, errUnreachable):
		fmt.Fprintf(e.Err, "%s, and %s\n", err, untold)
		return exitNoOutcome
	case status >= 500:
		fmt.Fprintf(e.Err, "the installation answered %d, %s, and %s\n", status, err, untold)
		return exitNoOutcome
	default:
		fmt.Fprintf(e.Err, "the installation minted no token: %s\n", err)
		return exitRefused
	}

	// Read again, since another agk may have written it while this one waited on the browser.
	p, err := readProfile(path)
	if err == nil {
		key := installationKey(where)
		replaced := p.Installations[key]
		p.Installations[key] = storedToken{
			Token: issued.Token, ID: issued.APIToken.ID, Principal: issued.APIToken.Principal, ExpiresAt: issued.APIToken.ExpiresAt,
		}
		if err = writeProfile(path, p); err == nil {
			replace(ctx, e, base, replaced)
		}
	}
	if err != nil {
		return unkept(ctx, e, base, issued, err)
	}
	fmt.Fprintf(e.Out, "signed in to %s as %s: token %s, expiring at %s, is kept in %s\n",
		base, issued.APIToken.Principal, issued.APIToken.ID, issued.APIToken.ExpiresAt.UTC().Format(time.RFC3339), path)
	if e.getenv(tokenVariable) != "" {
		fmt.Fprintf(e.Err, "%s is set here, and agk presents it rather than the token kept: unset it for agk to present the one agk login kept\n", tokenVariable)
	}
	if set := e.getenv(serverVariable); set == "" || installationKey(set) != installationKey(where) {
		fmt.Fprintf(e.Err, "agk reaches %s with --server %s, or with %s=%s set\n", base, base, serverVariable, base)
	}
	return exitSucceeded
}

// replace revokes the token agk login kept for the installation before it kept another, presenting
// that token itself: nobody holds it any more, and a token left live that nobody holds is one more
// in the listing and nearer the bound on live tokens. One the installation no longer accepts has
// ended already; anything else is said, with how to revoke it by hand.
func replace(ctx context.Context, e Env, base string, before storedToken) {
	if before.Token == "" || before.ID == "" {
		return
	}
	old := remote{base: base, token: before.Token}
	err := old.tokenRequest(ctx, http.MethodDelete, "/api/v1/auth/tokens/"+url.PathEscape(before.ID), nil, http.StatusNoContent, nil)
	if status := statusOf(err); err != nil && status != http.StatusUnauthorized && status != http.StatusNotFound {
		fmt.Fprintf(e.Err, "the token agk login kept before, %s, could not be revoked: %s. agk token list shows it, and agk token revoke %s revokes it\n", before.ID, err, before.ID)
	}
}

// unkept says a token was minted that the profile could not keep, revokes it with itself, since its
// value is about to be lost, and leaves with the code for what is left: nothing, or a token that may
// still be live.
func unkept(ctx context.Context, e Env, base string, issued api.IssuedToken, why error) int {
	fmt.Fprintf(e.Err, "%s\n", why)
	lost := remote{base: base, token: issued.Token}
	if err := lost.tokenRequest(ctx, http.MethodDelete, "/api/v1/auth/tokens/"+url.PathEscape(issued.APIToken.ID), nil, http.StatusNoContent, nil); err != nil {
		fmt.Fprintf(e.Err, "token %s was minted and is not kept, and it could not be revoked: %s. agk token list shows it, and agk token revoke %s revokes it\n", issued.APIToken.ID, err, issued.APIToken.ID)
		return exitNoOutcome
	}
	fmt.Fprintf(e.Err, "token %s was minted, is not kept, and is revoked: nothing was signed in for\n", issued.APIToken.ID)
	return exitRefused
}

// logout is agk logout [--server URL].
func logout(ctx context.Context, e Env, args []string) int {
	fs := flags(e, "agk logout", "agk logout [--server <url>]")
	server := fs.String("server", "", "The installation. Defaults to "+serverVariable+".")
	named, code, ok := positional(fs, args)
	if !ok {
		return code
	}
	if len(named) != 0 {
		fmt.Fprintf(e.Err, "agk logout names nothing after it, and was given %q: the installation is --server\n", named[0])
		return exitUsage
	}
	where, ok := installationOf(e, *server)
	if !ok {
		return exitUsage
	}
	base := strings.TrimRight(where, "/")
	path, kept, err := profilePath(e)
	if err == nil && !kept {
		err = errors.New("this agk keeps no local profile, and agk login kept no token here")
	}
	var p profile
	if err == nil {
		p, err = readProfile(path)
	}
	if err != nil {
		fmt.Fprintf(e.Err, "%s\n", err)
		return exitUsage
	}
	key := installationKey(where)
	stored, found := p.Installations[key]
	if !found {
		// Signing out twice leaves the second with nothing to do, which is what was asked.
		fmt.Fprintf(e.Err, "agk login keeps no token for %s, and there is nothing to sign out of\n", base)
		return exitSucceeded
	}

	at := remote{base: base, token: stored.Token}
	err = at.tokenRequest(ctx, http.MethodDelete, "/api/v1/auth/tokens/"+url.PathEscape(stored.ID), nil, http.StatusNoContent, nil)
	revoked := "is revoked"
	switch status := statusOf(err); {
	case err == nil:
	case status == http.StatusUnauthorized || status == http.StatusNotFound:
		// Expired, revoked already, or its user suspended: it opens nothing, and nobody will
		// hold it once it is out of the profile.
		revoked = "opened nothing any more"
	case errors.Is(err, errUnreachable) || status >= 500:
		fmt.Fprintf(e.Err, "%s, and whether token %s was revoked cannot be told from it: it is kept in %s, for agk logout to revoke once the installation answers\n", err, stored.ID, path)
		return exitNoOutcome
	default:
		fmt.Fprintf(e.Err, "the installation did not revoke token %s: %s. It is kept in %s\n", stored.ID, err, path)
		return exitRefused
	}
	delete(p.Installations, key)
	if err := writeProfile(path, p); err != nil {
		// Revoked, and still kept: agk logout run again finds it opening nothing and forgets it.
		fmt.Fprintf(e.Err, "token %s %s, and %s: run agk logout again to forget it\n", stored.ID, revoked, err)
		return exitNoOutcome
	}
	fmt.Fprintf(e.Out, "signed out of %s: token %s %s, and is no longer kept in %s\n", base, stored.ID, revoked, path)
	if e.getenv(tokenVariable) != "" {
		fmt.Fprintf(e.Err, "%s is still set here, and agk goes on presenting it\n", tokenVariable)
	}
	return exitSucceeded
}

// pkce is a one-time verifier, 32 bytes of the system's generator in base64url, and its S256
// challenge, RFC 7636's: the SHA-256 of the verifier, in base64url with no padding.
func pkce() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// defaultLabel is the device label a token is minted with when --label names none: agk and this
// machine's name, which is what a person looks for in agk token list when the machine is lost.
func defaultLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" || checkLabel("agk on "+host) != nil {
		return "agk"
	}
	return "agk on " + host
}

// checkLabel refuses a label the installation would refuse, before anybody has signed in for it,
// and one that is not one line, since the listing prints it as one.
func checkLabel(label string) error {
	switch {
	case utf8.RuneCountInString(label) > labelMost:
		return fmt.Errorf("a label is at most %d characters, and this one is %d", labelMost, utf8.RuneCountInString(label))
	case !utf8.ValidString(label):
		return errors.New("a label is text, and this one is not UTF-8")
	case strings.ContainsFunc(label, unicode.IsControl):
		return errors.New("a label is one line, and this one holds a line break or another control character")
	}
	return nil
}

// minutes is a wait as a person reads it.
func minutes(d time.Duration) string {
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// tellWhere opens the sign-in page in the person's browser, where one can be opened from here, and
// says where it is in any case, with what to do where the browser is on another machine.
func tellWhere(e Env, base, page string, back *callback) {
	why := "this agk opens no browser"
	if e.Browse != nil {
		why = ""
		if err := e.Browse(page); err != nil {
			why = err.Error()
		}
	}
	if why == "" {
		fmt.Fprintf(e.Err, "Opening the sign-in page of %s in your browser. Sign in there, and agk login takes it from there. If no browser opened, open this address in one:\n\n  %s\n\n", base, page)
		fmt.Fprintf(e.Err, "agk login waits %s.\n", minutes(loginWait))
		return
	}
	fmt.Fprintf(e.Err, "Sign in to %s in a browser: %s, so open this address in one:\n\n  %s\n\n", base, why, page)
	fmt.Fprintf(e.Err, "The page then hands agk its code at %s, a port of this machine's loopback, which a browser on another machine reaches only through a forwarded port: from that machine, in a second terminal, run ssh -N -L %s:%s:%s followed by the name you reach this machine at, then open the address. Where no browser can reach this machine, mint a token elsewhere with agk token create and set %s here instead. agk login waits %s.\n",
		back.redirect, back.port, back.hostOnly, back.port, tokenVariable, minutes(loginWait))
}

// browse opens an address in the person's browser, with what the system opens addresses with,
// handing it nothing of the terminal, and waits for it in the background, since some stay until the
// browser they started exits. Where no browser of the person's can be opened from here, it says
// why and opens none: over SSH, this machine's browser is not the person's, and a Unix with no
// display would open one in the terminal, if any, where no passkey can be used.
func browse(address string) error {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return errors.New("agk runs over SSH, where it opens no browser of yours")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", address)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("this machine has no display to open a browser on")
		}
		cmd = exec.Command("xdg-open", address)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("no browser could be opened (%v)", err)
	}
	go cmd.Wait()
	return nil
}

// exchangeCode trades the code the browser brought back, with the verifier, for a token: POST
// /api/v1/auth/exchange, carrying no credential, which the code and the verifier stand for.
func exchangeCode(ctx context.Context, base, code, verifier, label string) (api.IssuedToken, error) {
	body, err := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "device_label": label})
	if err != nil {
		return api.IssuedToken{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/exchange", bytes.NewReader(body))
	if err != nil {
		return api.IssuedToken{}, fmt.Errorf("%s could not be asked: %w", base, err)
	}
	req.Header.Set("Content-Type", "application/json")
	var issued api.IssuedToken
	if err := (remote{base: base}).do(req, http.StatusCreated, &issued); err != nil {
		return api.IssuedToken{}, err
	}
	if issued.Token == "" || issued.APIToken.ID == "" {
		return api.IssuedToken{}, fmt.Errorf("%w: its answer to the exchange carries no token", errUnreachable)
	}
	return issued, nil
}

// The ends of a wait for the browser that are not a code.
var (
	errNoCode   = errors.New("the browser came back with no code")
	errNoSignIn = errors.New("no sign-in came back")
)

// callback is where agk login listens for the browser: one request to callbackPath, on the address
// the sign-in page is told.
type callback struct {
	// redirect is the address the sign-in page is told, http://HOST:PORT/callback; host the Host
	// header a browser sends it with, HOST:PORT; hostOnly and port its two halves.
	redirect, host, hostOnly, port string

	server *http.Server

	mu    sync.Mutex
	taken bool

	// codes carries the one request's code, or nothing where it carried none that reads.
	codes chan string
}

// listenBack listens on a port the system chooses of 127.0.0.1, or of [::1] where there is no IPv4
// loopback, and serves the one request the browser brings back.
func listenBack() (*callback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		var again error
		if ln, again = net.Listen("tcp", "[::1]:0"); again != nil {
			return nil, err
		}
	}
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		ln.Close()
		return nil, err
	}
	b := &callback{host: ln.Addr().String(), port: port, hostOnly: host, codes: make(chan string, 1)}
	if strings.Contains(host, ":") {
		b.hostOnly = "[" + host + "]"
	}
	b.redirect = "http://" + b.host + callbackPath
	// A browser's request is small and comes at once: whatever holds the connection open
	// without sending one is let go.
	b.server = &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	go b.server.Serve(ln)
	return b, nil
}

// close stops listening, once the answer to the browser's request has been written.
func (b *callback) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b.server.Shutdown(ctx)
}

// ServeHTTP answers the browser: 404 to any path but the callback's, to any host but the address
// the page was told, which a page of another site reaching the port through a name of its own
// would carry, and to what a browser says is no navigation of the whole window, a page's fetch or
// a frame, since the sign-in page sends the window itself; the callback taken once, whatever it
// carries; and a plain page saying what happened. A client that says nothing of either, as no
// browser is, is taken at its word.
func (b *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	mode, dest := r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest")
	if r.URL.Path != callbackPath || r.Host != b.host || (mode != "" && mode != "navigate") || (dest != "" && dest != "document") {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintln(w, "Nothing is here. agk login listens for the sign-in page's answer and nothing else.")
		return
	}
	if r.Method != http.MethodGet {
		h.Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintln(w, "The sign-in page's answer is a GET, and this is not one.")
		return
	}
	b.mu.Lock()
	taken := b.taken
	b.taken = true
	b.mu.Unlock()
	if taken {
		w.WriteHeader(http.StatusGone)
		fmt.Fprintln(w, "agk login has had its answer already. This page can be closed.")
		return
	}
	code, ok := codeIn(r.URL.RawQuery)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "This address carries no code agk login can use, and agk login has stopped. Run it again in the terminal. This page can be closed.")
		b.codes <- ""
		return
	}
	fmt.Fprintln(w, "Signed in. agk login has its code and finishes in the terminal, which says whether it did. This page can be closed.")
	b.codes <- code
}

// codeIn is the code a query carries, as the installation writes it: the one member code, once, on
// its grammar.
func codeIn(query string) (string, bool) {
	q, err := url.ParseQuery(query)
	if err != nil || len(q) != 1 || len(q["code"]) != 1 || !loginCode.MatchString(q["code"][0]) {
		return "", false
	}
	return q["code"][0], true
}

// await waits for the browser's request, at most wait, and answers its code: errNoCode where it
// carried none that reads, errNoSignIn where none came, and the context's error where it ended
// first.
func (b *callback) await(ctx context.Context, wait time.Duration) (string, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case code := <-b.codes:
		if code == "" {
			return "", errNoCode
		}
		return code, nil
	case <-timer.C:
		return "", errNoSignIn
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
