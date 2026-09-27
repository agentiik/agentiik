package api_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// The sign-in and enrolment page and the sign-out it offers, against a real PostgreSQL: a page
// under a policy that lets it load nothing from anywhere else and run no script it did not load,
// never cached; agk login's hand-off carried on its grammar alone; the password form offered where
// the route is served and the policy lets passwords in; passkeys said to be unavailable where the
// installation is addressed by an IP address; and a sign-out that ends the one session it carries,
// from the public URL's origin alone, and clears its cookie.

// pagePolicy is the Content-Security-Policy every answer of the page carries.
const pagePolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'; base-uri 'none'"

// signInOn is the page of the installation of sessions, served on publicURL, with the password
// form where passwords is set.
func (in sessions) signInOn(t *testing.T, publicURL string, passwords bool) http.Handler {
	t.Helper()
	rt, err := api.NewRouter(in.p, in.p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewSignIn(rt, api.SignInOptions{
		Pool: in.pool, PublicURL: publicURL, Sessions: in.p, Passwords: passwords,
		Now: func() time.Time { return *in.clock },
	}); err != nil {
		t.Fatal(err)
	}
	return rt
}

// fetched is what h answers a GET of path, with the headers given as name and value in turn.
func fetched(t *testing.T, h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "GET", path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Add(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// attribute is the value of the first attribute of that name in page, unescaped, and whether there
// is one.
func attribute(page, name string) (string, bool) {
	m := regexp.MustCompile(`\s` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1]), true
}

// The two pages, a refusal of one and the files they load all carry the policy, no Referer and
// nosniff. The HTML is never kept by a cache, since it says what the installation offers when it
// is asked; the files are kept and asked after again by their ETag. Everything the HTML loads is
// served, from the page's own directory.
func TestThePageIsServedUnderAPolicyThatLoadsNothingFromElsewhere(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com/console", false)
	held := func(what string, w *httptest.ResponseRecorder) {
		t.Helper()
		for header, want := range map[string]string{
			"Content-Security-Policy": pagePolicy,
			"Referrer-Policy":         "no-referrer",
			"X-Content-Type-Options":  "nosniff",
		} {
			if got := w.Header().Values(header); len(got) != 1 || got[0] != want {
				t.Errorf("%s is answered %s %q", what, header, got)
			}
		}
	}
	for path, status := range map[string]int{
		"/auth/sign-in": http.StatusOK,
		"/auth/enrol":   http.StatusOK,
		"/auth/sign-in?redirect_uri=http%3A%2F%2F127.0.0.1%3A53682%2Fcallback&code_challenge=Ibi4l3hyoxxry38-L3XZ59u9IdHegygM4WK38DG2YKk": http.StatusOK,
		"/auth/sign-in?redirect_uri=http%3A%2F%2F127.0.0.1%3A53682%2Fcallback":                                                            http.StatusBadRequest,
	} {
		w := fetched(t, h, path)
		if w.Code != status {
			t.Fatalf("%s answered %d: %s", path, w.Code, w.Body)
		}
		held(path, w)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s is answered Cache-Control %q", path, got)
		}
		if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("%s is answered Content-Type %q", path, got)
		}
		if status != http.StatusOK {
			continue
		}
		// Each address the page names is relative, so that it works under the public URL's path,
		// and served.
		base, _ := url.Parse("https://agentiik.example.com" + path)
		for _, m := range regexp.MustCompile(`\s(?:src|href)="([^"]*)"`).FindAllStringSubmatch(w.Body.String(), -1) {
			named := html.UnescapeString(m[1])
			if named == "data:," {
				continue
			}
			u, err := url.Parse(named)
			if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(named, "/") {
				t.Errorf("%s names %q, which is not relative to the page", path, named)
				continue
			}
			if loaded := fetched(t, h, base.ResolveReference(u).Path); loaded.Code != http.StatusOK {
				t.Errorf("%s names %q, which is answered %d", path, named, loaded.Code)
			}
		}
	}

	for name, kind := range map[string]string{
		"page.css": "text/css; charset=utf-8",
		"codec.js": "text/javascript; charset=utf-8",
		"page.js":  "text/javascript; charset=utf-8",
	} {
		w := fetched(t, h, "/auth/assets/"+name)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Fatalf("%s answered %d: %s", name, w.Code, w.Body)
		}
		held(name, w)
		if got := w.Header().Get("Content-Type"); got != kind {
			t.Errorf("%s is answered Content-Type %q", name, got)
		}
		etag := w.Header().Get("ETag")
		if got := w.Header().Get("Cache-Control"); got != "no-cache" || etag == "" {
			t.Errorf("%s is answered Cache-Control %q and ETag %q", name, got, etag)
		}
		if again := fetched(t, h, "/auth/assets/"+name, "If-None-Match", etag); again.Code != http.StatusNotModified {
			t.Errorf("%s asked after again by its ETag answered %d", name, again.Code)
		}
	}
	for _, path := range []string{"/auth/assets/nothing.js", "/auth/assets/sign-in.html", "/auth/assets/", "/auth/signin/assets/page.js"} {
		if w := fetched(t, h, path); w.Code != http.StatusNotFound {
			t.Errorf("%s answered %d", path, w.Code)
		}
	}
}

