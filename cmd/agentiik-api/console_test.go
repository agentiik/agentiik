package main

import (
	"bytes"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentiik/agentiik/console"
	"github.com/agentiik/agentiik/internal/config"
)

// AGK_CONSOLE chooses whether the build this binary carries is served: on or unset serves it where
// there is one, and says there is none where the build skipped the console's stage; off serves none
// whatever the binary carries; and any other word refuses the start naming the variable.
func TestAGKConsoleChoosesWhetherTheCarriedBuildIsServed(t *testing.T) {
	carried := console.Files() != nil
	for written, served := range map[string]bool{"": carried, "on": carried, "off": false} {
		env := configured(t)
		if written != "" {
			env.set(config.Console, written)
		}
		s, err := readSettings(env.lookup)
		if err != nil {
			t.Fatalf("AGK_CONSOLE=%s was refused: %s", written, err)
		}
		if (s.console != nil) != served {
			t.Errorf("AGK_CONSOLE=%s serves a console %v, and this build carries one %v", written, s.console != nil, carried)
		}
		want := map[bool]string{true: "served", false: "not carried by this build"}[carried]
		if written == "off" {
			want = "off"
		}
		if got := consoleState(s); got != want {
			t.Errorf("AGK_CONSOLE=%s starts saying the console is %q, want %q", written, got, want)
		}
	}

	env := configured(t)
	env.set(config.Console, "disabled")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"serve"}, env.lookup, io.Discard, &stderr); code != exitFailed || !strings.Contains(stderr.String(), config.Console) {
		t.Errorf("AGK_CONSOLE=disabled exited %d:\n%s", code, stderr.String())
	}
}

// Served, the console answers the root of the public URL and every address of its own, beside the
// sign-in page and the API, which answer as they do without it: off, the root is a path no route
// serves, and every route is served as before, /auth among them.
func TestServeAnswersTheConsoleBesideTheSignInPageAndTheAPI(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	natsURL := natsFrom(t, dir)
	page := `<!doctype html><html><head><base href="/"></head><body>the console</body></html>`
	build := fstest.MapFS{"index.html": {Data: []byte(page)}}

	for what, c := range map[string]struct {
		build fs.FS
		root  int
	}{"served": {build, http.StatusOK}, "off": {nil, http.StatusNotFound}} {
		s := servingSettings(t, database.Application, dir, natsURL)
		s.console = c.build
		in, err := open(t.Context(), s, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		answer := func(path string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			in.router.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", path, nil))
			return w
		}
		for _, path := range []string{"/", "/finance/monthly-invoicing/runs/7"} {
			w := answer(path)
			if w.Code != c.root || (c.root == http.StatusOK) != strings.Contains(w.Body.String(), "the console") {
				t.Errorf("with the console %s, %s answered %d %q", what, path, w.Code, w.Body)
			}
		}
		if w := answer("/auth/sign-in"); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "the console") {
			t.Errorf("with the console %s, the sign-in page answered %d", what, w.Code)
		}
		if w := answer("/api/v1/me"); w.Code != http.StatusUnauthorized {
			t.Errorf("with the console %s, the API answered %d to a request with no credential: %q", what, w.Code, w.Body)
		}
		in.close()
	}
}
