package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
)

// agk login and agk logout against a stand-in installation that answers the exchange and the token
// routes as the API does, with a stand-in browser that goes back to the loopback address as the
// sign-in page does once somebody has signed in; the loopback address itself; and the local profile.

// signInStandIn is an installation answering POST /api/v1/auth/exchange, GET /api/v1/auth/tokens and
// DELETE /api/v1/auth/tokens/{id} as the API does, for alice, and recording what it was asked.
type signInStandIn struct {
	*httptest.Server

	mu sync.Mutex
	// challenges are the challenges each code the stand-in minted was minted against.
	challenges map[string]string
	// tokens are the live tokens by value, each with its identifier.
	tokens map[string]string
	// exchanged are the bodies the exchange read, and presented the credentials each request
	// carried, "" for none.
	exchanged []map[string]string
	presented []string
	revoked   []string
	// answer, where it is not zero, is what the exchange answers in place of a token.
	answer int
}

func anInstallationSigningIn(t *testing.T) *signInStandIn {
	t.Helper()
	in := &signInStandIn{challenges: map[string]string{}, tokens: map[string]string{}}
	in.Server = httptest.NewServer(http.HandlerFunc(in.serve))
	t.Cleanup(in.Close)
	return in
}

func (in *signInStandIn) serve(w http.ResponseWriter, r *http.Request) {
	in.mu.Lock()
	defer in.mu.Unlock()
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	in.presented = append(in.presented, bearer)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v1/auth/exchange":
		var asked map[string]string
		json.NewDecoder(r.Body).Decode(&asked)
		in.exchanged = append(in.exchanged, asked)
		if in.answer != 0 {
			w.WriteHeader(in.answer)
			fmt.Fprintf(w, `{"error":"answered %d"}`, in.answer)
			return
		}
		sum := sha256.Sum256([]byte(asked["code_verifier"]))
		challenge, minted := in.challenges[asked["code"]]
		delete(in.challenges, asked["code"])
		if !minted || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"that code opens nothing"}`)
			return
		}
		value, id := "agktoken_"+secret(), fmt.Sprintf("01TOKEN%019d", len(in.tokens)+1)
		in.tokens[value] = id
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.IssuedToken{Token: value, APIToken: api.APIToken{
			ID: id, Principal: "alice", DeviceLabel: asked["device_label"],
			CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().AddDate(0, 0, 90).Truncate(time.Second),
		}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/auth/tokens":
		if _, live := in.tokens[bearer]; !live {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"that token opens nothing"}`)
			return
		}
		fmt.Fprint(w, `{"tokens":[]}`)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v1/auth/tokens/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/auth/tokens/")
		if in.tokens[bearer] != id {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"that token opens nothing"}`)
			return
		}
		delete(in.tokens, bearer)
		in.revoked = append(in.revoked, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such thing"}`)
	}
}