// No script or style is written into a page, which the policy would refuse to run anyway: every
// script is loaded from a file, no element carries a style or a handler, and nothing is a
// javascript: address. The scripts write text, never markup, so that nothing an answer says can
// become part of the page.
func TestThePageHoldsNoScriptOrStyleOfItsOwn(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com", true)
	for _, path := range []string{"/auth/sign-in", "/auth/enrol", "/auth/sign-in?code_challenge=x"} {
		page := fetched(t, h, path).Body.String()
		if !strings.Contains(page, "<h1>") {
			t.Fatalf("%s answered no page: %s", path, page)
		}
		for what, found := range map[string]*regexp.Regexp{
			"a style element":      regexp.MustCompile(`(?i)<style`),
			"a style attribute":    regexp.MustCompile(`(?i)\sstyle\s*=`),
			"a handler attribute":  regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
			"a javascript address": regexp.MustCompile(`(?i)javascript:`),
		} {
			if found.MatchString(page) {
				t.Errorf("%s holds %s", path, what)
			}
		}
		for _, tag := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(page, -1) {
			if !regexp.MustCompile(`\ssrc="[^"]+"`).MatchString(tag) {
				t.Errorf("%s holds a script written into it: %s", path, tag)
			}
		}
		for _, closing := range regexp.MustCompile(`(?is)<script[^>]*>(.*?)</script>`).FindAllStringSubmatch(page, -1) {
			if strings.TrimSpace(closing[1]) != "" {
				t.Errorf("%s holds a script with a body: %q", path, closing[1])
			}
		}
	}
	for _, name := range []string{"codec.js", "page.js"} {
		script := fetched(t, h, "/auth/assets/"+name).Body.String()
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute(\"style\""} {
			if strings.Contains(script, sink) {
				t.Errorf("%s writes through %s", name, sink)
			}
		}
	}
}

// Every element the page's script names is on the page it runs on: the sign-in page's and the
// enrolment page's own, each on its page, and those both share on both. A name the script looks up
// and the page does not hold stops the script there, in a browser, which no other test runs it in.
func TestEveryElementThePageScriptNamesIsOnItsPage(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com", true)
	script := fetched(t, h, "/auth/assets/page.js").Body.String()
	shared, rest, ok := strings.Cut(script, "// The sign-in page.")
	signIn, enrol, ok2 := strings.Cut(rest, "// The enrolment page.")
	if !ok || !ok2 {
		t.Fatal("page.js no longer says where each page's part of it starts")
	}
	named := func(part string) []string {
		var ids []string
		for _, m := range regexp.MustCompile(`(?:\$|getElementById)\("([^"]+)"\)`).FindAllStringSubmatch(part, -1) {
			ids = append(ids, m[1])
		}
		if len(ids) == 0 {
			t.Fatalf("a part of page.js names no element:\n%s", part)
		}
		return ids
	}
	pages := map[string]string{"/auth/sign-in": signIn, "/auth/enrol": enrol}
	for path, own := range pages {
		page := fetched(t, h, path).Body.String()
		for _, id := range append(named(shared), named(own)...) {
			if n := strings.Count(page, ` id="`+id+`"`); n != 1 {
				t.Errorf("%s holds %d elements of id %q, which page.js names", path, n, id)
			}
		}
	}
}

