package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The rules of the installation's own authorisation server that hold without a database: what a
// client_id, a client's document and its redirect URIs are held to, how long a document is kept,
// where none is fetched from, and the addresses and challenges the server writes.

// "A Client Identifier URL MUST use the https URL scheme, MUST NOT contain a userinfo component, MUST
// contain a path component, MUST NOT contain single-dot or double-dot path components, MUST NOT
// contain a fragment component", and here contains no query and is no address of the installation's.
func TestAClientIDIsAnHTTPSURLWithAPathOfSomebodyElses(t *testing.T) {
	s := &OAuth{origin: "https://agentiik.example.com"}
	for _, id := range []string{"https://client.example.com/oauth.json", "https://client.example.com:8443/a/b", "https://192.0.2.10/client"} {
		if err := s.clientIDHolds(id); err != nil {
			t.Errorf("%s was refused: %v", id, err)
		}
	}
	for _, id := range []string{
		"http://client.example.com/oauth.json", "https://client.example.com", "https://client.example.com/",
		"https://user@client.example.com/oauth.json", "https://client.example.com/oauth.json?v=1",
		"https://client.example.com/oauth.json#x", "https://client.example.com/a/../oauth.json",
		"https://client.example.com/a/%2e%2e/oauth.json", "https://client.example.com/./oauth.json",
		"https://agentiik.example.com/client.json", "urn:client", "not a url at all", "https:client.example.com/x",
	} {
		if err := s.clientIDHolds(id); err == nil {
			t.Errorf("%s was taken as a client_id", id)
		}
	}
}

// A document holds client_id to the character, a client_name of one line, and redirect URIs that are
// https or the loopback; it authenticates no client; and a grant type or a response type it names
// includes the code flow.
func TestAClientsDocumentHoldsWhatTheRevisionAsksOfOne(t *testing.T) {
	const id = "https://client.example.com/oauth.json"
	doc := func(extra string) []byte {
		return []byte(`{"client_id":"` + id + `","client_name":"Example","redirect_uris":["https://client.example.com/cb","http://127.0.0.1:3000/cb"]` + extra + `}`)
	}
	got, err := readDocument(doc(`,"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"]`), id)
	if err != nil || got.Name != "Example" || len(got.RedirectURIs) != 2 {
		t.Fatalf("a document that holds was read as %+v: %v", got, err)
	}
	for name, raw := range map[string][]byte{
		"another client_id":       []byte(`{"client_id":"https://client.example.com/other.json","client_name":"Example","redirect_uris":["https://client.example.com/cb"]}`),
		"no client_name":          []byte(`{"client_id":"` + id + `","redirect_uris":["https://client.example.com/cb"]}`),
		"a blank client_name":     []byte(`{"client_id":"` + id + `","client_name":"  ","redirect_uris":["https://client.example.com/cb"]}`),
		"a name of two lines":     []byte(`{"client_id":"` + id + `","client_name":"Ex\nample","redirect_uris":["https://client.example.com/cb"]}`),
		"a name too long":         []byte(`{"client_id":"` + id + `","client_name":"` + strings.Repeat("n", 257) + `","redirect_uris":["https://client.example.com/cb"]}`),
		"no redirect_uris":        []byte(`{"client_id":"` + id + `","client_name":"Example","redirect_uris":[]}`),
		"a plain http redirect":   []byte(`{"client_id":"` + id + `","client_name":"Example","redirect_uris":["http://client.example.com/cb"]}`),
		"a private-use scheme":    []byte(`{"client_id":"` + id + `","client_name":"Example","redirect_uris":["com.example.app:/cb"]}`),
		"a redirect's fragment":   []byte(`{"client_id":"` + id + `","client_name":"Example","redirect_uris":["https://client.example.com/cb#x"]}`),
		"a secret authentication": doc(`,"token_endpoint_auth_method":"client_secret_basic"`),
		"a key authentication":    doc(`,"token_endpoint_auth_method":"private_key_jwt"`),
		"no code grant":           doc(`,"grant_types":["client_credentials"]`),
		"no code response":        doc(`,"response_types":["token"]`),
		"no object":               []byte(`["` + id + `"]`),
	} {
		if _, err := readDocument(raw, id); err == nil {
			t.Errorf("a document with %s was taken", name)
		}
	}
}

