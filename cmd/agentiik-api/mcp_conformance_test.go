package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/internal/webauthn/webauthntest"
	"github.com/agentiik/agentiik/mcp"
)

// "Run the user's server and a collection against a conformance client: server/discover,
// tools/list, tools/call and resources/read, over HTTP, each request standing alone." The client is
// written here from the revision's rules rather than from package mcp, so that it does not agree
// with the server by sharing its code: it speaks HTTP to the installation as serve answers it, with
// a fresh connection and no cookie for each request, and holds every answer to what the revision
// asks of it.

// conformant is a client of one endpoint: its URL and the token it presents.
type conformant struct {
	t     *testing.T
	url   string
	token string
	id    int
}

// answer is one exchange as the client saw it.
type answer struct {
	status int
	header http.Header
	result map[string]any
	err    map[string]any
}

// send makes one request standing alone: its own connection, the revision's headers, and the
// version and the client's capabilities in its own _meta, and reads the answer back as JSON-RPC.
func (c *conformant) send(method string, params map[string]any) answer {
	c.t.Helper()
	c.id++
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	r, _ := http.NewRequestWithContext(c.t.Context(), "POST", c.url, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", method)
	for _, field := range []string{"name", "uri"} {
		if v, ok := params[field].(string); ok {
			r.Header.Set("Mcp-Name", v)
		}
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	w, err := client.Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	defer w.Body.Close()
	raw, _ := io.ReadAll(w.Body)
	var out struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  map[string]any  `json:"result"`
		Error   map[string]any  `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%s at %s answered %d %q, which is no JSON-RPC message", method, c.url, w.StatusCode, raw)
	}
	if out.JSONRPC != "2.0" || string(out.ID) != strings.TrimSpace(string(must(json.Marshal(c.id)))) {
		c.t.Errorf("%s at %s answered jsonrpc %q and id %s to the request with id %d", method, c.url, out.JSONRPC, out.ID, c.id)
	}
	if (out.Result == nil) == (out.Error == nil) {
		c.t.Errorf("%s at %s answered a result and an error, or neither: %s", method, c.url, raw)
	}
	if ct := w.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		c.t.Errorf("%s at %s answered Content-Type %q", method, c.url, ct)
	}
	// The revision keeps no session: an answer that hands one out asks the client for state the
	// server does not keep.
	if w.Header.Get("Mcp-Session-Id") != "" || len(w.Header.Values("Set-Cookie")) > 0 {
		c.t.Errorf("%s at %s handed out a session: %v", method, c.url, w.Header)
	}
	if out.Result != nil {
		if out.Result["resultType"] != "complete" {
			c.t.Errorf("%s at %s answered resultType %v", method, c.url, out.Result["resultType"])
		}
		info, _ := out.Result["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)
		if info["name"] == "" || info["version"] == nil {
			c.t.Errorf("%s at %s names no server in its _meta: %v", method, c.url, out.Result["_meta"])
		}
	}
	return answer{status: w.StatusCode, header: w.Header, result: out.Result, err: out.Error}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// cacheable holds a discovery, a list or a read to the caching hints the revision requires.
func (c *conformant) cacheable(method string, a answer) {
	c.t.Helper()
	if _, ok := a.result["ttlMs"].(float64); !ok || a.result["cacheScope"] == nil {
		c.t.Errorf("%s at %s carries no caching hints: %v", method, c.url, a.result)
	}
}

// toolName is the protocol's advice on a tool's name: one to 128 of letters, digits, _, - and .
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// listed holds tools/list to what a client relies on, and answers the tools by name.
func (c *conformant) listed() map[string]map[string]any {
	c.t.Helper()
	first := c.send("tools/list", nil)
	if first.status != http.StatusOK || first.err != nil {
		c.t.Fatalf("tools/list at %s answered %d %v", c.url, first.status, first.err)
	}
	c.cacheable("tools/list", first)
	again := c.send("tools/list", nil)
	if a, b := must(json.Marshal(first.result["tools"])), must(json.Marshal(again.result["tools"])); !bytes.Equal(a, b) {
		c.t.Errorf("tools/list at %s answered two lists to two requests alike", c.url)
	}
	byName := map[string]map[string]any{}
	for _, raw := range first.result["tools"].([]any) {
		tool := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if !toolName.MatchString(name) || byName[name] != nil {
			c.t.Errorf("tools/list at %s names a tool %q, off the grammar or twice", c.url, name)
		}
		byName[name] = tool
		if d, _ := tool["description"].(string); d == "" {
			c.t.Errorf("%s at %s carries no description", name, c.url)
		}
		for _, field := range []string{"inputSchema", "outputSchema"} {
			schema, present := tool[field].(map[string]any)
			if !present {
				if field == "inputSchema" {
					c.t.Errorf("%s at %s carries no inputSchema", name, c.url)
				}
				continue
			}
			if schema["type"] != "object" {
				c.t.Errorf("%s's %s at %s is not an object schema: %v", name, field, c.url, schema)
			}
			if _, err := compiled(schema); err != nil {
				c.t.Errorf("%s's %s at %s does not compile as JSON Schema 2020-12: %v", name, field, c.url, err)
			}
		}
		if hints, ok := tool["annotations"].(map[string]any); ok {
			for hint, v := range hints {
				if _, ok := v.(bool); !ok || !strings.HasSuffix(hint, "Hint") {
					c.t.Errorf("%s at %s carries the annotation %s: %v", name, c.url, hint, v)
				}
			}
		}
	}
	return byName
}

// compiled is a schema a tool publishes, compiled as a client validating against it would.
func compiled(schema map[string]any) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(must(json.Marshal(schema))))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("tool.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("tool.json")
}

// called holds a call's result to the revision: content blocks, and structured content conforming
// to the outputSchema a tool publishes, where it publishes one and the call did not fail.
func (c *conformant) called(tool map[string]any, arguments map[string]any) answer {
	c.t.Helper()
	name := tool["name"].(string)
	if s, err := compiled(tool["inputSchema"].(map[string]any)); err == nil {
		v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(must(json.Marshal(arguments))))
		if err := s.Validate(v); err != nil {
			c.t.Fatalf("the test's own arguments to %s do not fit its inputSchema: %v", name, err)
		}
	}
	a := c.send("tools/call", map[string]any{"name": name, "arguments": arguments})
	if a.status != http.StatusOK || a.err != nil {
		c.t.Fatalf("%s at %s answered %d %v", name, c.url, a.status, a.err)
	}
	content, _ := a.result["content"].([]any)
	for _, block := range content {
		if b := block.(map[string]any); b["type"] != "text" || b["text"] == nil {
			c.t.Errorf("%s at %s answered the content block %v", name, c.url, b)
		}
	}
	if output, ok := tool["outputSchema"].(map[string]any); ok && a.result["isError"] != true {
		s, err := compiled(output)
		if err != nil {
			c.t.Fatal(err)
		}
		structured, present := a.result["structuredContent"]
		v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(must(json.Marshal(structured))))
		if !present || s.Validate(v) != nil {
			c.t.Errorf("%s at %s answered structured content its outputSchema refuses: %v", name, c.url, structured)
		}
	}
	return a
}

// The user's server and a collection, each held to the revision by a client that speaks HTTP to the
// installation serve builds, request by request.
func TestBothServersSpeakTheRevisionToAConformanceClient(t *testing.T) {
	x := someTenants(t)
	push := aPushOf(t, payrollPublished)
	push.Parent = theCommit
	x.must("PUT", "/api/v1/finance/workflows/payroll/versions/"+payrollCommit, x.as["alice"], push, http.StatusOK)
	server := httptest.NewServer(x.in.router)
	t.Cleanup(server.Close)
	user := &conformant{t: t, url: server.URL + "/mcp", token: x.as["alice"].bearer}

	// server/discover, before anything else is known.
	discovered := user.send("server/discover", nil)
	if discovered.status != http.StatusOK || discovered.err != nil {
		t.Fatalf("server/discover answered %d %v", discovered.status, discovered.err)
	}
	user.cacheable("server/discover", discovered)
	versions, _ := discovered.result["supportedVersions"].([]any)
	capabilities, _ := discovered.result["capabilities"].(map[string]any)
	if len(versions) == 0 || versions[0] != "2026-07-28" || capabilities["tools"] == nil || capabilities["resources"] == nil {
		t.Errorf("the user's server discovers as %v", discovered.result)
	}

	// tools/list, and a call of the tool every client reads first.
	tools := user.listed()
	language := user.called(tools["workflow.language"], map[string]any{})
	if language.result["isError"] == true || len(language.result["content"].([]any)) == 0 {
		t.Errorf("workflow.language answered %v", language.result)
	}

	// resources/read of what resources/list names.
	listed := user.send("resources/list", nil)
	user.cacheable("resources/list", listed)
	first := listed.result["resources"].([]any)[0].(map[string]any)
	read := user.send("resources/read", map[string]any{"uri": first["uri"]})
	user.cacheable("resources/read", read)
	contents, _ := read.result["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["uri"] != first["uri"] || contents[0].(map[string]any)["text"] == nil {
		t.Errorf("resources/read of %v answered %v", first["uri"], read.result)
	}

	// A collection, made with the user's server and called at its own address.
	made := user.called(tools["collection.create"], map[string]any{"name": "payroll-desk"})
	id := made.result["structuredContent"].(map[string]any)["id"].(string)
	user.called(tools["collection.add"], map[string]any{"collection": id, "namespace": "finance", "workflow": "payroll"})
	collection := &conformant{t: t, url: server.URL + "/mcp/collections/" + id, token: x.as["alice"].bearer}
	discovered = collection.send("server/discover", nil)
	capabilities, _ = discovered.result["capabilities"].(map[string]any)
	if discovered.err != nil || capabilities["tools"] == nil || capabilities["resources"] != nil {
		t.Errorf("the collection discovers as %v %v", discovered.result, discovered.err)
	}
	offered := collection.listed()
	if len(offered) != 1 || offered["run_payroll"] == nil {
		t.Fatalf("the collection lists %v", offered)
	}
	if run := collection.called(offered["run_payroll"], map[string]any{"orders": []any{}}); run.result["isError"] == true {
		t.Errorf("run_payroll answered %v", run.result)
	}
	if a := collection.send("resources/read", map[string]any{"uri": "agentiik://language/steps"}); a.status != http.StatusNotFound || a.err["code"] != float64(mcp.CodeMethodNotFound) {
		t.Errorf("resources/read on a collection answered %d %v", a.status, a.err)
	}

	// Each request stands alone: no stream to open, no session to end, and a version the server
	// does not speak told which it does.
	for _, at := range []string{user.url, collection.url} {
		for _, method := range []string{"GET", "DELETE"} {
			r, _ := http.NewRequestWithContext(t.Context(), method, at, nil)
			r.Header.Set("Authorization", "Bearer "+x.as["alice"].bearer)
			r.Header.Set("Accept", "text/event-stream")
			w, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			w.Body.Close()
			if w.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s answered %d", method, at, w.StatusCode)
			}
		}
		r, _ := http.NewRequestWithContext(t.Context(), "POST", at, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18","io.modelcontextprotocol/clientCapabilities":{}}}}`))
		r.Header.Set("Authorization", "Bearer "+x.as["alice"].bearer)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("MCP-Protocol-Version", "2025-06-18")
		r.Header.Set("Mcp-Method", "server/discover")
		w, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var refused struct {
			Error struct {
				Code int `json:"code"`
				Data struct {
					Supported []string `json:"supported"`
				} `json:"data"`
			} `json:"error"`
		}
		json.NewDecoder(w.Body).Decode(&refused)
		w.Body.Close()
		if w.StatusCode != http.StatusBadRequest || refused.Error.Code != -32022 || len(refused.Error.Data.Supported) != 1 || refused.Error.Data.Supported[0] != "2026-07-28" {
			t.Errorf("an older revision at %s answered %d %+v", at, w.StatusCode, refused)
		}
	}
}

