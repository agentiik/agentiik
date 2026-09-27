package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"time"

	"github.com/agentiik/agentiik/db"
)

// The sign-in and enrolment page, GET /auth/sign-in and GET /auth/enrol, and the sign-out it
// offers, POST /api/v1/auth/sign-out.
//
// "A passkey ceremony runs in a browser, on a page whose origin the browser checks against the
// Relying Party Identifier", and the console comes in its own releases, so the API serves a page of
// its own on the public URL's origin: HTML, one stylesheet and two scripts, embedded here, which
// load nothing from anywhere else and do the two ceremonies and the password fallback, nothing
// more. Each answer of it carries a Content-Security-Policy letting it load its own files and reach
// its own origin and nothing else: no script or style written into the page, no form posted
// anywhere, never framed. The HTML is never cached, since it says what the installation offers when
// it is asked; the stylesheet and the scripts are revalidated at every load, so that an upgrade never
// serves a page beside the scripts of another release.
//
// The page links to its files and the API by relative addresses, so that it works where the public
// URL has a path, behind a proxy serving the API under one.
//
// What the page is told rather than finds out, written into it as it is served: whether a passkey
// ceremony can run here at all, which it cannot on an installation addressed by an IP address;
// whether it offers the password form; and agk login's loopback address and challenge, held to
// their grammar first, so that a page that would hand a code to another host than the loopback is
// never served.

// signinFiles are the page's: the templates of its HTML beside assets, the files served as they are.
//
//go:embed signin
var signinFiles embed.FS

// pagePolicy is the Content-Security-Policy of every answer of the page: its own scripts, its own
// stylesheet and requests to its own origin, and nothing else. form-action 'none' because nothing
// is posted by a form, and a form posted from a page under Referrer-Policy no-referrer carries
// Origin: null, which the API refuses; frame-ancestors 'none' because a ceremony run in a frame is
// refused anyway, and a page framed by another could be dressed to be clicked blind.
const pagePolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'; base-uri 'none'"

// SignInOptions are what the sign-in page is given.
type SignInOptions struct {
	// Pool is where the installation's authentication policy is read, which says whether the page
	// offers the password form. Required where Passwords is set.
	Pool *db.Pool

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy: the origin the page is served on
	// and a ceremony accepted from, and a host that is an IP address where no ceremony runs.
	PublicURL string

	// Sessions is the installation's Principals, accepting sessions: the one a sign-out ends is
	// read and revoked there.
	Sessions *Principals

	// Passwords is whether POST /api/v1/auth/login is served, which the password form calls. The
	// form is offered only where it is, and the policy lets passwords in.
	Passwords bool

	// Now is the clock a session ends by, the wall clock where it is nil.
	Now func() time.Time
}

// SignInAPI is the sign-in page and the sign-out.
type SignInAPI struct {
	pool      *db.Pool
	sessions  *Principals
	passwords bool
	now       func() time.Time

	// ipAddressed is an installation whose public URL names an IP address, where a browser runs no
	// ceremony and the policy the API applies lets passwords in.
	ipAddressed bool

	pages  *template.Template
	assets map[string]asset
}

// asset is one of the page's files, served as it is.
type asset struct {
	body        []byte
	contentType string
	etag        string
}

// assetTypes are the types of the page's files by their extension, each served as what it is,
// since the page is answered nosniff.
var assetTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
}

