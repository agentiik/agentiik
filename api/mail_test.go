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
// Go of every package, the root's and the commands' included, whatever its build tags, the sign-in
// page's HTML and scripts, the migrations, the images' Dockerfiles and scripts, go.mod, and whatever
// a //go:embed directive puts in a binary, and names each that imports a mail package, requires a
// module for mail, or holds a mail transport, a provider's name, the word email or a mailto: link.
// Test files and testdata are not read: they ship nowhere, and a test may name what it refuses, as
// this one does.
func TestNothingSendsMailOrWritesALinkForIt(t *testing.T) {
	// What no shipped file holds, whatever the case of its letters: the protocol mail is sent
	// with, the program that sends it on a host, a link that opens a message to be sent, the word
	// itself, which a mailer calling a provider's HTTP API writes somewhere, in a field, a route
	// or a function's name, and the providers whose APIs send it.
	words := []string{"smtp", "sendmail", "mailto:", "email", "e-mail", "sendgrid", "mailgun", "postmark", "mandrill", "sesv2", "sparkpost"}
	// The packages that send or compose mail, and any module whose path says it does.
	mailPackages := map[string]bool{"net/smtp": true, "net/mail": true}

	read := map[string]bool{}
	var embedded []string
	goFiles, pageFiles, migrations := 0, 0, 0
	check := func(path string) error {
		if read[path] {
			return nil
		}
		read[path] = true
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lower := bytes.ToLower(b)
		for _, word := range words {
			// Git's author, committer and tagger lines carry an email address, and git names its
			// own checks of them for it, missingEmail and badEmail among them. Package repo reads
			// that format as the data a commit holds and sends nothing to anyone, so it may hold
			// that word, and none of the others.
			if word == "email" && strings.HasPrefix(filepath.ToSlash(path), "../repo/") {
				continue
			}
			if bytes.Contains(lower, []byte(word)) {
				t.Errorf("%s holds %q: nothing in the product sends mail or writes a link to be mailed", path, word)
			}
		}
		name := filepath.Base(path)
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
			// What the file embeds ships in the binary as surely as the file does.
			for _, line := range strings.Split(string(b), "\n") {
				patterns, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:embed ")
				if !ok {
					continue
				}
				for _, pattern := range strings.Fields(patterns) {
					matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), strings.TrimPrefix(pattern, "all:")))
					if err != nil {
						return err
					}
					for _, m := range matches {
						if err := filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
							switch {
							case err != nil:
								return err
							case d.IsDir() && d.Name() == "testdata":
								return filepath.SkipDir
							case !d.IsDir():
								embedded = append(embedded, p)
							}
							return nil
						}); err != nil {
							return err
						}
					}
				}
			}
		case strings.HasSuffix(name, ".sql"):
			migrations++
		case strings.Contains(filepath.ToSlash(path), "api/signin/"):
			pageFiles++
		}
		return nil
	}
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
		for _, suffix := range []string{".js", ".html", ".css", ".sh", ".sql", ".Dockerfile"} {
			shipped = shipped || strings.HasSuffix(name, suffix)
		}
		if !shipped && name != "go.mod" && name != "Dockerfile" {
			return nil
		}
		return check(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range embedded {
		if err := check(path); err != nil {
			t.Fatal(err)
		}
	}
	// A walk that read nothing would pass by looking at no code at all, one that missed the page
	// or the migrations would pass without reading what a browser runs or a database is given, and
	// one that followed no //go:embed would pass without reading what the binaries carry.
	if goFiles < 150 || pageFiles < 5 || migrations < 30 || len(embedded) < migrations+pageFiles {
		t.Fatalf("read %d files, %d of them Go, %d of the sign-in page's and %d migrations, %d embedded, and the module ships more than that", len(read), goFiles, pageFiles, migrations, len(embedded))
	}
}