// signedIn plays the sign-in page once somebody has signed in on it: it mints a code against the
// challenge agk opened the page with, and sends the browser to the loopback address with it,
// answering what that address answered.
func (in *signInStandIn) signedIn(t *testing.T, page string) (int, string) {
	t.Helper()
	opened, err := url.Parse(page)
	if err != nil {
		t.Fatal(err)
	}
	q := opened.Query()
	if opened.Path != "/auth/sign-in" || len(q) != 2 {
		t.Errorf("agk login opened %s", page)
	}
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/callback$`).MatchString(q.Get("redirect_uri")) || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(q.Get("code_challenge")) {
		t.Errorf("agk login opened the sign-in page with %s", opened.RawQuery)
	}
	code := "agkcode_" + secret()
	in.mu.Lock()
	in.challenges[code] = q.Get("code_challenge")
	in.mu.Unlock()
	return get(t, q.Get("redirect_uri")+"?code="+code)
}

// get asks an address as a browser does, and answers the status and the body.
func get(t *testing.T, address string) (int, string) {
	t.Helper()
	answer, err := http.Get(address)
	if err != nil {
		t.Fatalf("the browser could not reach %s: %s", address, err)
	}
	defer answer.Body.Close()
	body, _ := io.ReadAll(answer.Body)
	return answer.StatusCode, string(body)
}

func secret() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// person is somebody at a terminal: their configuration directory, their environment, and what the
// browser agk opens does, which browse is where it is not nil.
type person struct {
	config string
	env    map[string]string
	browse func(string) error
}

func somebody(t *testing.T) *person {
	return &person{config: t.TempDir(), env: map[string]string{}}
}

// agk runs one command line as them, and answers its exit code and what it printed.
func (p *person) agk(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	out, errs := &strings.Builder{}, &strings.Builder{}
	code := run(t.Context(), Env{
		Out: out, Err: errs, Dir: t.TempDir(),
		Getenv:    func(k string) string { return p.env[k] },
		ConfigDir: func() (string, error) { return p.config, nil },
		Browse:    p.browse,
	}, args)
	return code, out.String(), errs.String()
}

// kept is the profile as agk left it.
func (p *person) kept(t *testing.T) profile {
	t.Helper()
	got, err := readProfile(filepath.Join(p.config, profileDir, profileFile))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// signingIn is a person whose browser signs in on in's page whenever agk opens it, keeping what the
// loopback address answered it.
func signingIn(t *testing.T, in *signInStandIn) (*person, *[]string) {
	p := somebody(t)
	var pages []string
	p.browse = func(page string) error {
		status, body := in.signedIn(t, page)
		pages = append(pages, fmt.Sprintf("%d %s", status, body))
		return nil
	}
	return p, &pages
}

// agk login opens the sign-in page with its loopback address and challenge, takes the code the
// browser brings back, trades it with its verifier for a token, labelled as asked, presenting no
// credential, and keeps it in the profile, 0600 in a directory 0700, by the installation's address;
// the browser is told it can close the page. From then on agk presents that token to that
// installation wherever AGENTIIK_TOKEN is not set, AGENTIIK_TOKEN winning where it is, and to no
// other installation.
func TestAgkLoginKeepsTheTokenTheBrowsersCodeIsTradedFor(t *testing.T) {
	in := anInstallationSigningIn(t)
	p, pages := signingIn(t, in)
	code, out, errs := p.agk(t, "login", "--server", in.URL+"/", "--label", "alice-laptop")
	if code != exitSucceeded {
		t.Fatalf("agk login left with %d: %s", code, errs)
	}
	if len(*pages) != 1 || !strings.HasPrefix((*pages)[0], "200 Signed in.") || !strings.Contains((*pages)[0], "This page can be closed.") {
		t.Errorf("the loopback address answered the browser %q", *pages)
	}
	if len(in.exchanged) != 1 || in.exchanged[0]["device_label"] != "alice-laptop" || in.presented[0] != "" {
		t.Errorf("the exchange was asked %v, presenting %q", in.exchanged, in.presented)
	}
	if !strings.Contains(errs, in.URL+"/auth/sign-in?code_challenge=") {
		t.Errorf("agk login does not print the page it opened: %s", errs)
	}
	stored := p.kept(t).Installations[in.URL]
	if !strings.HasPrefix(stored.Token, "agktoken_") || stored.ID != "01TOKEN0000000000000000001" || stored.Principal != "alice" {
		t.Fatalf("the profile keeps %+v for %s", p.kept(t), in.URL)
	}
	path := filepath.Join(p.config, profileDir, profileFile)
	if want := fmt.Sprintf("signed in to %s as alice: token %s, expiring at %s, is kept in %s\n", in.URL, stored.ID, stored.ExpiresAt.UTC().Format(time.RFC3339), path); out != want {
		t.Errorf("agk login printed %q, want %q", out, want)
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700 | os.ModeDir} {
			if info, err := os.Stat(name); err != nil || info.Mode() != want {
				t.Errorf("%s is %v, want %v: %v", name, info.Mode(), want, err)
			}
		}
	}

	presented := func(env map[string]string) (int, string) {
		t.Helper()
		p.env = env
		in.mu.Lock()
		in.presented = nil
		in.mu.Unlock()
		code, _, errs := p.agk(t, "token", "list")
		if len(in.presented) == 0 {
			return code, errs
		}
		return code, in.presented[0]
	}
	if code, got := presented(map[string]string{serverVariable: in.URL}); code != exitSucceeded || got != stored.Token {
		t.Errorf("with no AGENTIIK_TOKEN agk left with %d presenting %q", code, got)
	}
	if code, got := presented(map[string]string{serverVariable: strings.ToUpper(in.URL[:4]) + in.URL[4:] + "/"}); code != exitSucceeded || got != stored.Token {
		t.Errorf("with the address written otherwise agk left with %d presenting %q", code, got)
	}
	if _, got := presented(map[string]string{serverVariable: in.URL, tokenVariable: "agktoken_script"}); got != "agktoken_script" {
		t.Errorf("with AGENTIIK_TOKEN set agk presented %q", got)
	}
	if code, said := presented(map[string]string{serverVariable: "https://another.example.com"}); code != exitUsage || !strings.Contains(said, "no credential: sign in with agk login") {
		t.Errorf("another installation was answered %d: %s", code, said)
	}
}

// Signing in again to the same installation keeps the new token in place of the one kept, and
// revokes that one, presenting it, since nobody holds it any more; another installation's is left
// as it was.
func TestSigningInAgainReplacesTheTokenKeptAndRevokesIt(t *testing.T) {
	in, other := anInstallationSigningIn(t), anInstallationSigningIn(t)
	p, _ := signingIn(t, in)
	for _, where := range []string{in.URL, in.URL} {
		if code, _, errs := p.agk(t, "login", "--server", where); code != exitSucceeded {
			t.Fatalf("agk login left with %d: %s", code, errs)
		}
	}
	p.browse = func(page string) error { other.signedIn(t, page); return nil }
	if code, _, errs := p.agk(t, "login", "--server", other.URL); code != exitSucceeded {
		t.Fatalf("agk login left with %d: %s", code, errs)
	}
	kept := p.kept(t).Installations
	if len(kept) != 2 || kept[in.URL].ID != "01TOKEN0000000000000000002" || kept[other.URL].ID != "01TOKEN0000000000000000001" {
		t.Errorf("the profile keeps %+v", kept)
	}
	if len(in.revoked) != 1 || in.revoked[0] != "01TOKEN0000000000000000001" || len(other.revoked) != 0 {
		t.Errorf("signing in again revoked %v and %v", in.revoked, other.revoked)
	}
}

// Where no browser opens, agk login says why and prints the page for the person to open, with what
// to do where their browser is on another machine: forward the loopback port it listens on, or
// take a token made elsewhere in AGENTIIK_TOKEN. It goes on waiting all the same.
func TestAgkLoginSaysWhatToDoWhereNoBrowserOpens(t *testing.T) {
	in := anInstallationSigningIn(t)
	p := somebody(t)
	p.browse = func(page string) error {
		// The person opens the page themselves, and the loopback address holds their answer
		// until agk asks for it.
		in.signedIn(t, page)
		return errors.New("agk runs over SSH, where it opens no browser of yours")
	}
	code, _, errs := p.agk(t, "login", "--server", in.URL)
	if code != exitSucceeded {
		t.Fatalf("agk login left with %d: %s", code, errs)
	}
	port := regexp.MustCompile(`http://127\.0\.0\.1:([0-9]+)/callback`).FindStringSubmatch(errs)
	if port == nil || !strings.Contains(errs, "agk runs over SSH, where it opens no browser of yours, so open this address in one") ||
		!strings.Contains(errs, fmt.Sprintf("ssh -N -L %s:127.0.0.1:%s", port[1], port[1])) || !strings.Contains(errs, "set AGENTIIK_TOKEN here instead") {
		t.Errorf("agk login said:\n%s", errs)
	}
}

