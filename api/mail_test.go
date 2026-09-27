package api_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// "never: a recovery link by mail. It would put the account back behind a mailbox and forfeit the
// passkey's phishing resistance." So nothing in this module sends mail, and nothing writes a link to
// be mailed: a recovery code is answered once, to the administrator who issued it, who hands it over.
//
// A rule nothing checks is a preference, and a mailer is one import and one afternoon away, added
// for a notification nobody meant to carry a link. So this reads every file the module ships, the
// Go of every package, the root's and the commands' included, the sign-in page's HTML and scripts,
// the images' Dockerfiles and scripts, and go.mod, and names each that imports a mail package,
// requires a module for mail, or holds a mail transport or a mailto: link. Test files and testdata
// are not read: they ship nowhere, and a test may name what it refuses, as this one does.
func TestNothingSendsMailOrWritesALinkForIt(t *testing.T) {
	// What no shipped file holds, whatever the case of its letters: the protocol mail is sent
	// with, the program that sends it on a host, and a link that opens a message to be sent.
	words := []string{"smtp", "sendmail", "mailto:"}
	// The packages that send or compose mail, and any module whose path says it does.
	mailPackages := map[string]bool{"net/smtp": true, "net/mail": true}

	read, goFiles, pageFiles := 0, 0, 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		shipped := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		for _, suffix := range []string{".js", ".html", ".css", ".sh", ".Dockerfile"} {
			shipped = shipped || strings.HasSuffix(name, suffix)
		}
		if !shipped && name != "go.mod" && name != "Dockerfile" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		read++
		lower := bytes.ToLower(b)
		for _, word := range words {
			if bytes.Contains(lower, []byte(word)) {
				t.Errorf("%s holds %q: nothing in the product sends mail or writes a link to be mailed", path, word)
			}
		}
		switch {
		case name == "go.mod":
			for _, line := range strings.Split(string(lower), "\n") {
				if strings.Contains(line, "mail") {
					t.Errorf("go.mod requires what reads as a module for mail: %s", strings.TrimSpace(line))
				}
			}
		case strings.HasSuffix(name, ".go"):
			goFiles++
			f, err := parser.ParseFile(token.NewFileSet(), path, b, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range f.Imports {
				imported, _ := strconv.Unquote(spec.Path.Value)
				if mailPackages[imported] || strings.Contains(strings.ToLower(imported), "mail") {
					t.Errorf("%s imports %s: nothing in the product sends mail", path, imported)
				}
			}
		case strings.Contains(filepath.ToSlash(path), "api/signin/"):
			pageFiles++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that read nothing would pass by looking at no code at all, and one that missed the
	// page would pass without reading what a browser runs.
	if goFiles < 150 || pageFiles < 5 || read < goFiles+pageFiles+1 {
		t.Fatalf("read %d files, %d of them Go and %d of the sign-in page's, and the module ships more than that", read, goFiles, pageFiles)
	}
}
