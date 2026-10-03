package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

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