// "Act as an OAuth 2.1 resource server and as its own authorisation server": a client that knows the
// URL of /mcp and nothing else is held to the revision's authorisation from its first request, and
// holds the installation to it, over HTTP against what serve builds. It is told where to ask by the
// 401, reads the Protected Resource Metadata and the Authorization Server Metadata the revision
// names, is registered by nothing but its own Client ID Metadata Document, has its person allow it
// by a sign-in carrying its PKCE challenge on the sign-in page, trades the code with its verifier,
// lists the tools with the token, and refreshes it. What the revision refuses is refused as the page
// says: a code presented with another verifier or a second time, a token at another resource than
// its audience, one revoked or past its hour, a refresh token presented after it was spent, which
// ends the grant, and a call needing a scope the token was not allowed, which is told the scope.
//
// The installation's public URL is https://agentiik.example.com, and every address it writes is one
// under it, which the client asks at the test's server in its place, as a client reaching the
// installation asks it at its own host.
func TestAClientIsAllowedAndHeldToTheRevisionsAuthorisation(t *testing.T) {
	const redirect = "http://127.0.0.1:4711/callback"
	clientID, fetch := anMCPClient(t, "Conformance client", redirect)
	x := someTenantsWith(t, func(s *settings) { s.clients = fetch })
	server := httptest.NewServer(x.in.router)
	t.Cleanup(server.Close)
	at := func(address string) string {
		if !strings.HasPrefix(address, tenantsOrigin) {
			t.Fatalf("the installation wrote %q, which is not under its public URL", address)
		}
		return server.URL + strings.TrimPrefix(address, tenantsOrigin)
	}
	do := func(r *http.Request) (*http.Response, []byte) {
		t.Helper()
		w, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Body.Close()
		raw, _ := io.ReadAll(w.Body)
		return w, raw
	}
	object := func(raw []byte) map[string]any {
		t.Helper()
		var o map[string]any
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("%q is no JSON object", raw)
		}
		return o
	}
	challenged := regexp.MustCompile(`(\w+)="([^"]*)"`)
	params := func(header string) map[string]string {
		out := map[string]string{}
		if !strings.HasPrefix(header, "Bearer ") {
			t.Errorf("WWW-Authenticate is %q, no Bearer challenge", header)
		}
		for _, m := range challenged.FindAllStringSubmatch(header, -1) {
			out[m[1]] = m[2]
		}
		return out
	}

	// The first request carries no token, and is told where to ask for one: the metadata of the URL
	// it called, the scopes the resource uses, and no error, since no credential came.
	discover := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	r, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", strings.NewReader(discover))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Mcp-Method", "server/discover")
	w, _ := do(r)
	challenge := params(w.Header.Get("WWW-Authenticate"))
	if w.StatusCode != http.StatusUnauthorized || challenge["resource_metadata"] != tenantsOrigin+"/.well-known/oauth-protected-resource/mcp" || challenge["error"] != "" {
		t.Fatalf("/mcp with no token answered %d and the challenge %v", w.StatusCode, challenge)
	}
	if scopes := strings.Fields(challenge["scope"]); len(scopes) != 9 || !slices.Contains(scopes, "workflow:write") || slices.Contains(scopes, "offline_access") {
		t.Errorf("the challenge asks for the scopes %v", scopes)
	}

	// RFC 9728: the resource is the URL called, and names its authorisation server.
	r, _ = http.NewRequestWithContext(t.Context(), "GET", at(challenge["resource_metadata"]), nil)
	w, raw := do(r)
	resource := object(raw)
	servers, _ := resource["authorization_servers"].([]any)
	if w.StatusCode != http.StatusOK || resource["resource"] != tenantsOrigin+"/mcp" || len(servers) != 1 || servers[0] != tenantsOrigin {
		t.Fatalf("the resource's metadata answered %d %v", w.StatusCode, resource)
	}
	// RFC 8414: the issuer the metadata names, at the address its suffix makes, holding PKCE's S256,
	// iss and Client ID Metadata Documents, and no registration.
	issuer := servers[0].(string)
	r, _ = http.NewRequestWithContext(t.Context(), "GET", at(issuer+"/.well-known/oauth-authorization-server"), nil)
	w, raw = do(r)
	metadata := object(raw)
	methods, _ := metadata["code_challenge_methods_supported"].([]any)
	if w.StatusCode != http.StatusOK || metadata["issuer"] != issuer || !slices.Contains(methods, any("S256")) || len(methods) != 1 ||
		metadata["client_id_metadata_document_supported"] != true || metadata["authorization_response_iss_parameter_supported"] != true ||
		metadata["registration_endpoint"] != nil {
		t.Fatalf("the authorisation server's metadata answered %d %v", w.StatusCode, metadata)
	}
	authorize, tokens := metadata["authorization_endpoint"].(string), metadata["token_endpoint"].(string)

	// alice registers a passkey from a session her password opens, as she would on the enrolment
	// page, which is what she allows a client with.
	keys := webauthntest.New(tenantsOrigin)
	var cookie *http.Cookie
	if err := x.in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		var err error
		cookie, err = api.OpenSession(ctx, w, "alice", api.OpenedBy{Credential: x.credential}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page := func(path string, body any, cookie *http.Cookie) (int, map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		r, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+path, bytes.NewReader(encoded))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", tenantsOrigin)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w, raw := do(r)
		return w.StatusCode, object(raw)
	}
	optionsOf := func(answer map[string]any) []byte { return must(json.Marshal(answer["options"])) }
	status, options := page("/api/v1/auth/passkey/options", map[string]any{"ceremony": "registration"}, cookie)
	made, _, err := keys.Create(optionsOf(options))
	if status != http.StatusOK || err != nil {
		t.Fatalf("a registration's options answered %d %v: %v", status, options, err)
	}
	if status, answer := page("/api/v1/auth/passkey/verify", map[string]any{"ceremony": "registration", "credential": made}, cookie); status != http.StatusOK {
		t.Fatalf("the registration answered %d %v", status, answer)
	}

	// allow opens the authorisation page as the client would send the browser there, holds it to
	// naming the client, its hosts and what it asks, and signs alice in with what the page hands on,
	// its data attributes, as the page's script does; it answers the address the browser is sent back
	// to and the client's verifier.
	attribute := regexp.MustCompile(`data-([a-z-]+)="([^"]*)"`)
	allow := func(resource, scope, state string) (*url.URL, string) {
		t.Helper()
		verifier := randomHex(t) + randomHex(t)
		sum := sha256.Sum256([]byte(verifier))
		asked := url.Values{
			"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
			"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
			"resource": {resource}, "scope": {scope}, "state": {state},
		}
		r, _ := http.NewRequestWithContext(t.Context(), "GET", at(authorize)+"?"+asked.Encode(), nil)
		w, raw := do(r)
		shown := string(raw)
		clientHost, _, _ := strings.Cut(strings.TrimPrefix(clientID, "https://"), "/")
		if w.StatusCode != http.StatusOK || !strings.Contains(shown, "Conformance client") || !strings.Contains(shown, "<code>127.0.0.1:4711</code>") ||
			!strings.Contains(shown, "<code>"+clientHost+"</code>") {
			t.Fatalf("the authorisation page answered %d:\n%s", w.StatusCode, shown)
		}
		handed := map[string]string{}
		for _, m := range attribute.FindAllStringSubmatch(shown, -1) {
			handed[m[1]] = html.UnescapeString(m[2])
		}
		authorization := map[string]string{
			"client_id": handed["client-id"], "redirect_uri": handed["redirect-uri"], "code_challenge": handed["code-challenge"],
			"resource": handed["resource"], "scope": handed["scope"], "state": handed["state"],
		}
		status, options := page("/api/v1/auth/passkey/options", map[string]any{"ceremony": "assertion"}, nil)
		asserted, err := keys.Get(optionsOf(options))
		if status != http.StatusOK || err != nil {
			t.Fatalf("an assertion's options answered %d %v: %v", status, options, err)
		}
		status, signedIn := page("/api/v1/auth/passkey/verify", map[string]any{"ceremony": "assertion", "credential": asserted, "authorization": authorization}, nil)
		back, _ := signedIn["redirect_to"].(string)
		if status != http.StatusOK || !strings.HasPrefix(back, redirect+"?") {
			t.Fatalf("the sign-in allowing the client answered %d %v", status, signedIn)
		}
		u, err := url.Parse(back)
		if err != nil {
			t.Fatal(err)
		}
		return u, verifier
	}
	trade := func(form url.Values) (int, map[string]any) {
		t.Helper()
		r, _ := http.NewRequestWithContext(t.Context(), "POST", at(tokens), strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w, raw := do(r)
		if w.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("the token endpoint answered Cache-Control %q", w.Header.Get("Cache-Control"))
		}
		return w.StatusCode, object(raw)
	}
	redeem := func(back *url.URL, verifier, resource string) (int, map[string]any) {
		return trade(url.Values{
			"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "code_verifier": {verifier},
			"client_id": {clientID}, "redirect_uri": {redirect}, "resource": {resource},
		})
	}

	// Allowed, the browser goes back with the code, the client's state and iss (RFC 9207), and the code
	// and the verifier answer a pair, for the scopes the challenge named.
	back, verifier := allow(tenantsOrigin+"/mcp", challenge["scope"], "state-one")
	if back.Query().Get("state") != "state-one" || back.Query().Get("iss") != issuer || !strings.HasPrefix(back.Query().Get("code"), "agkcode_") {
		t.Fatalf("the browser is sent back to %s", back)
	}
	status, pair := redeem(back, verifier, tenantsOrigin+"/mcp")
	access, _ := pair["access_token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(access, "agktoken_") || pair["token_type"] != "Bearer" || pair["expires_in"] != float64(3600) ||
		!strings.HasPrefix(pair["refresh_token"].(string), "agkrefresh_") || pair["scope"] != challenge["scope"] {
		t.Fatalf("the code answered %d %v", status, pair)
	}
	user := &conformant{t: t, url: server.URL + "/mcp", token: access}
	listed := user.listed()
	if listed["workflow.language"] == nil || listed["workflow.commit"] == nil || listed["collection.list"] != nil || listed["runner.list"] != nil {
		t.Errorf("the token lists %v", slices.Sorted(maps.Keys(listed)))
	}
	if language := user.called(listed["workflow.language"], map[string]any{}); language.result["isError"] == true {
		t.Errorf("workflow.language answered %v", language.result)
	}

	// The code is spent: presented again, with its own verifier, it opens nothing.
	if status, refused := redeem(back, verifier, tenantsOrigin+"/mcp"); status != http.StatusBadRequest || refused["error"] != "invalid_grant" {
		t.Errorf("a code presented again answered %d %v", status, refused)
	}
	// A code presented with another verifier opens nothing, and is spent by it.
	other, verifier := allow(tenantsOrigin+"/mcp", "", "")
	if status, refused := redeem(other, strings.Repeat("v", 43), ""); status != http.StatusBadRequest || refused["error"] != "invalid_grant" {
		t.Errorf("a code presented with another verifier answered %d %v", status, refused)
	}
	if status, refused := redeem(other, verifier, ""); status != http.StatusBadRequest || refused["error"] != "invalid_grant" {
		t.Errorf("a code presented after another verifier spent it answered %d %v", status, refused)
	}

	// The token is accepted at its audience alone: the API answers it as a token that opens nothing,
	// and so does a collection, telling the client the token is invalid there.
	asking := func(method, path, token string, body string) (*http.Response, []byte) {
		t.Helper()
		r, _ := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2026-07-28")
		r.Header.Set("Mcp-Method", "server/discover")
		return do(r)
	}
	if w, raw := asking("GET", "/api/v1/me", access, ""); w.StatusCode != http.StatusUnauthorized {
		t.Errorf("the token for /mcp at /api/v1/me answered %d %s", w.StatusCode, raw)
	}
	w, raw = asking("POST", "/mcp/collections/"+x.collection, access, discover)
	if c := params(w.Header.Get("WWW-Authenticate")); w.StatusCode != http.StatusUnauthorized || c["error"] != "invalid_token" ||
		c["resource_metadata"] != tenantsOrigin+"/.well-known/oauth-protected-resource/mcp/collections/"+x.collection {
		t.Errorf("the token for /mcp at a collection answered %d %v %s", w.StatusCode, c, raw)
	}
	// A collection's own token reaches it, and nothing else.
	back, verifier = allow(tenantsOrigin+"/mcp/collections/"+x.collection, "workflow:run", "")
	status, ofCollection := redeem(back, verifier, "")
	if status != http.StatusOK || ofCollection["scope"] != "workflow:run" {
		t.Fatalf("the collection's code answered %d %v", status, ofCollection)
	}
	collection := &conformant{t: t, url: server.URL + "/mcp/collections/" + x.collection, token: ofCollection["access_token"].(string)}
	if a := collection.send("tools/list", nil); a.status != http.StatusOK || a.err != nil {
		t.Errorf("the collection's token at its collection answered %d %v", a.status, a.err)
	}
	if w, raw := asking("POST", "/mcp", ofCollection["access_token"].(string), discover); w.StatusCode != http.StatusUnauthorized {
		t.Errorf("a collection's token at /mcp answered %d %s", w.StatusCode, raw)
	}

	// A token allowed less than its person holds lists what its person may use and, called for a tool
	// needing what it lacks, is told the scope to ask for.
	back, verifier = allow(tenantsOrigin+"/mcp", "workflow:read run:read", "")
	status, reading := redeem(back, verifier, "")
	if status != http.StatusOK || reading["scope"] != "workflow:read run:read" {
		t.Fatalf("the narrower code answered %d %v", status, reading)
	}
	reader := &conformant{t: t, url: server.URL + "/mcp", token: reading["access_token"].(string)}
	if listed := reader.listed(); listed["workflow.commit"] == nil {
		t.Errorf("a token allowed less lists %v, and not what its person may be asked for", slices.Sorted(maps.Keys(listed)))
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"workflow.commit","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	r, _ = http.NewRequestWithContext(t.Context(), "POST", server.URL+"/mcp", strings.NewReader(call))
	r.Header.Set("Authorization", "Bearer "+reading["access_token"].(string))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", "tools/call")
	r.Header.Set("Mcp-Name", "workflow.commit")
	w, raw = do(r)
	if c := params(w.Header.Get("WWW-Authenticate")); w.StatusCode != http.StatusForbidden || c["error"] != "insufficient_scope" || c["scope"] != "workflow:write" ||
		c["resource_metadata"] != tenantsOrigin+"/.well-known/oauth-protected-resource/mcp" {
		t.Errorf("a call needing workflow:write with a token allowed less answered %d %v %s", w.StatusCode, c, raw)
	}

	// A refresh answers the next pair; the access token it replaced, and the refresh token it spent,
	// open nothing, and the second ends the grant, the new pair with it.
	status, next := trade(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pair["refresh_token"].(string)}, "client_id": {clientID}})
	if status != http.StatusOK || next["access_token"] == access || next["scope"] != challenge["scope"] {
		t.Fatalf("the refresh answered %d %v", status, next)
	}
	if w, _ := asking("POST", "/mcp", access, discover); w.StatusCode != http.StatusUnauthorized {
		t.Errorf("the access token a refresh replaced answered %d", w.StatusCode)
	}
	if a := (&conformant{t: t, url: server.URL + "/mcp", token: next["access_token"].(string)}).send("server/discover", nil); a.status != http.StatusOK {
		t.Errorf("the refreshed access token answered %d", a.status)
	}
	status, refused := trade(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pair["refresh_token"].(string)}, "client_id": {clientID}})
	if status != http.StatusBadRequest || refused["error"] != "invalid_grant" {
		t.Errorf("a refresh token presented after it was spent answered %d %v", status, refused)
	}
	if w, _ := asking("POST", "/mcp", next["access_token"].(string), discover); w.StatusCode != http.StatusUnauthorized {
		t.Errorf("the grant whose refresh token was presented twice still opens: %d", w.StatusCode)
	}

	// The grants are alice's tokens, listed with their audience and their client: the reading one,
	// revoked there, opens nothing from its next request, and the collection's, its access token
	// past its hour, opens nothing though the grant goes on.
	var listing api.TokenList
	if err := json.Unmarshal(x.must("GET", "/api/v1/auth/tokens", x.as["alice"], nil, http.StatusOK).Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	grants := map[string]api.APIToken{}
	for _, tk := range listing.Tokens {
		if tk.ClientID == clientID {
			grants[tk.Audience+" "+strings.Join(tk.Scope.Permissions, " ")] = tk
		}
	}
	readingGrant, collectionGrant := grants[tenantsOrigin+"/mcp workflow:read run:read"], grants[tenantsOrigin+"/mcp/collections/"+x.collection+" workflow:run"]
	if len(grants) != 2 || readingGrant.DeviceLabel != "Conformance client" || collectionGrant.ID == "" {
		t.Fatalf("alice's tokens list the client's grants as %+v", listing.Tokens)
	}
	x.must("DELETE", "/api/v1/auth/tokens/"+readingGrant.ID, x.as["alice"], nil, http.StatusNoContent)
	if w, _ := asking("POST", "/mcp", reading["access_token"].(string), discover); w.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked grant's access token answered %d", w.StatusCode)
	}
	if err := x.in.pool.Installation(t.Context(), db.Identity, func(ctx context.Context, w *db.Wide) error {
		held, err := w.Token(ctx, collectionGrant.ID)
		if err != nil {
			return err
		}
		return w.RotateToken(ctx, held.ID, held.Hash, held.CreatedAt.Add(time.Microsecond), held.Permissions)
	}); err != nil {
		t.Fatal(err)
	}
	if w, raw := asking("POST", "/mcp/collections/"+x.collection, ofCollection["access_token"].(string), discover); w.StatusCode != http.StatusUnauthorized ||
		params(w.Header.Get("WWW-Authenticate"))["error"] != "invalid_token" {
		t.Errorf("an access token past its hour answered %d %s", w.StatusCode, raw)
	}
}