// NewSignIn registers the page, its files and the sign-out on a router.
func NewSignIn(rt *Router, o SignInOptions) (*SignInAPI, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Sessions == nil || o.Sessions.origin == "":
		return nil, errors.New("api: the sign-in page signs out a browser's session, and sessions are not accepted: call AcceptSessions first")
	case o.Passwords && o.Pool == nil:
		return nil, errors.New("api: no database, and whether the page offers a password is the authentication policy's to say")
	}
	if _, err := originOf(o.PublicURL); err != nil {
		return nil, err
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	pages, err := template.ParseFS(signinFiles, "signin/*.html")
	if err != nil {
		return nil, err
	}
	assets := map[string]asset{}
	err = fs.WalkDir(signinFiles, "signin/assets", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		kind, known := assetTypes[path.Ext(name)]
		if !known {
			return errors.New("api: the sign-in page holds " + name + ", of a type it does not serve")
		}
		body, err := signinFiles.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		assets[path.Base(name)] = asset{body: body, contentType: kind, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s := &SignInAPI{
		pool: o.Pool, sessions: o.Sessions, passwords: o.Passwords, now: o.Now,
		ipAddressed: net.ParseIP(u.Hostname()) != nil,
		pages:       pages, assets: assets,
	}

	page := Public{Why: "the sign-in page is where somebody who holds no credential yet proves who they are: it answers everybody the same page and its files, reads nothing a request carries but agk login's query, and changes nothing"}
	for _, r := range []struct {
		pattern string
		handler Handler
	}{
		{"/auth/sign-in", s.signInPage},
		{"/auth/enrol", s.enrolPage},
		{"/auth/assets/{name}", s.asset},
	} {
		if err := rt.Handle("GET", r.pattern, page, r.handler); err != nil {
			return nil, err
		}
	}
	out := Public{Why: "a sign-out ends the session the request carries, which whoever holds its cookie may end, a session that may only enrol as well as any other, which the router would refuse: it reads the session itself, from the public URL's origin alone"}
	if err := rt.Handle("POST", "/api/v1/auth/sign-out", out, s.signOut); err != nil {
		return nil, err
	}
	return s, nil
}

// pageData is what a template of the page is written with.
type pageData struct {
	Title   string
	Scripts bool

	// Passkeys is available, or unavailable where the installation is addressed by an IP address;
	// Password is offered or withheld.
	Passkeys, Password string

	// Redirect and Challenge are what agk login opened the sign-in page with, where it did.
	Redirect, Challenge string

	// Reason and Next are a refusal's.
	Reason, Next string
}

func (s *SignInAPI) passkeys() string {
	if s.ipAddressed {
		return "unavailable"
	}
	return "available"
}

// signInPage is GET /auth/sign-in.
func (s *SignInAPI) signInPage(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	handOff, err := terminalOf(r.URL.RawQuery)
	if err != nil {
		s.page(w, http.StatusBadRequest, "refused.html", pageData{
			Title: "Cannot sign in", Reason: sentenceOf(err.Error()),
			Next: "Run agk login again, which opens this page as it should be opened.",
		})
		return
	}
	offered, err := s.passwordOffered(r.Context())
	if err != nil {
		s.page(w, http.StatusInternalServerError, "refused.html", pageData{
			Title: "Cannot sign in", Reason: "The page could not read the installation's authentication policy.",
			Next: "Try again in a moment.",
		})
		return
	}
	password := "withheld"
	if offered {
		password = "offered"
	}
	s.page(w, http.StatusOK, "sign-in.html", pageData{
		Title: "Sign in to Agentiik", Scripts: true, Passkeys: s.passkeys(), Password: password,
		Redirect: handOff.RedirectURI, Challenge: handOff.CodeChallenge,
	})
}

// enrolPage is GET /auth/enrol. The code of the link that opened it is after its #, which a browser
// never sends: the page reads it from its own address and hands it to the registration's options.
func (s *SignInAPI) enrolPage(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	s.page(w, http.StatusOK, "enrol.html", pageData{Title: "Enrol a passkey", Scripts: true, Passkeys: s.passkeys()})
}

// page answers one page of the HTML, written whole before anything is sent, so that a template
// that fails answers a 500 rather than half a page.
func (s *SignInAPI) page(w http.ResponseWriter, status int, name string, data pageData) {
	var b bytes.Buffer
	h := w.Header()
	pageHeaders(h)
	if err := s.pages.ExecuteTemplate(&b, name, data); err != nil {
		fail(w, http.StatusInternalServerError, "the page could not be written")
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

// asset is GET /auth/assets/{name}: one of the page's files, the same bytes to everybody, which a
// browser may keep and asks after again at every load, by its ETag.
func (s *SignInAPI) asset(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	h := w.Header()
	pageHeaders(h)
	a, ok := s.assets[r.PathValue("name")]
	if !ok {
		fail(w, http.StatusNotFound, "no such thing, or not yours")
		return
	}
	h.Set("Content-Type", a.contentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", a.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.body))
}

// pageHeaders are what every answer of the page carries: its Content-Security-Policy; no Referer
// sent from it, since the address of the sign-in page carries agk login's; and nosniff, so that a
// file is only ever what its type says.
func pageHeaders(h http.Header) {
	h.Set("Content-Security-Policy", pagePolicy)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

// passwordOffered says whether the page offers the password form: where POST /api/v1/auth/login is
// served and the policy lets passwords in. On an installation addressed by an IP address the policy
// the API applies lets them in, whatever the stored one says; elsewhere the installation's policy
// says, read at each page so that a change applies from the next one. A namespace's policy may forbid
// them to the accounts holding a grant in it, which a page served before anybody signs in cannot
// know: the route refuses such an account with a 403 naming the setting, and the page then takes
// the form away.
func (s *SignInAPI) passwordOffered(ctx context.Context) (bool, error) {
	if !s.passwords {
		return false, nil
	}
	if s.ipAddressed {
		return true, nil
	}
	var policy db.AuthPolicy
	err := s.pool.Installation(ctx, db.Identity, func(ctx context.Context, wide *db.Wide) error {
		var err error
		policy, err = wide.InstallationPolicy(ctx)
		return err
	})
	return err == nil && policy.Password != "forbidden", err
}

// TerminalSignIn is openapi.json's terminalSignIn: what agk login opened the sign-in page with, the
// loopback address it listens on and the SHA-256 of its verifier, which the page hands on with the
// sign-in it completes.
type TerminalSignIn struct {
	RedirectURI   string `json:"redirect_uri"`
	CodeChallenge string `json:"code_challenge"`
}

// loopbackURI is openapi.json's loopbackUri: plain http on a port of the loopback, by address and
// never by the name localhost, since a name can be resolved elsewhere and an address cannot.
var loopbackURI = regexp.MustCompile(`^http://(?:127\.0\.0\.1|\[::1\]):(?:6553[0-5]|655[0-2][0-9]|65[0-4][0-9]{2}|6[0-4][0-9]{3}|[1-5][0-9]{4}|[1-9][0-9]{0,3})(?:/[A-Za-z0-9._~/-]*)?$`)

// codeChallenge is openapi.json's codeChallenge: a SHA-256 in base64url with no padding.
var codeChallenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// terminalOf reads what agk login opened the sign-in page with from its query: both of
// redirect_uri and code_challenge or neither, each once and on its grammar, "so the sign-in page
// can never be made to hand a code to another host". Neither is the zero value.
func terminalOf(query string) (TerminalSignIn, error) {
	q, err := url.ParseQuery(query)
	if err != nil {
		return TerminalSignIn{}, errors.New("this page's address carries a query that does not read")
	}
	redirects, challenges := q["redirect_uri"], q["code_challenge"]
	switch {
	case len(redirects) == 0 && len(challenges) == 0:
		return TerminalSignIn{}, nil
	case len(redirects) != 1 || len(challenges) != 1:
		return TerminalSignIn{}, errors.New("agk login opens this page with one redirect_uri and one code_challenge, and this address carries another number of one or the other")
	case !loopbackURI.MatchString(redirects[0]):
		// Not repeated, since it is whatever the link's author wrote.
		return TerminalSignIn{}, errors.New("redirect_uri is where agk login listens, http on a port of 127.0.0.1 or [::1], and this address names another place, where this page hands no code")
	case !codeChallenge.MatchString(challenges[0]):
		return TerminalSignIn{}, errors.New("code_challenge is the SHA-256 of agk login's verifier, 43 base64url characters, and this one is not")
	}
	return TerminalSignIn{RedirectURI: redirects[0], CodeChallenge: challenges[0]}, nil
}

// sentenceOf is one of the API's sentences as a page shows it, with a capital and a full stop.
func sentenceOf(s string) string {
	if s == "" {
		return s
	}
	if c := s[0]; c >= 'a' && c <= 'z' {
		s = string(c-'a'+'A') + s[1:]
	}
	return s + "."
}

// signOut is POST /api/v1/auth/sign-out: the session the request carries revoked, from its next
// request, and its cookie cleared from the browser.
func (s *SignInAPI) signOut(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	status, why, err := s.sessions.endSession(r, s.now())
	switch {
	case err != nil:
		fail(w, http.StatusInternalServerError, "the session could not be ended")
		return
	case status != 0:
		fail(w, status, why)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", MaxAge: -1,
		Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
