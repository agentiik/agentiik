// Package console carries the web console's build into the programs that serve it.
//
// The console is written in TypeScript, in this directory, and built into dist/, which this package
// embeds: agentiik-api serves it at the root of the public URL, on the origin of the API it talks
// to, so that the console is always the release of that API. The documentation says why it is
// served from the API's binary rather than from an image of its own: a second image on one origin
// needs a proxy in front of both, which neither the Homebrew service nor a Compose installation
// without a proxy has.
//
// # How the build gets here
//
// The release build builds the console into dist/ before it builds the Go programs, which then
// carry it. The directive is unconditional and dist/ carries a committed .gitignore, because
// //go:embed refuses a directory it matches nothing in and this module has to compile on a machine
// that has never built the console. A build that skipped the console's stage therefore carries none,
// which is not a broken build: Files answers nil, and the API serves every route of its own and no
// console, as a build that skipped the helper's stage carries no helper.
//
// all: because a bundler names a chunk it splits off after the module it came from, and some of
// those begin with an underscore, which //go:embed leaves out of a directory unless told otherwise.
// It also takes in dist/.gitignore, which api.NewConsole leaves out with every other name starting
// with a dot: nothing a console serves is one.
//
// # What the build has to be
//
// An index.html at the root of dist/, which is what a browser is answered at every address of the
// console, and in it one <base href="/">, which the API rewrites to the public URL's path so that
// the console's relative addresses resolve from its root at any depth and under any path a proxy
// serves the installation at. Beside it, the files index.html loads, of the types api.NewConsole
// serves. The directory's node_modules is ignored by go.mod, so that a package some dependency
// ships with a .go file in it is never one of this module's.
package console

import (
	"embed"
	"io/fs"
)

// carried is what this build has to offer: the console's build, as the release build puts it in
// dist/, or the committed .gitignore alone where it skipped that stage.
//
//go:embed all:dist
var carried embed.FS

// built is where the build sits inside the embedded filesystem.
const built = "dist"

// Index is the page a browser is answered at every address of the console.
const Index = "index.html"

// Files is the console's build rooted at its index.html, or nil where this build carries none.
//
// A dist/ holding files but no index.html is no console either: nothing would load them, and the
// API serving them would serve a directory rather than a console.
func Files() fs.FS {
	root, err := fs.Sub(carried, built)
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(root, Index); err != nil {
		return nil
	}
	return root
}
