package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/api"
)

// page is a console's index.html as a build writes it: the base the API rewrites, and a script and
// a chunk loaded relative to it.
const page = `<!doctype html><html><head><base href="/"><script type="module" src="assets/app.js"></script></head><body></body></html>`

// served is the page as the console's addresses answer it on https://agentiik.example.com: its base
// and the public URL's origin written in.
var served = strings.Replace(page, `<base href="/">`, `<base href="/"><meta name="agentiik-origin" content="https://agentiik.example.com">`, 1)

// build is a console's build: its page, its script, a chunk named as a bundler names one after the
// module it split off, and the .gitignore the directory is committed with.
func build() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                 {Data: []byte(page)},
		"assets/app.js":              {Data: []byte(`import "./_plugin-helper.js"`)},
		"assets/_plugin-helper.js":   {Data: []byte(`export {}`)},
		"assets/archivo-700.woff2":   {Data: []byte("wOF2")},
		".gitignore":                 {Data: []byte("*\n!.gitignore\n")},
		"assets/.cache/ignored.json": {Data: []byte("{}")},
	}
}

// consoled is a router serving the console of b on the public URL, beside a route under a word of
// /api/v1 and one under /auth, so that what the console never answers is seen answered by the API.
func consoled(t *testing.T, publicURL string, b fstest.MapFS) *api.Router {
	t.Helper()
	rt := router(t, api.DenyAll{})
	ok := func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) { w.Write([]byte("route")) }
	rt.MustHandle("GET", "/api/v1/runners", api.Public{Why: "a route of the API's own, to tell its answer from the console's"}, ok)
	rt.MustHandle("GET", "/auth/sign-in", api.Public{Why: "a route of the API's own, to tell its answer from the console's"}, ok)
	if _, err := api.NewConsole(rt, api.ConsoleOptions{Files: b, PublicURL: publicURL}); err != nil {
		t.Fatal(err)
	}
	return rt
}