// A refused exchange keeps nothing and is exit 1, with the installation's sentence; one answered with
// a 5xx is exit 4, since whether a token was minted cannot be told; and a browser that comes back
// with no code agk can use ends agk login with nothing asked of the installation.
func TestAgkLoginKeepsNothingTheExchangeDidNotMint(t *testing.T) {
	for status, want := range map[int]int{http.StatusUnauthorized: exitRefused, http.StatusForbidden: exitRefused, http.StatusBadGateway: exitNoOutcome} {
		in := anInstallationSigningIn(t)
		in.answer = status
		p, _ := signingIn(t, in)
		code, _, errs := p.agk(t, "login", "--server", in.URL)
		if code != want || !strings.Contains(errs, fmt.Sprintf("answered %d", status)) {
			t.Errorf("an exchange answered %d left with %d: %s", status, code, errs)
		}
		if len(p.kept(t).Installations) != 0 {
			t.Errorf("an exchange answered %d kept %+v", status, p.kept(t))
		}
	}

	in := anInstallationSigningIn(t)
	p := somebody(t)
	p.browse = func(page string) error {
		opened, _ := url.Parse(page)
		get(t, opened.Query().Get("redirect_uri")+"?code=agkcode_short")
		return nil
	}
	if code, _, errs := p.agk(t, "login", "--server", in.URL); code != exitRefused || !strings.Contains(errs, "no code it can use") || len(in.exchanged) != 0 {
		t.Errorf("a browser back with no code left agk login with %d: %s", code, errs)
	}
}