// "The authorization server MUST allow variable port numbers" for a loopback redirect URI, and
// compares every other one to the character.
func TestALoopbackRedirectURIMayNameAnotherPort(t *testing.T) {
	d := clientDocument{RedirectURIs: []string{"http://127.0.0.1:3000/callback", "http://localhost/cb", "https://client.example.com/cb"}}
	for _, asked := range []string{"http://127.0.0.1:3000/callback", "http://127.0.0.1:51004/callback", "http://127.0.0.1/callback", "http://localhost:8123/cb", "https://client.example.com/cb"} {
		if !d.redirects(asked) {
			t.Errorf("%s was refused", asked)
		}
	}
	for _, asked := range []string{"http://127.0.0.1:3000/other", "http://[::1]:3000/callback", "https://client.example.com:8443/cb", "https://client.example.com/cb/", "https://Client.example.com/cb", "http://127.0.0.1:3000/callback?x"} {
		if d.redirects(asked) {
			t.Errorf("%s was taken as one of the document's", asked)
		}
	}
}

// A document is kept as long as its Cache-Control says, within an hour, five minutes where it says
// nothing, and not at all where it says no-store or no-cache.
func TestADocumentIsKeptAsItsAnswerSays(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"": documentKeptLeast, "max-age=60": time.Minute, "public, max-age=86400": documentKeptMost,
		"no-store": 0, "max-age=600, no-cache": 0, "max-age=nonsense": documentKeptLeast, "max-age=0": 0,
	} {
		if got := keptFor(header); got != want {
			t.Errorf("Cache-Control %q keeps a document %s, want %s", header, got, want)
		}
	}
}

// "Authorization servers MUST NOT fetch a Client ID Metadata Document URL ... that resolve to
// special-use IP addresses": every range of RFC 6890, an IPv4 address written as IPv6 among them,
// and no address of the global unicast space.
func TestNoDocumentIsFetchedFromASpecialUseAddress(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"192.0.2.1", "198.18.0.1", "224.0.0.1", "255.255.255.255", "::1", "::", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "2001:db8::1", "2002::1"} {
		if !specialUse(netip.MustParseAddr(a)) {
			t.Errorf("%s is taken as an address a document may be fetched from", a)
		}
	}
	for _, a := range []string{"93.184.215.14", "8.8.8.8", "2606:4700::1111", "2a00:1450:4007::1"} {
		if specialUse(netip.MustParseAddr(a)) {
			t.Errorf("%s is refused as special-use", a)
		}
	}
	// The guarded fetch dials nothing at the loopback, where a test's own server listens.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) }))
	defer server.Close()
	_, err := guardedFetch().Get(server.URL + "/client.json")
	if !errors.Is(err, errSpecialUse) {
		t.Errorf("the guarded fetch reached the loopback: %v", err)
	}
	documents := newClientDocuments(guardedFetch(), time.Now)
	if _, err := documents.get(t.Context(), server.URL+"/client.json"); err == nil || !strings.Contains(err.Error(), "special-use") {
		t.Errorf("a document on the loopback was answered %v", err)
	}
}

// A document is fetched following no redirect, at most documentMost bytes of it read, and kept only
// once it holds.
func TestADocumentIsFetchedWithinItsBoundsAndKeptOnlyWhenItHolds(t *testing.T) {
	fetched := 0
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched++
		switch r.URL.Path {
		case "/moved":
			http.Redirect(w, r, "/client.json", http.StatusFound)
		case "/long":
			w.Write([]byte(`{"client_id":"` + server.URL + `/long","client_name":"` + strings.Repeat("x", documentMost) + `"}`))
		case "/broken":
			w.Write([]byte(`{"client_id":"` + server.URL + `/broken"}`))
		case "/client.json":
			if r.Header.Get("Accept") != "application/json" {
				t.Errorf("the document was asked for with Accept %q", r.Header.Get("Accept"))
			}
			w.Write([]byte(`{"client_id":"` + server.URL + `/client.json","client_name":"Kept","redirect_uris":["http://127.0.0.1/cb"]}`))
		}
	}))
	defer server.Close()
	fetch := server.Client()
	fetch.CheckRedirect = guardedFetch().CheckRedirect
	documents := newClientDocuments(fetch, time.Now)
	for _, path := range []string{"/moved", "/long", "/broken"} {
		if _, err := documents.get(t.Context(), server.URL+path); err == nil {
			t.Errorf("%s was read as a document", path)
		}
	}
	before := fetched
	if _, err := documents.get(t.Context(), server.URL+"/broken"); err == nil || fetched != before+1 {
		t.Errorf("a document that does not hold was kept: fetched %d times since", fetched-before)
	}
	for range 2 {
		if d, err := documents.get(t.Context(), server.URL+"/client.json"); err != nil || d.Name != "Kept" {
			t.Fatalf("the document was read as %+v: %v", d, err)
		}
	}
	if fetched != before+2 {
		t.Errorf("a document that holds was fetched %d times in two reads", fetched-before-1)
	}
	// Past fetchesMost at once, a fetch is refused rather than made.
	for range fetchesMost {
		documents.fetching <- struct{}{}
	}
	if _, err := documents.get(t.Context(), server.URL+"/broken"); err == nil || !strings.Contains(err.Error(), "at once") || fetched != before+2 {
		t.Errorf("a fetch past the most at once answered %v", err)
	}
}