// browsed answers one request to rt, as a browser asking for a page sends it.
func browsed(rt *api.Router, method, path string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for name, values := range header {
		r.Header[name] = values
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w
}

// The console is at the root of the public URL: its page at every address of its own, however
// deep, which its script reads to draw what the address names, and each file of its build at its
// path, chunks named with an underscore included, since a bundler names them so.
func TestTheConsoleAnswersEveryAddressOfItsOwn(t *testing.T) {
	rt := consoled(t, "https://agentiik.example.com", build())
	for _, path := range []string{"/", "/finance", "/finance/monthly-invoicing/runs/7", "/finance/monthly-invoicing.git", "/index.html", "/assets/", "/assets/missing.js"} {
		w := browsed(rt, "GET", path, nil)
		if w.Code != http.StatusOK || w.Body.String() != served || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s answered %d %s %q, and not the console's page", path, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
	}
	for path, kind := range map[string]string{
		"/assets/app.js":            "text/javascript; charset=utf-8",
		"/assets/_plugin-helper.js": "text/javascript; charset=utf-8",
		"/assets/archivo-700.woff2": "font/woff2",
	} {
		w := browsed(rt, "GET", path, nil)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != kind || w.Body.String() == served {
			t.Errorf("%s answered %d %s %q", path, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
	}

	// A HEAD is answered as its GET, with no body.
	if w := browsed(rt, "HEAD", "/finance", nil); w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("ETag") == "" {
		t.Errorf("a HEAD answered %d with %d bytes and ETag %q", w.Code, w.Body.Len(), w.Header().Get("ETag"))
	}
}

// Nothing starting with a dot is served, the build directory's .gitignore included: a console
// serves none, and a file under such a name is one a tool left, which nobody meant to publish.
func TestTheConsoleServesNoFileNamedWithADot(t *testing.T) {
	rt := consoled(t, "https://agentiik.example.com", build())
	for _, path := range []string{"/.gitignore", "/assets/.cache/ignored.json"} {
		if w := browsed(rt, "GET", path, nil); w.Body.String() != served {
			t.Errorf("%s answered %d %q, and not the console's page", path, w.Code, w.Body)
		}
	}
}

// Every path under one of the API's roots is the API's, whether or not a route answers it there:
// the console never answers an address of the API's with a page, which a client of the API would
// read as a 200, and never an address a later release serves, /hooks and /mcp among them. A
// repository's path is git's.
func TestTheConsoleNeverAnswersAnAddressOfTheAPIs(t *testing.T) {
	rt := consoled(t, "https://agentiik.example.com", build())
	for path, want := range map[string]string{
		"/api/v1/runners": "route",
		"/auth/sign-in":   "route",
	} {
		if w := browsed(rt, "GET", path, nil); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s answered %d %q, and not its route", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{
		"/api", "/api/", "/api/v1", "/api/v1/runners/nothing", "/api/v1/finance/nothing", "/api/v2/runs",
		"/auth", "/auth/nothing", "/hooks", "/hooks/finance/github", "/mcp", "/mcp/finance/monthly-invoicing",
		"/objects", "/objects/finance/sha256/ab", "/%61pi/v1/nothing", "/finance/monthly-invoicing.git/info/refs",
	} {
		if w := browsed(rt, "GET", path, nil); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "<base") {
			t.Errorf("%s answered %d %q, and it is the API's", path, w.Code, w.Body)
		}
	}
}

// The console answers a browser asking for a page, GET or HEAD, and nothing else: any other method
// is answered by the API as a route nobody registered is, 404, rather than a 405 that would say
// the path is served.
func TestTheConsoleAnswersNoOtherMethod(t *testing.T) {
	rt := consoled(t, "https://agentiik.example.com", build())
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		for _, path := range []string{"/", "/finance/monthly-invoicing"} {
			if w := browsed(rt, method, path, nil); w.Code != http.StatusNotFound {
				t.Errorf("%s %s answered %d %q", method, path, w.Code, w.Body)
			}
		}
	}
}

// A path the mux cleans first is redirected to its clean form, and answered there: read as it
// stands, /x/../api/v1/runners would be the console's page at the API's address.
func TestAPathIsAnsweredInItsCleanForm(t *testing.T) {
	rt := consoled(t, "https://agentiik.example.com", build())
	for path, clean := range map[string]string{
		"//finance":                     "/finance",
		"/finance/./monthly-invoicing":  "/finance/monthly-invoicing",
		"/finance/../api/v1/runners":    "/api/v1/runners",
		"/assets/../assets/app.js":      "/assets/app.js",
		"/finance/monthly-invoicing//7": "/finance/monthly-invoicing/7",
	} {
		w := browsed(rt, "GET", path, nil)
		if w.Code/100 != 3 || w.Header().Get("Location") != clean {
			t.Errorf("%s answered %d to %q", path, w.Code, w.Header().Get("Location"))
		}
	}
}

// Every answer of the console lets it load its own files and reach its own origin and nothing else,
// is never framed, sends no Referer, is only ever the type it says, and is revalidated at every load
// by its ETag, so that an upgrade never serves the page beside another release's scripts.
func TestEveryAnswerOfTheConsoleIsHeldToItsOrigin(t *testing.T) {
	rt := consoled(t, "https://agentiik.example.com", build())
	for _, path := range []string{"/", "/finance/monthly-invoicing", "/assets/app.js"} {
		w := browsed(rt, "GET", path, nil)
		h := w.Header()
		policy := h.Get("Content-Security-Policy")
		for _, directive := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "font-src 'self'", "frame-ancestors 'none'", "form-action 'none'", "base-uri 'self'"} {
			if !strings.Contains(policy, directive) {
				t.Errorf("%s is served under %q, without %s", path, policy, directive)
			}
		}
		if strings.Contains(policy, "unsafe") || strings.Contains(policy, "http") {
			t.Errorf("%s is served under %q, which lets it load what is not its own", path, policy)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s is served with %v", path, h)
		}
		etag := h.Get("ETag")
		if again := browsed(rt, "GET", path, http.Header{"If-None-Match": {etag}}); etag == "" || again.Code != http.StatusNotModified {
			t.Errorf("%s asked again under its ETag %q answered %d", path, etag, again.Code)
		}
	}
	if a, b := browsed(rt, "GET", "/", nil).Header().Get("ETag"), browsed(rt, "GET", "/assets/app.js", nil).Header().Get("ETag"); a == b {
		t.Errorf("the page and a script share the ETag %s", a)
	}
}