// agk login opens the sign-in page with the loopback address it listens on and its challenge, which
// the page is written with. Where one comes without the other, or either is outside its grammar,
// the page is not served: it would hand a code to a host other than the loopback, or one no
// verifier opens.
func TestTheSignInPageCarriesAgkLoginsHandOffOnItsGrammarAlone(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com", false)
	const challenge = "Ibi4l3hyoxxry38-L3XZ59u9IdHegygM4WK38DG2YKk"
	for _, redirect := range []string{"http://127.0.0.1:53682/callback", "http://[::1]:61020/", "http://127.0.0.1:1", "http://127.0.0.1:65535/a/b.c~_-"} {
		w := fetched(t, h, "/auth/sign-in?"+url.Values{"redirect_uri": {redirect}, "code_challenge": {challenge}, "other": {"kept out"}}.Encode())
		if w.Code != http.StatusOK {
			t.Errorf("%s answered %d: %s", redirect, w.Code, w.Body)
			continue
		}
		page := w.Body.String()
		if got, _ := attribute(page, "data-terminal-redirect"); got != redirect {
			t.Errorf("%s: the page carries redirect %q", redirect, got)
		}
		if got, _ := attribute(page, "data-terminal-challenge"); got != challenge {
			t.Errorf("%s: the page carries challenge %q", redirect, got)
		}
	}
	w := fetched(t, h, "/auth/sign-in")
	for _, name := range []string{"data-terminal-redirect", "data-terminal-challenge"} {
		if got, ok := attribute(w.Body.String(), name); !ok || got != "" {
			t.Errorf("a page agk login did not open carries %s %q", name, got)
		}
	}

	for query, why := range map[string]string{
		"redirect_uri=http://127.0.0.1:53682/": "one redirect_uri and one code_challenge",
		"code_challenge=" + challenge:          "one redirect_uri and one code_challenge",
		"redirect_uri=http://127.0.0.1:1&redirect_uri=http://127.0.0.1:2&code_challenge=" + challenge: "one redirect_uri and one code_challenge",
		"redirect_uri=http://localhost:53682/&code_challenge=" + challenge:                            "127.0.0.1 or [::1]",
		"redirect_uri=https://127.0.0.1:53682/&code_challenge=" + challenge:                           "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.2:53682/&code_challenge=" + challenge:                            "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1.evil.example:53682/&code_challenge=" + challenge:               "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1@evil.example:53682/&code_challenge=" + challenge:               "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1:0/&code_challenge=" + challenge:                                "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1:65536/&code_challenge=" + challenge:                            "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1/&code_challenge=" + challenge:                                  "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1:53682/?code=x&code_challenge=" + challenge:                     "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1:53682/%23x&code_challenge=" + challenge:                        "127.0.0.1 or [::1]",
		"redirect_uri=&code_challenge=" + challenge:                                                   "127.0.0.1 or [::1]",
		"redirect_uri=http://127.0.0.1:53682/&code_challenge=" + challenge[1:]:                        "43 base64url characters",
		"redirect_uri=http://127.0.0.1:53682/&code_challenge=" + challenge + "A":                      "43 base64url characters",
		"redirect_uri=http://127.0.0.1:53682/&code_challenge=" + challenge[1:] + "=":                  "43 base64url characters",
		"redirect_uri=http://127.0.0.1:53682/&code_challenge=" + challenge + ";x":                     "does not read",
		"redirect_uri=http://127.0.0.1:53682/&code_challenge=%zz":                                     "does not read",
	} {
		w := fetched(t, h, "/auth/sign-in?"+query)
		page := w.Body.String()
		if w.Code != http.StatusBadRequest || !strings.Contains(page, "Cannot sign in") || !strings.Contains(page, why) {
			t.Errorf("%s answered %d: %s", query, w.Code, page)
		}
		if strings.Contains(page, "data-terminal") || strings.Contains(page, "<script") {
			t.Errorf("%s answered a page that signs in: %s", query, page)
		}
	}
}

