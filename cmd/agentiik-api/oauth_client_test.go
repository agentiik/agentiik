package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// anMCPClient publishes an MCP client's Client ID Metadata Document, as a client does at the https
// URL that is its client_id, naming the redirect URIs given, and answers that client_id and what the
// installation fetches it with. The server is the test's own on the loopback, which the guarded fetch
// an installation uses refuses, so the fetch answered reaches it alone, trusting its certificate.
func anMCPClient(t *testing.T, name string, redirects ...string) (clientID string, fetch *http.Client) {
	t.Helper()
	var id string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/client.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=60")
		json.NewEncoder(w).Encode(map[string]any{
			"client_id": id, "client_name": name, "redirect_uris": redirects,
			"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
			"token_endpoint_auth_method": "none",
		})
	}))
	t.Cleanup(server.Close)
	id = server.URL + "/oauth/client.json"
	return id, server.Client()
}
