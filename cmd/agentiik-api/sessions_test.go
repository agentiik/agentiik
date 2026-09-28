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
// that left AcceptSessions out would answer a signed-in browser as nobody. A session a password
// opened where a passkey is required reaches none of its routes, the caller's own tokens among them.
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

	var session, enrolling *http.Cookie
	now := time.Now()
	err = in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		if err := w.CreateUser(ctx, db.User{Login: "carol", DisplayName: "Carol", Admin: true}); err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "carol-passkey", Login: "carol", Type: db.CredentialPasskey, PublicKey: []byte{1}, AAGUID: make([]byte, 16)}); err != nil {
			return err
		}
		session, err = api.OpenSession(ctx, w, "carol", api.OpenedBy{Credential: "carol-passkey"}, now)
		if err != nil {
			return err
		}
		if err := w.AddCredential(ctx, db.Credential{ID: "carol-password", Login: "carol", Type: db.CredentialPassword, PasswordHash: "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"}); err != nil {
			return err
		}
		enrolling, err = api.OpenSession(ctx, w, "carol", api.OpenedBy{Credential: "carol-password"}, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	carried := func(c *http.Cookie, method, path, origin, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		r.AddCookie(c)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		in.router.ServeHTTP(w, r)
		return w
	}
	asked := func(method, origin, body string) *httptest.ResponseRecorder {
		t.Helper()
		return carried(session, method, "/api/v1/namespaces", origin, body)
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
	if w := carried(session, "POST", "/api/v1/auth/tokens", "https://agentiik.example.com", `{}`); w.Code != http.StatusCreated {
		t.Errorf("a token minted by a session was answered %d: %s", w.Code, w.Body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/namespaces", ""},
		{"POST", "/api/v1/auth/tokens", `{}`},
		{"GET", "/api/v1/auth/tokens", ""},
	} {
		if w := carried(enrolling, c.method, c.path, "https://agentiik.example.com", c.body); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "enrols passkeys and nothing else") {
			t.Errorf("%s %s answered a session that may only enrol %d: %s", c.method, c.path, w.Code, w.Body)
		}
	}
}