// A browser runs no passkey ceremony for an installation addressed by an IP address, and the page
// says so before anybody tries: it is written with passkeys unavailable, which its script tells the
// person with the reason. A name, localhost among them, runs one.
func TestThePageSaysWherePasskeysCannotRun(t *testing.T) {
	in := someSessions(t)
	for publicURL, passkeys := range map[string]string{
		"https://192.0.2.10":                "unavailable",
		"https://192.0.2.10:8443/agentiik":  "unavailable",
		"https://[2001:db8::1]":             "unavailable",
		"https://agentiik.example.com":      "available",
		"https://localhost:8443":            "available",
		"https://agentiik.example.com:8443": "available",
	} {
		h := in.signInOn(t, publicURL, false)
		for _, path := range []string{"/auth/sign-in", "/auth/enrol"} {
			if got, _ := attribute(fetched(t, h, path).Body.String(), "data-passkeys"); got != passkeys {
				t.Errorf("%s on %s says passkeys are %q", path, publicURL, got)
			}
		}
	}
	script := fetched(t, in.signInOn(t, "https://192.0.2.10", false), "/auth/assets/page.js").Body.String()
	if !strings.Contains(script, `"unavailable"`) || !strings.Contains(script, "addressed by an IP address") {
		t.Error("the page's script does not say why passkeys are unavailable")
	}
}

// The page offers the password form where the password route is served and the policy lets
// passwords in, which it reads at each page, so that a change applies from the next one. An
// installation addressed by an IP address lets them in whatever the stored policy says, since
// nobody could sign in there otherwise.
func TestThePasswordFormIsOfferedWhereTheRouteIsServedAndThePolicyLetsPasswordsIn(t *testing.T) {
	in := someSessions(t)
	password := func(h http.Handler) string {
		t.Helper()
		w := fetched(t, h, "/auth/sign-in")
		if w.Code != http.StatusOK {
			t.Fatalf("the page answered %d: %s", w.Code, w.Body)
		}
		got, _ := attribute(w.Body.String(), "data-password")
		return got
	}
	policy := func(setting string) {
		t.Helper()
		bound := false
		if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.SetInstallationPolicy(ctx, db.AuthPolicy{
				Password: setting, Passkey: "required", UserVerification: "required", DeviceBoundOnly: &bound, MinPasskeys: 2,
			}, *in.clock)
		}); err != nil {
			t.Fatal(err)
		}
	}
	withheld := in.signInOn(t, "https://agentiik.example.com", false)
	served := in.signInOn(t, "https://agentiik.example.com", true)
	byAddress := in.signInOn(t, "https://192.0.2.10", true)

	policy("allowed")
	if got := password(withheld); got != "withheld" {
		t.Errorf("with no password route, the page's password form is %q", got)
	}
	if got := password(served); got != "offered" {
		t.Errorf("where passwords are allowed, the page's password form is %q", got)
	}
	policy("forbidden")
	if got := password(served); got != "withheld" {
		t.Errorf("where passwords are forbidden, the page's password form is %q", got)
	}
	if got := password(byAddress); got != "offered" {
		t.Errorf("on an installation addressed by an IP address, the page's password form is %q", got)
	}
	policy("allowed")
	if got := password(served); got != "offered" {
		t.Errorf("once passwords are allowed again, the page's password form is %q", got)
	}
}