// The addresses the server writes: a resource's metadata under the suffix before the public URL's
// path, an answer appended to a redirect URI keeping its query, and a challenge naming its error, its
// scopes and the metadata, with no error where no token came.
func TestTheServerWritesItsAddressesAndItsChallenges(t *testing.T) {
	s := &OAuth{issuer: "https://agentiik.example.com/console", origin: "https://agentiik.example.com", path: "/console"}
	if got := s.metadataOf(mcpPath); got != "https://agentiik.example.com/.well-known/oauth-protected-resource/console/mcp" {
		t.Errorf("/mcp's metadata is at %s", got)
	}
	if got := withQuery("https://client.example.com/cb?tenant=a", url.Values{"code": {"x"}}); got != "https://client.example.com/cb?tenant=a&code=x" {
		t.Errorf("an answer to a redirect URI with a query is %s", got)
	}
	if got := s.Challenge(mcpPath, "", nil); got != `Bearer scope="`+strings.Join(mcpScopes, " ")+`", resource_metadata="https://agentiik.example.com/.well-known/oauth-protected-resource/console/mcp"` {
		t.Errorf("/mcp challenges a request with no token as %s", got)
	}
	if got := s.Challenge(collectionsPath+"01JR8Q2W6H3V0X9K4M7N5P1T2C", "invalid_token", nil); !strings.HasPrefix(got, `Bearer error="invalid_token", scope="workflow:run", resource_metadata="https://agentiik.example.com/.well-known/oauth-protected-resource/console/mcp/collections/01JR8Q2W6H3V0X9K4M7N5P1T2C"`) {
		t.Errorf("a collection challenges an invalid token as %s", got)
	}
	r := httptest.NewRequest("POST", "/mcp/collections/01JR8Q2W6H3V0X9K4M7N5P1T2C", nil)
	if got := servedResource(r, s.issuer); got != s.issuer+"/mcp/collections/01JR8Q2W6H3V0X9K4M7N5P1T2C" {
		t.Errorf("a request to a collection is to %q", got)
	}
	for _, path := range []string{"/api/v1/me", "/mcp/collections/not-an-id", "/mcp/other", "/auth/authorize"} {
		if got := servedResource(httptest.NewRequest("GET", path, nil), s.issuer); got != "" {
			t.Errorf("a request to %s is to the resource %q", path, got)
		}
	}
	made := httptest.NewRequest("GET", "/api/v1/runs", nil)
	made = made.WithContext(forResource(made.Context(), s.issuer+mcpPath))
	if got := servedResource(made, s.issuer); got != s.issuer+mcpPath {
		t.Errorf("a request a tool makes for /mcp is to %q", got)
	}
	for v, want := range map[string]string{"workflow:read": "workflow:read", `a"b`: "a value", "é": "a value", "": "a value"} {
		if got := describable(v); got != want {
			t.Errorf("%q is described as %q", v, got)
		}
	}
	if !stateHolds("") || !stateHolds("xyz ~!") || stateHolds("a\tb") || stateHolds(strings.Repeat("s", stateMost+1)) || stateHolds("é") {
		t.Error("a state is held to another grammar than 1*VSCHAR within its bound")
	}
}
