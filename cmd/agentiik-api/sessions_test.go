package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
)

// The API serve builds reads the console's session beside the bearer token, and accepts a request
// changing something that a session carries from the public URL's origin alone: an installation
// that left AcceptSessions out would answer a signed-in browser as nobody.
func TestServeAcceptsSessionsFromThePublicURL(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	in, err := open(t.Context(), servingSettings(t, database.Application, dir, natsFrom(t, dir)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()

	var session *http.Cookie
	err = in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "carol", DisplayName: "Carol", Admin: true}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "carol-passkey", Login: "carol", Type: db.CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
			return err
		}
		session, err = api.OpenSession(ctx, w, "carol", api.OpenedBy{Credential: "carol-passkey"}, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	asked := func(method, origin, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/namespaces", strings.NewReader(body))
		r.AddCookie(session)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		in.router.ServeHTTP(w, r)
		return w
	}

	if w := asked("GET", "", ""); w.Code != http.StatusOK {
		t.Errorf("the namespaces were answered to a session %d: %s", w.Code, w.Body)
	}
	if w := asked("POST", "https://evil.example.com", `{"name": "ops", "owner": "carol"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "public URL") {
		t.Errorf("a namespace created from another origin was answered %d: %s", w.Code, w.Body)
	}
	if w := asked("POST", "https://agentiik.example.com", `{"name": "ops", "owner": "carol"}`); w.Code != http.StatusCreated {
		t.Errorf("a namespace created from the public URL was answered %d: %s", w.Code, w.Body)
	}
}