// The page's base is the public URL's path, so that its relative addresses resolve from the
// console's root at any depth, and under the path a proxy serves the installation at; and the page
// carries the public URL's origin, which it tells a person they opened it away from.
func TestThePagesBaseIsThePublicURLsPath(t *testing.T) {
	for publicURL, base := range map[string]string{
		"https://agentiik.example.com":              `<base href="/"><meta name="agentiik-origin" content="https://agentiik.example.com">`,
		"https://example.com/agentiik":              `<base href="/agentiik/"><meta name="agentiik-origin" content="https://example.com">`,
		"https://example.com/tools/agentiik":        `<base href="/tools/agentiik/"><meta name="agentiik-origin" content="https://example.com">`,
		"https://example.com/work%20flows":          `<base href="/work%20flows/"><meta name="agentiik-origin" content="https://example.com">`,
		"https://agentiik.example.com:8443/console": `<base href="/console/"><meta name="agentiik-origin" content="https://agentiik.example.com:8443">`,
	} {
		rt := consoled(t, publicURL, build())
		body := browsed(rt, "GET", "/finance/monthly-invoicing/runs/7", nil).Body.String()
		if want := strings.Replace(page, `<base href="/">`, base, 1); body != want {
			t.Errorf("on %s the page is %q, want %q", publicURL, body, want)
		}
	}
}

// An installation serving no MCP says so in the page, which the console's MCP panel reads to say
// that this one serves none, and one serving it, as an installation does by default, says nothing.
func TestThePageSaysWhereNoMCPIsServed(t *testing.T) {
	rt := router(t, api.DenyAll{})
	if _, err := api.NewConsole(rt, api.ConsoleOptions{Files: build(), PublicURL: "https://agentiik.example.com", MCPOff: true}); err != nil {
		t.Fatal(err)
	}
	body := browsed(rt, "GET", "/me/mcp", nil).Body.String()
	want := strings.Replace(page, `<base href="/">`, `<base href="/"><meta name="agentiik-origin" content="https://agentiik.example.com"><meta name="agentiik-mcp" content="off">`, 1)
	if body != want {
		t.Errorf("with AGK_MCP off the page is %q, want %q", body, want)
	}
	if body := browsed(consoled(t, "https://agentiik.example.com", build()), "GET", "/me/mcp", nil).Body.String(); strings.Contains(body, "agentiik-mcp") {
		t.Errorf("an installation serving MCP wrote %q", body)
	}
}

