package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// The web console, served at the root of the public URL, on the API's origin, beside /api/v1 and
// /auth: every address that is not one of the API's own answers the console's page or one of its
// files, so that the console is always the release of the API it talks to.
//
// It is not a route. A route is under one of the API's roots and passes the authorisation hook;
// the console is what a browser is answered everywhere else, to GET and HEAD alone, and the same to
// everybody, since it reads nothing a request carries but its path and holds nothing but the build
// this binary carries: the console asks the API for everything else, as any client does, under the
// session its page signed in with. So it is not in Routes, and the router hands it a request only
// once the request is outside every root a route may take: see Router.consoles.
//
// A request for a file of the build is answered that file, and any other the console's index.html,
// whose own script reads the address and draws what it names. A missing file is therefore answered
// the page rather than a 404, as a static host serving a single page answers one; its type says what
// it is, and a browser loads no script or stylesheet of another type, since every answer is nosniff.
//
// Every answer is revalidated at every load, by its ETag, so that an upgrade never serves a page
// beside the scripts of another release, as the sign-in page's files are; and carries a
// Content-Security-Policy letting the console load its own files and reach its own origin, and
// nothing else.

// consolePolicy is the Content-Security-Policy of every answer of the console: its own scripts,
// stylesheets, images, fonts and manifest, requests to its own origin and nothing else, as the
// sign-in page's own; base-uri 'self' rather than 'none' because its page carries the <base> the
// API writes into it. A face the console draws with is one of its files, never a font service's,
// since a request to another origin would tell that origin who opens the console and when.
const consolePolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; manifest-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'self'"

// consoleBase is what the console's index.html carries for the API to rewrite: the base its
// relative addresses resolve against, which is the root of the origin until the API says the public
// URL has a path.
const consoleBase = `<base href="/">`

// consoleTypes are the types of the console's files by their extension, each served as what it is,
// since every answer is nosniff, and fixed here rather than read from the host's MIME table, which
// an image with no /etc/mime.types does not have. A build holding a file of another type is refused
// at start rather than served as something a browser would guess at.
var consoleTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
}

// ConsoleOptions are what the console is given.
type ConsoleOptions struct {
	// Files is the console's build, rooted at its index.html: console.Files, which is nil where
	// this binary carries none, and then there is no console to serve.
	Files fs.FS

	// PublicURL is AGK_PUBLIC_URL, or AGK_PROXY_URL behind a proxy, whose path, where it has one,
	// is the root the console's addresses resolve against.
	PublicURL string
}

// Console is the web console as the API serves it.
type Console struct {
	// index is the console's page, with the public URL's path written into its <base>.
	index asset

	// files are the build's other files, by their path from its root without the leading slash.
	files map[string]asset
}

// NewConsole reads the console's build and has the router answer with it every request outside
// the API's roots.
//
// Every file is read once, here, so that a build the API cannot serve is refused at start rather
// than at the first browser to ask for the file: an index.html with no <base href="/">, which would
// load nothing at an address below the root, a file of a type it does not serve, and a file at an
// address of the API's.
func NewConsole(rt *Router, o ConsoleOptions) (*Console, error) {
	switch {
	case rt == nil:
		return nil, errors.New("api: no router")
	case o.Files == nil:
		return nil, errors.New("api: no console to serve: this binary carries no console's build")
	case rt.console != nil:
		return nil, errors.New("api: the router already serves a console")
	}
	if _, err := originOf(o.PublicURL); err != nil {
		return nil, err
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil {
		return nil, err
	}
	c := &Console{files: map[string]asset{}}
	err = fs.WalkDir(o.Files, ".", func(name string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case name != "." && strings.HasPrefix(d.Name(), "."):
			// The build's .gitignore, and anything else a tool leaves under a dotted name: a
			// console serves none, and one served would be a file nobody meant to publish.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case d.IsDir():
			return nil
		case underRoot("/"+name) || gitPath("/"+name):
			// A file no browser would ever be answered, since its address is the API's or git's:
			// a console that loads it is one that works nowhere, which the start says rather than
			// the first page to be missing it.
			return errors.New("api: the console's build holds " + name + ", at an address of the API's, which the console never answers")
		}
		kind, known := consoleTypes[path.Ext(name)]
		if !known {
			return errors.New("api: the console's build holds " + name + ", of a type the API does not serve")
		}
		body, err := fs.ReadFile(o.Files, name)
		if err != nil {
			return err
		}
		c.files[name] = asset{body: body, contentType: kind, etag: etagOf(body)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	page, ok := c.files["index.html"]
	if !ok {
		return nil, errors.New("api: the console's build holds no index.html, which is the page every address of it answers")
	}
	if bytes.Count(page.body, []byte(consoleBase)) != 1 {
		return nil, errors.New("api: the console's index.html carries no " + consoleBase + ", or more than one, where the API writes the public URL's path for its addresses to resolve against")
	}
	// The path alone, since the page is served on the public URL's origin whichever host a
	// browser named, and ending in a slash, since a base without one resolves against its parent.
	base := `<base href="` + html.EscapeString(strings.TrimRight(u.EscapedPath(), "/")+"/") + `">`
	body := bytes.Replace(page.body, []byte(consoleBase), []byte(base), 1)
	c.index = asset{body: body, contentType: page.contentType, etag: etagOf(body)}
	delete(c.files, "index.html")
	rt.console = c
	return c, nil
}

// etagOf is the strong validator of a file served as it is, the same for the same bytes.
func etagOf(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// ServeHTTP answers one request the router found outside every root of the API: the build's file
// its path names, or the console's page.
func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", consolePolicy)
	// No Referer, since an address of the console names a namespace and a workflow, which a link
	// followed out of it would otherwise tell the site it leads to.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	a, ok := c.files[strings.TrimPrefix(r.URL.Path, "/")]
	if !ok {
		a = c.index
	}
	h.Set("Content-Type", a.contentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", a.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.body))
}
