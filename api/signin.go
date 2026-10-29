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
	"strings"
	"time"

	"github.com/agentiik/agentiik/db"
)

// The sign-in and enrolment page, GET /auth/sign-in and GET /auth/enrol, and the sign-out it
// offers, POST /api/v1/auth/sign-out.
//
// "A passkey ceremony runs in a browser, on a page whose origin the browser checks against the
// Relying Party Identifier", and the console comes in its own releases, so the API serves a page of
// its own on the public URL's origin: HTML, one stylesheet and three scripts, embedded here, which
// load nothing from anywhere else and do the two ceremonies and the password fallback, nothing
// more: signing in with a password, setting one from an enrolment code or a session, and enrolling
// a TOTP generator beside it, whose key the page draws as a QR code with a script of its own, qr.js.
// Each answer of it carries a Content-Security-Policy letting it load its own files and reach
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
// whether it offers the password forms; and agk login's loopback address and challenge, held to
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

	// Passwords is whether the password routes are served: POST /api/v1/auth/login, which the
	// sign-in form calls, and those that set a password and a TOTP generator, which the enrolment
	// form and the signed-in section call. The forms are offered only where they are, and the
	// policy lets passwords in.
	Passwords bool

	// Now is the clock a session ends by, the wall clock where it is nil.
	Now func() time.Time

	// OAuth is the installation's own authorisation server, whose authorisation endpoint is a page
	// of this one, /auth/authorize, served where it is given and nowhere else.
	OAuth *OAuth
}