// agk logout revokes the token kept, presenting it, and forgets it; one the installation no longer
// accepts is forgotten all the same; one whose revocation cannot be told, the installation not
// answering, is kept for agk logout to revoke later, exit 4; and signing out where nothing is kept
// has nothing to do.
func TestAgkLogoutRevokesTheTokenKeptAndForgetsIt(t *testing.T) {
	in := anInstallationSigningIn(t)
	p, _ := signingIn(t, in)
	p.env[serverVariable] = in.URL
	login := func() storedToken {
		t.Helper()
		if code, _, errs := p.agk(t, "login"); code != exitSucceeded {
			t.Fatalf("agk login left with %d: %s", code, errs)
		}
		return p.kept(t).Installations[in.URL]
	}

	stored := login()
	code, out, errs := p.agk(t, "logout")
	if code != exitSucceeded || out != fmt.Sprintf("signed out of %s: token %s is revoked, and is no longer kept in %s\n", in.URL, stored.ID, filepath.Join(p.config, profileDir, profileFile)) {
		t.Errorf("agk logout left with %d, printing %q: %s", code, out, errs)
	}
	if len(in.revoked) != 1 || in.revoked[0] != stored.ID || len(p.kept(t).Installations) != 0 {
		t.Errorf("agk logout revoked %v and kept %+v", in.revoked, p.kept(t))
	}
	if code, _, errs := p.agk(t, "logout"); code != exitSucceeded || !strings.Contains(errs, "nothing to sign out of") {
		t.Errorf("agk logout with nothing kept left with %d: %s", code, errs)
	}

	stored = login()
	in.mu.Lock()
	delete(in.tokens, stored.Token)
	in.mu.Unlock()
	if code, out, errs := p.agk(t, "logout"); code != exitSucceeded || !strings.Contains(out, "opened nothing any more") || len(p.kept(t).Installations) != 0 {
		t.Errorf("agk logout of a token that opens nothing left with %d, printing %q: %s", code, out, errs)
	}

	stored = login()
	in.Close()
	if code, _, errs := p.agk(t, "logout"); code != exitNoOutcome || !strings.Contains(errs, "for agk logout to revoke once the installation answers") || p.kept(t).Installations[in.URL] != stored {
		t.Errorf("agk logout with the installation gone left with %d: %s", code, errs)
	}
}

// A token kept past its expiry is not sent: agk says it expired and to sign in again.
func TestATokenKeptPastItsExpiryIsNotSent(t *testing.T) {
	in := anInstallationSigningIn(t)
	p := somebody(t)
	p.env[serverVariable] = in.URL
	path := filepath.Join(p.config, profileDir, profileFile)
	if err := writeProfile(path, profile{Installations: map[string]storedToken{
		in.URL: {Token: "agktoken_old", ID: "01OLD", Principal: "alice", ExpiresAt: time.Now().Add(-time.Minute)},
	}}); err != nil {
		t.Fatal(err)
	}
	code, _, errs := p.agk(t, "token", "list")
	if code != exitUsage || !strings.Contains(errs, "the token agk login stored for "+in.URL+" expired at") || len(in.presented) != 0 {
		t.Errorf("an expired token kept left agk with %d, presenting %v: %s", code, in.presented, errs)
	}
}

