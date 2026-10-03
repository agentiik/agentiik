package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/config"
)

// AGK_MCP chooses whether the MCP endpoints are served: on or unset serves them, off serves none, and
// any other word refuses the start naming the variable; the line the start logs says which.
func TestAGKMCPChoosesWhetherTheEndpointsAreServed(t *testing.T) {
	for written, served := range map[string]bool{"": true, "on": true, "off": false} {
		env := configured(t)
		if written != "" {
			env.set(config.MCP, written)
		}
		s, err := readSettings(env.lookup)
		if err != nil {
			t.Fatalf("AGK_MCP=%s was refused: %s", written, err)
		}
		if s.MCP != served {
			t.Errorf("AGK_MCP=%s serves MCP %v", written, s.MCP)
		}
		if want := map[bool]string{true: "served", false: "off"}[served]; mcpState(s) != want {
			t.Errorf("AGK_MCP=%s starts saying MCP is %q, want %q", written, mcpState(s), want)
		}
	}

	env := configured(t)
	env.set(config.MCP, "disabled")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"serve"}, env.lookup, io.Discard, &stderr); code != exitFailed || !strings.Contains(stderr.String(), config.MCP) {
		t.Errorf("AGK_MCP=disabled exited %d:\n%s", code, stderr.String())
	}
}

// Served, /mcp is a route of the API from this process, which asks for a bearer token before
// anything else; off, it answers 404 as a route the installation does not serve does, whatever the
// request carries, and the console, which answers every other path, never takes it.
func TestServeAnswersMCPUnlessItIsOff(t *testing.T) {
	database := freshDatabase(t)
	if err := migrate(t.Context(), database, io.Discard); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bus")
	if code := run(t.Context(), []string{"bus-init", dir}, empty, io.Discard, io.Discard); code != exitStopped {
		t.Fatal("bus-init failed")
	}
	natsURL := natsFrom(t, dir)

	for served, want := range map[bool]int{true: http.StatusUnauthorized, false: http.StatusNotFound} {
		s := servingSettings(t, database.Application, dir, natsURL)
		s.MCP = served
		in, err := open(t.Context(), s, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`))
		w := httptest.NewRecorder()
		in.router.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("with MCP served %v, /mcp answered %d %q", served, w.Code, w.Body)
		}
		in.close()
	}
}