// A build the API cannot serve refuses the start, naming what is wrong with it, rather than
// failing at the first browser to ask: no build at all, one with no page, a page with no base or
// two, and a file of a type the API does not serve.
func TestABuildTheAPICannotServeIsRefused(t *testing.T) {
	without := func(name string) fstest.MapFS {
		b := build()
		delete(b, name)
		return b
	}
	with := func(name, data string) fstest.MapFS {
		b := build()
		b[name] = &fstest.MapFile{Data: []byte(data)}
		return b
	}
	for what, c := range map[string]struct {
		files fstest.MapFS
		says  string
	}{
		"no page":       {without("index.html"), "index.html"},
		"no base":       {with("index.html", `<html><head></head></html>`), `<base href="/">`},
		"another base":  {with("index.html", `<html><head><base href="./"></head></html>`), `<base href="/">`},
		"two bases":     {with("index.html", `<base href="/"><base href="/">`), `<base href="/">`},
		"a gif":         {with("assets/spinner.gif", "GIF89a"), "assets/spinner.gif"},
		"no extension":  {with("assets/LICENSE", "MIT"), "assets/LICENSE"},
		"a nested page": {with("docs/page.php", "<?php"), "docs/page.php"},
		"an API's path": {with("auth/logo.svg", "<svg/>"), "auth/logo.svg"},
		"a hook's path": {with("hooks/index.html", "<html>"), "hooks/index.html"},
		"a git path":    {with("finance/tools.git/info.txt", "refs"), "finance/tools.git/info.txt"},
	} {
		rt := router(t, api.DenyAll{})
		_, err := api.NewConsole(rt, api.ConsoleOptions{Files: c.files, PublicURL: "https://agentiik.example.com"})
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("a build with %s was answered %v", what, err)
		}
	}

	rt := router(t, api.DenyAll{})
	if _, err := api.NewConsole(rt, api.ConsoleOptions{PublicURL: "https://agentiik.example.com"}); err == nil {
		t.Error("a console with no build was served")
	}
	if _, err := api.NewConsole(rt, api.ConsoleOptions{Files: build(), PublicURL: "not a URL"}); err == nil {
		t.Error("a console was served on no public URL")
	}
	if _, err := api.NewConsole(rt, api.ConsoleOptions{Files: build(), PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewConsole(rt, api.ConsoleOptions{Files: build(), PublicURL: "https://agentiik.example.com"}); err == nil {
		t.Error("a router served two consoles")
	}
}

// Where no console is served, AGK_CONSOLE=off or a build that carries none, the root and every
// address of the console are answered as a route nobody registered is, and every route of the
// API's is served as before.
func TestWithNoConsoleEveryOtherRouteIsServed(t *testing.T) {
	rt := router(t, api.DenyAll{})
	ok := func(w http.ResponseWriter, _ *http.Request, _ api.Principal, _ api.Target) { w.Write([]byte("route")) }
	rt.MustHandle("GET", "/auth/sign-in", api.Public{Why: "a route of the API's own, to tell its answer from the console's"}, ok)
	for _, path := range []string{"/", "/finance/monthly-invoicing", "/assets/app.js"} {
		if w := browsed(rt, "GET", path, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s answered %d %q with no console served", path, w.Code, w.Body)
		}
	}
	if w := browsed(rt, "GET", "/auth/sign-in", nil); w.Code != http.StatusOK || w.Body.String() != "route" {
		t.Errorf("the sign-in page answered %d %q with no console served", w.Code, w.Body)
	}
}

// Every path but the API's roots and a repository's is the console's, so a route is registered
// under one of them or not at all: a route at /healthz would be a path the console answers once a
// root is missing, and one an installation serving the console's files from a proxy of its own,
// which sends the API the paths the documentation names, never reaches.
func TestARouteIsRegisteredUnderARootOfTheAPIOrNotAtAll(t *testing.T) {
	rt := router(t, api.DenyAll{})
	ok := func(http.ResponseWriter, *http.Request, api.Principal, api.Target) {}
	public := api.Public{Why: "a route registered to see where the router lets one be"}
	for _, pattern := range []string{"/healthz", "/", "/{namespace}", "/finance/runs", "/apis/v1/runs", "/authn/sign-in", "/mcpx"} {
		if err := rt.Handle("GET", pattern, public, ok); err == nil || !strings.Contains(err.Error(), "console") {
			t.Errorf("GET %s was registered outside the API's roots: %v", pattern, err)
		}
	}
	for _, pattern := range []string{"/auth/probe", "/hooks/{namespace}/probe", "/mcp", "/mcp/{namespace}/{workflow}", "/objects/probe/{key...}", "/api/v1/runners/probe"} {
		if err := rt.Handle("GET", pattern, public, ok); err != nil {
			t.Errorf("GET %s was refused under a root of the API: %v", pattern, err)
		}
	}
}