// The profile is its owner's alone: written 0600 in a directory 0700 whatever the umask, a
// directory that was more open brought to 0700; read only while nobody else may read or write it,
// and only as a file; and one that does not read, or that is not there, says so or is empty.
func TestTheProfileIsItsOwnersAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no Unix modes")
	}
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), profileDir)
	path := filepath.Join(dir, profileFile)
	if p, err := readProfile(path); err != nil || len(p.Installations) != 0 {
		t.Errorf("a profile not there read as %+v, %v", p, err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProfile(path, profile{Installations: map[string]storedToken{"https://a.example": {Token: "agktoken_a"}}}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{path: 0o600, dir: 0o700 | os.ModeDir} {
		if info, err := os.Stat(name); err != nil || info.Mode() != want {
			t.Errorf("%s is %v, want %v: %v", name, info.Mode(), want, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("writing the profile left %d files beside it", len(entries)-1)
	}
	if p, err := readProfile(path); err != nil || p.Installations["https://a.example"].Token != "agktoken_a" {
		t.Errorf("the profile read back as %+v, %v", p, err)
	}

	for mode, loose := range map[os.FileMode]string{0o640: "0640", 0o604: "0604", 0o620: "0620"} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := readProfile(path); err == nil || !strings.Contains(err.Error(), "(mode "+loose+"): chmod 600 it") {
			t.Errorf("a profile of mode %v read: %v", mode, err)
		}
	}
	os.Chmod(path, 0o600)

	linked := filepath.Join(t.TempDir(), profileFile)
	if err := os.Symlink(path, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfile(linked); err == nil || !strings.Contains(err.Error(), "is not a file") {
		t.Errorf("a profile that is a link read: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfile(path); err == nil || !strings.Contains(err.Error(), "does not read as agk writes it") {
		t.Errorf("a profile that does not read read: %v", err)
	}
}

// An installation is one entry of the profile however its address is written: its scheme and host
// in any case, with or without the slash it may end with.
func TestAnInstallationIsOneEntryHoweverItsAddressIsWritten(t *testing.T) {
	for _, written := range []string{"https://agentiik.example.com", "https://agentiik.example.com/", "HTTPS://Agentiik.Example.COM"} {
		if got := installationKey(written); got != "https://agentiik.example.com" {
			t.Errorf("%s is kept as %s", written, got)
		}
	}
	if installationKey("https://example.com/agentiik/") != "https://example.com/agentiik" || installationKey("https://example.com/agentiik") == installationKey("https://example.com/other") {
		t.Error("an installation under a path is kept by its path")
	}
}

// The loopback address answers one request to /callback on the address the page was told, and hands
// agk its code: 404 at any other path or host, 405 to another method, 410 to a second request; a
// query carrying anything but one code on its grammar hands agk none, so that it stops; and agk
// waits for the browser no longer than it is told to, nor past an interrupt.
func TestTheLoopbackAddressTakesOneCode(t *testing.T) {
	back, err := listenBack()
	if err != nil {
		t.Fatal(err)
	}
	defer back.close()
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/callback$`).MatchString(back.redirect) {
		t.Fatalf("agk login listens at %s", back.redirect)
	}
	base := strings.TrimSuffix(back.redirect, callbackPath)
	code := "agkcode_" + secret()

	if status, _ := get(t, base+"/favicon.ico"); status != http.StatusNotFound {
		t.Errorf("another path answered %d", status)
	}
	// A page of another site reaching the port through a name of its own carries that name.
	req, _ := http.NewRequest("GET", back.redirect+"?code="+code, nil)
	req.Host = "evil.example:" + back.port
	if answer, err := http.DefaultClient.Do(req); err != nil || answer.StatusCode != http.StatusNotFound {
		t.Errorf("another host answered %v, %v", answer, err)
	}
	if answer, err := http.Post(back.redirect+"?code="+code, "text/plain", nil); err != nil || answer.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a POST answered %v, %v", answer, err)
	}
	status, body := get(t, back.redirect+"?code="+code)
	if status != http.StatusOK || !strings.HasPrefix(body, "Signed in.") {
		t.Errorf("the code answered %d %s", status, body)
	}
	if got, err := back.await(t.Context(), time.Second); got != code || err != nil {
		t.Errorf("agk was handed %q, %v", got, err)
	}
	if status, _ := get(t, back.redirect+"?code="+"agkcode_"+secret()); status != http.StatusGone {
		t.Errorf("a second request answered %d", status)
	}
	select {
	case second := <-back.codes:
		t.Errorf("a second request handed agk %q", second)
	default:
	}

	for _, query := range []string{"", "code=agkcode_short", "code=" + code + "&code=" + code, "code=" + code + "&state=x", "error=access_denied", "code=agktoken_" + secret()} {
		back, err := listenBack()
		if err != nil {
			t.Fatal(err)
		}
		if status, _ := get(t, back.redirect+"?"+query); status != http.StatusBadRequest {
			t.Errorf("?%s answered %d", query, status)
		}
		if got, err := back.await(t.Context(), time.Second); !errors.Is(err, errNoCode) {
			t.Errorf("?%s handed agk %q, %v", query, got, err)
		}
		back.close()
	}

	if _, err := back.await(t.Context(), 10*time.Millisecond); !errors.Is(err, errNoSignIn) {
		t.Errorf("a wait nobody came back to ended %v", err)
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	if _, err := back.await(stopped, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("an interrupted wait ended %v", err)
	}
}

// agk login's label is held to what the installation keeps before anybody signs in for it, and so
// is its command line.
func TestAgkLoginRefusesWhatCannotBeSignedInForFirst(t *testing.T) {
	in := anInstallationSigningIn(t)
	p, pages := signingIn(t, in)
	for args, said := range map[string]string{
		"login --server " + in.URL + " --label " + strings.Repeat("x", 257): "--label: a label is at most 256 characters",
		"login --server " + in.URL + " extra":                               "names nothing after it",
		"login":                                                             "no installation to talk to",
		"login --server http://agentiik.example.com":                        "is plain http",
	} {
		if code, _, errs := p.agk(t, strings.Fields(args)...); code != exitUsage || !strings.Contains(errs, said) {
			t.Errorf("agk %s left with %d: %s", args, code, errs)
		}
	}
	if code, _, errs := p.agk(t, "login", "--server", in.URL, "--label", "two\nlines"); code != exitUsage || !strings.Contains(errs, "one line") {
		t.Errorf("a label of two lines left with %d: %s", code, errs)
	}
	nowhere := &person{env: map[string]string{}, browse: p.browse}
	out, errs := &strings.Builder{}, &strings.Builder{}
	if code := run(t.Context(), Env{Out: out, Err: errs, Getenv: func(k string) string { return nowhere.env[k] }}, []string{"login", "--server", in.URL}); code != exitRefused || !strings.Contains(errs.String(), "keeps no local profile") {
		t.Errorf("an agk keeping no profile left with %d: %s", code, errs)
	}
	if len(*pages) != 0 || len(in.exchanged) != 0 {
		t.Errorf("a refused agk login opened %v and exchanged %v", *pages, in.exchanged)
	}
}

// The verifier is 32 bytes of the system's generator in base64url, 43 characters, a new one each
// time, and the challenge its SHA-256 in base64url with no padding, as RFC 7636 computes S256.
func TestTheChallengeIsTheVerifiersSHA256(t *testing.T) {
	verifier, challenge, err := pkce()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifier))
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(verifier) || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Errorf("the verifier %q is answered the challenge %q", verifier, challenge)
	}
	if other, _, _ := pkce(); other == verifier {
		t.Error("two verifiers are the same")
	}
}