// SignInAPI is the sign-in page and the sign-out.
type SignInAPI struct {
	pool      *db.Pool
	sessions  *Principals
	passwords bool
	now       func() time.Time
	oauth     *OAuth

	// ipAddressed is an installation whose public URL names an IP address, where a browser runs no
	// ceremony and the policy the API applies lets passwords in.
	ipAddressed bool

	// hsts is the Strict-Transport-Security the page is served with, hstsFor the public URL.
	hsts string

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
		hsts:        hstsFor(u),
		pages:       pages, assets: assets, oauth: o.OAuth,
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
	if s.oauth != nil {
		asked := Public{Why: "the authorisation endpoint is the sign-in page asking a person whether to allow an MCP client: it reads the client's request from its address, fetches the client's document, and changes nothing, the sign-in it asks for being what allows the client"}
		if err := rt.Handle("GET", authorizePath, asked, s.authorizePage); err != nil {
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
	// Password is offered or withheld, to whoever opens the page, and Own is the same to the
	// account whose session the browser carries, as the policy that applies to it says.
	Passkeys, Password, Own string

	// Redirect and Challenge are what agk login opened the sign-in page with, where it did.
	Redirect, Challenge string

	// Reason and Next are a refusal's, and Back, where it is one an MCP client is told of, the
	// address that tells it, BackTo the host it is on.
	Reason, Next string
	Back, BackTo string

	// Authorize is what /auth/authorize shows and hands on, where it is that page.
	Authorize *authorizeData
}

// authorizeData is what the authorisation page shows of a request that holds, and the request itself,
// which it hands on with the sign-in that allows it.
type authorizeData struct {
	// ClientName is the name the client's document gives, ClientHost the host of its client_id,
	// which the draft has the page show "in addition to displaying the fetched client information",
	// and ReturnHost the host its redirect URI names, which the revision has it "clearly display".
	// Loopback says the code goes to a program on this machine.
	ClientName, ClientHost, ReturnHost string
	Loopback                           bool

	// Surface is the resource as a person reads it: /mcp, or a collection by its identifier.
	Surface string

	// Permissions are those asked, each with what it allows.
	Permissions []permissionAsked

	// Deny is the address that tells the client its person denied it.
	Deny string

	// What the page hands on: openapi.json's oauthAuthorization.
	ClientID, RedirectURI, CodeChallenge, Resource, Scope, State string
}

// permissionAsked is one permission a client asks for, and what it allows, in the words the page
// gives a person deciding.
type permissionAsked struct {
	Name, Allows string
}

// allows is what each permission lets a client do, as Permissions says it, for the page.
var allows = map[Permission]string{
	WorkflowRead:   "read your workflows' files, versions and schedules",
	WorkflowRun:    "start, cancel and replay runs, and read the inputs a run takes",
	WorkflowWrite:  "create workflows, commit to them, rename and move them",
	WorkflowDelete: "delete workflows with their runs",
	RunRead:        "read runs, their steps and their logs",
	RunReadData:    "read the inputs, outputs and artifacts of runs",
	SecretUse:      "commit a workflow that names a namespace's secrets",
	SecretWrite:    "declare, change and remove a namespace's secrets, never reading a value",
	GrantManage:    "share namespaces and workflows, and protect a workflow's default branch",
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
	offered, own, err := s.passwordOffered(r)
	if err != nil {
		s.page(w, http.StatusInternalServerError, "refused.html", pageData{
			Title: "Cannot sign in", Reason: "The page could not read the installation's authentication policy.",
			Next: "Try again in a moment.",
		})
		return
	}
	s.page(w, http.StatusOK, "sign-in.html", pageData{
		Title: "Sign in to Agentiik", Scripts: true, Passkeys: s.passkeys(), Password: offering(offered), Own: offering(own),
		Redirect: handOff.RedirectURI, Challenge: handOff.CodeChallenge,
	})
}

// authorizePage is GET /auth/authorize, the authorisation endpoint: the request read from the page's
// address and held to its rules (OAuth.approve), the client's document fetched, and the page naming
// the client, where it is sent back, the resource and the permissions, which allows it by a sign-in.
//
// A client_id or a redirect_uri that does not hold, and a document that cannot be read or does not
// hold, are a page saying so, sending the browser nowhere, since OAuth 2.1 never sends it to an
// address it has not checked. Any other refusal is a page with a link back to the client carrying
// the error, which the person follows: "Authorization servers SHOULD only automatically redirect the
// user agent if it trusts the redirection URI", and an address a document fetched from anywhere names
// is none it trusts.
func (s *SignInAPI) authorizePage(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	refused := func(status int, why, next, back, backTo string) {
		s.page(w, status, "refused.html", pageData{Title: "Cannot allow this client", Reason: sentenceOf(why), Next: next, Back: back, BackTo: backTo})
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		refused(http.StatusBadRequest, "this page's address carries a query that does not read", "Start again from the client, which opens this page as it should be opened.", "", "")
		return
	}
	if len(q["client_id"]) != 1 || len(q["redirect_uri"]) != 1 {
		refused(http.StatusBadRequest, "an MCP client opens this page with one client_id and one redirect_uri, and this address carries another number of one or the other", "Start again from the client, which opens this page as it should be opened.", "", "")
		return
	}
	ask := OAuthSignIn{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), CodeChallenge: q.Get("code_challenge"),
		Resource: q.Get("resource"), Scope: q.Get("scope"), State: q.Get("state"),
	}
	approved, err := s.oauth.approve(r.Context(), ask)
	var client *clientRefused
	var wrong *oauthRefused
	switch {
	case errors.As(err, &client):
		refused(http.StatusBadRequest, client.why, "Nothing was sent to the client. Tell whoever publishes it, or start again from it.", "", "")
		return
	case err != nil && !errors.As(err, &wrong):
		refused(http.StatusInternalServerError, "the client's request could not be checked", "Try again in a moment.", "", "")
		return
	}
	// What approve does not read, once the client and its redirect URI hold, so that each refusal is
	// one sent to the client.
	for _, name := range []string{"response_type", "code_challenge", "code_challenge_method", "resource", "scope", "state"} {
		if len(q[name]) > 1 {
			wrong = &oauthRefused{code: "invalid_request", why: "the request names " + name + " more than once"}
		}
	}
	switch {
	case wrong != nil:
	case q.Get("response_type") == "":
		wrong = &oauthRefused{code: "invalid_request", why: "the request names no response_type, which is code"}
	case q.Get("response_type") != "code":
		wrong = &oauthRefused{code: "unsupported_response_type", why: "response_type is code, the one OAuth 2.1 keeps"}
	case q.Get("code_challenge_method") != "S256":
		wrong = &oauthRefused{code: "invalid_request", why: "code_challenge_method is S256, the one method served, and is required"}
	}
	back := hostOf(ask.RedirectURI)
	if wrong != nil {
		refused(http.StatusBadRequest, wrong.why, "The client is told why when you return to it.", s.oauth.refusedTo(approved, wrong), back)
		return
	}
	offered, own, err := s.passwordOffered(r)
	if err != nil {
		refused(http.StatusInternalServerError, "the page could not read the installation's authentication policy", "Try again in a moment.", "", "")
		return
	}
	surface := "/mcp, everything your account reaches there"
	if approved.collection != "" {
		surface = "your collection " + approved.collection
	}
	data := &authorizeData{
		ClientName: approved.client.Name, ClientHost: hostOf(approved.client.ID), ReturnHost: back,
		Surface: surface, Deny: s.oauth.refusedTo(approved, &oauthRefused{code: "access_denied", why: "the person asked to allow the client denied it"}),
		ClientID: ask.ClientID, RedirectURI: ask.RedirectURI, CodeChallenge: ask.CodeChallenge,
		Resource: ask.Resource, Scope: strings.Join(approved.scope, " "), State: ask.State,
	}
	_, data.Loopback = loopbackWithoutPort(ask.RedirectURI)
	for _, p := range approved.scope {
		data.Permissions = append(data.Permissions, permissionAsked{Name: p, Allows: allows[Permission(p)]})
	}
	s.page(w, http.StatusOK, "authorize.html", pageData{
		Title: "Allow " + approved.client.Name, Scripts: true, Passkeys: s.passkeys(), Password: offering(offered), Own: offering(own),
		Authorize: data,
	})
}

// hostOf is the host of an address, its port with it, as the page names where a client is.
func hostOf(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return address
	}
	return u.Host
}