// The page is served only where sessions are accepted, since it signs one out, and on a public URL
// a browser's page can have.
func TestTheSignInPageNeedsSessions(t *testing.T) {
	in := someSessions(t)
	p, err := api.NewPrincipals(in.pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := api.NewRouter(p, p.Identify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewSignIn(rt, api.SignInOptions{PublicURL: "https://agentiik.example.com", Sessions: p}); err == nil || !strings.Contains(err.Error(), "AcceptSessions") {
		t.Errorf("a page over principals that accept no session was built: %v", err)
	}
	if err := in.p.AcceptSessions("https://agentiik.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewSignIn(rt, api.SignInOptions{PublicURL: "http://agentiik.example.com", Sessions: in.p}); err == nil {
		t.Error("a page on a public URL no browser page has was built")
	}
	if len(rt.Routes()) != 0 {
		t.Errorf("a refused page is on the surface: %+v", rt.Routes())
	}
}

// signOut posts a sign-out from origin, carrying the cookies given.
func signOut(t *testing.T, h http.Handler, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := request(t, "POST", "/api/v1/auth/sign-out", origin, cookies...)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// cleared says whether an answer clears the session's cookie from the browser: the same name, an
// empty value and a Max-Age of 0, with the attributes the __Host- prefix requires a browser to see
// before it takes the cookie at all.
func cleared(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == api.SessionCookie && c.Value == "" && c.MaxAge < 0 && c.Path == "/" && c.Domain == "" &&
			c.Secure && c.HttpOnly && c.SameSite == http.SameSiteLaxMode {
			return strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0")
		}
	}
	return false
}

// A sign-out ends the session it carries, from its next request, and clears its cookie; the same
// user's other sessions go on. A session ended already, and no session at all, sign out as well:
// the browser is signed out either way, which is what was asked.
func TestASignOutEndsTheSessionItCarriesAndClearsItsCookie(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com/console", false)
	ending := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	other := in.open(t, "alice", api.OpenedBy{Credential: "alice-password"})

	w := signOut(t, h, publicOrigin, ending)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("a sign-out answered %d: %s", w.Code, w.Body)
	}
	if !cleared(w) || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a sign-out answered Set-Cookie %q and Cache-Control %q", w.Header().Values("Set-Cookie"), w.Header().Get("Cache-Control"))
	}
	if as := in.asked(t, "GET", "", ending); as.Principal != "" || !strings.Contains(as.Refused, "that session opens nothing") {
		t.Errorf("a session signed out of identified %+v", as)
	}
	if as := in.asked(t, "GET", "", other); as.Principal != "alice" {
		t.Errorf("signing out of one session ended another: %+v", as)
	}

	for what, cookies := range map[string][]*http.Cookie{
		"a session ended already":      {ending},
		"no session":                   nil,
		"a session nobody ever opened": {{Name: api.SessionCookie, Value: "wi_A5PaJaVU_FCqBRQ18T1mIolRGXZLNzF0fkSncFl0"}},
	} {
		if w := signOut(t, h, publicOrigin, cookies...); w.Code != http.StatusNoContent || !cleared(w) {
			t.Errorf("a sign-out carrying %s answered %d, Set-Cookie %q: %s", what, w.Code, w.Header().Values("Set-Cookie"), w.Body)
		}
	}
	if as := in.asked(t, "GET", "", other); as.Principal != "alice" {
		t.Errorf("a sign-out carrying no session of alice's ended hers: %+v", as)
	}
}

// A sign-out changes something, and is refused unless it comes from the public URL's origin,
// whatever it carries: a page of another host of the same site, which SameSite=Lax lets send the
// cookie, signs nobody out, and a form posted from the page itself carries Origin: null.
func TestASignOutIsRefusedFromAnotherOrigin(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com", false)
	c := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	for _, origin := range []string{"", "null", "https://evil.example.com", "https://console.agentiik.example.com", "http://agentiik.example.com", "https://agentiik.example.com:8443"} {
		for _, cookies := range [][]*http.Cookie{{c}, nil} {
			w := signOut(t, h, origin, cookies...)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "public URL") || len(w.Result().Cookies()) != 0 {
				t.Errorf("a sign-out from %q carrying %d cookies answered %d, Set-Cookie %q: %s", origin, len(cookies), w.Code, w.Header().Values("Set-Cookie"), w.Body)
			}
		}
	}
	r := request(t, "POST", "/api/v1/auth/sign-out", publicOrigin, c)
	r.Header.Add("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("a sign-out with two Origin headers answered %d: %s", w.Code, w.Body)
	}
	if as := in.asked(t, "GET", "", c); as.Principal != "alice" {
		t.Errorf("a sign-out refused ended the session: %+v", as)
	}
}