// enrolPage is GET /auth/enrol. The code of the link that opened it is after its #, which a browser
// never sends: the page reads it from its own address and hands it to the registration's options,
// or to the password it sets, which the page offers beside the passkey where the policy lets
// passwords in, and in its place on an installation addressed by an IP address.
func (s *SignInAPI) enrolPage(w http.ResponseWriter, r *http.Request, _ Principal, _ Target) {
	offered, own, err := s.passwordOffered(r)
	if err != nil {
		s.page(w, http.StatusInternalServerError, "refused.html", pageData{
			Title: "Cannot enrol", Reason: "The page could not read the installation's authentication policy.",
			Next: "Try again in a moment.",
		})
		return
	}
	data := pageData{Title: "Enrol a passkey", Scripts: true, Passkeys: s.passkeys(), Password: "withheld", Own: offering(own)}
	if offered {
		data.Password = "offered"
		if s.ipAddressed {
			data.Title = "Set a password"
		}
	}
	s.page(w, http.StatusOK, "enrol.html", data)
}

// page answers one page of the HTML, written whole before anything is sent, so that a template
// that fails answers a 500 rather than half a page.
func (s *SignInAPI) page(w http.ResponseWriter, status int, name string, data pageData) {
	var b bytes.Buffer
	h := w.Header()
	pageHeaders(h, s.hsts)
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
	pageHeaders(h, s.hsts)
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
// sent from it, since the address of the sign-in page carries agk login's; nosniff, so that a file
// is only ever what its type says; and hsts, where hstsFor gives one, since the page is where the
// password is typed.
func pageHeaders(h http.Header, hsts string) {
	h.Set("Content-Security-Policy", pagePolicy)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	if hsts != "" {
		h.Set("Strict-Transport-Security", hsts)
	}
}

// passwordOffered says whether the page offers the password forms, to whoever opens it and to the
// account whose session the request carries: where the password routes are served and the policy
// that applies lets passwords in, read at each page so that a change applies from the next one. To
// whoever opens it, that is the installation's policy, which the policy of nobody in particular is;
// to a signed-in account, its own, which a namespace it holds a grant in may tighten to forbid them.
// On an installation addressed by an IP address, either lets them in, whatever is stored. A sign-in
// made on the page after it was served is under a policy the page could not know: the routes refuse
// such an account with a 403 naming the setting, and the page then takes the form away.
func (s *SignInAPI) passwordOffered(r *http.Request) (anybody, own bool, err error) {
	if !s.passwords {
		return false, false, nil
	}
	// The session is read as every route reads it, and kept open as a request of its holder's.
	as, err := s.sessions.Identify(r)
	if err != nil {
		return false, false, err
	}
	login := ""
	if as.Principal != "" && as.Token == "" && as.Principal != BootstrapOperator {
		login = string(as.Principal)
	}
	now := s.now()
	err = s.pool.Installation(r.Context(), db.Identity, func(ctx context.Context, wide *db.Wide) error {
		policy, err := policyFor(ctx, wide, "", now, s.ipAddressed)
		if err != nil {
			return err
		}
		anybody, own = !policy.passwordsForbidden, !policy.passwordsForbidden
		if login == "" {
			return nil
		}
		if policy, err = policyFor(ctx, wide, login, now, s.ipAddressed); err != nil {
			return err
		}
		own = !policy.passwordsForbidden
		return nil
	})
	return anybody, own, err
}

// offering is whether the page offers a form, as its data attributes write it.
func offering(offered bool) string {
	if offered {
		return "offered"
	}
	return "withheld"
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