// A session that may only enrol, which the router refuses everywhere else, is signed out of like
// any other.
func TestASignOutEndsASessionThatMayOnlyEnrol(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com", false)
	c := in.open(t, "alice", api.OpenedBy{EnrolmentCode: in.recovery(t, "alice", "agkenrol_signing-out")})
	if as := in.asked(t, "GET", "", c); as.Principal != "alice" || !as.Enrolling {
		t.Fatalf("a session a recovery code opened identified %+v", as)
	}
	if w := signOut(t, h, publicOrigin, c); w.Code != http.StatusNoContent || !cleared(w) {
		t.Fatalf("a sign-out of a session that may only enrol answered %d: %s", w.Code, w.Body)
	}
	if as := in.asked(t, "GET", "", c); as.Principal != "" {
		t.Errorf("a session that may only enrol, signed out of, identified %+v", as)
	}
}

// A suspended user's session opens nothing while the suspension lasts and would open again once it
// is lifted; signed out of meanwhile, it stays ended.
func TestASessionSignedOutOfWhileItsUserIsSuspendedStaysEnded(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com", false)
	c := in.open(t, "carol", api.OpenedBy{Credential: "carol-passkey"})
	suspend := func(suspended bool) {
		t.Helper()
		if err := in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
			return w.UpdateUser(ctx, db.User{Login: "carol", DisplayName: "Carol", Admin: true, Suspended: suspended})
		}); err != nil {
			t.Fatal(err)
		}
	}
	suspend(true)
	if w := signOut(t, h, publicOrigin, c); w.Code != http.StatusNoContent || !cleared(w) {
		t.Fatalf("a sign-out of a suspended user's session answered %d: %s", w.Code, w.Body)
	}
	suspend(false)
	if as := in.asked(t, "GET", "", c); as.Principal != "" {
		t.Errorf("a session signed out of while its user was suspended opened again: %+v", as)
	}
}

// A sign-out carries one session and nothing else: a bearer token is no session, and signs nothing
// out, and two credentials are refused as they are everywhere, ending neither.
func TestASignOutCarryingATokenOrTwoSessionsIsRefused(t *testing.T) {
	in := someSessions(t)
	h := in.signInOn(t, "https://agentiik.example.com", false)
	first := in.open(t, "alice", api.OpenedBy{Credential: "alice-passkey"})
	second := in.open(t, "carol", api.OpenedBy{Credential: "carol-passkey"})
	token := in.token(t, "alice", nil, nil, in.clock.Add(time.Hour))

	bearing := func(cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := request(t, "POST", "/api/v1/auth/sign-out", publicOrigin, cookies...)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for what, w := range map[string]*httptest.ResponseRecorder{
		"a bearer token":                 bearing(),
		"a bearer token beside a cookie": bearing(first),
		"two cookies":                    signOut(t, h, publicOrigin, first, second),
	} {
		if w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
			t.Errorf("a sign-out carrying %s answered %d, Set-Cookie %q: %s", what, w.Code, w.Header().Values("Set-Cookie"), w.Body)
		}
	}
	if w := bearing(); !strings.Contains(w.Body.String(), "DELETE /api/v1/auth/tokens/{id}") {
		t.Errorf("a sign-out carrying a bearer token does not say how a token is revoked: %s", w.Body)
	}
	for login, c := range map[string]*http.Cookie{"alice": first, "carol": second} {
		if as := in.asked(t, "GET", "", c); string(as.Principal) != login {
			t.Errorf("a sign-out refused ended %s's session: %+v", login, as)
		}
	}
}
