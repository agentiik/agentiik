package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/language"
	"github.com/agentiik/agentiik/mcp"
)

// cookieOrBearer identifies a caller by a bearer token as bearer does, or by a session cookie
// naming them, which is how the console's credential looks from the router's side.
func cookieOrBearer(r *http.Request) (api.Identity, error) {
	if c, err := r.Cookie("session"); err == nil {
		return api.Identity{Principal: api.Principal(c.Value)}, nil
	}
	return bearer(r)
}

// standing is an authorizer that says what each principal is granted, as Principals does, and owns
// nothing: what the user's server lists its tools by.
type standing struct {
	owning
	grants map[api.Principal][]access.Grant
}

func (s standing) Standing(_ context.Context, who api.Principal) (api.Standing, error) {
	return api.Standing{Grants: s.grants[who], At: time.Now()}, nil
}

// platform is a router serving the user's MCP server on the public URL, where alice is an editor of
// finance and nobody else holds anything.
func platform(t *testing.T) *api.Router {
	t.Helper()
	editor := access.Grant{Principal: "alice", Scope: access.Scope{Namespace: "finance"}, Role: access.Editor}
	rt, err := api.NewRouter(standing{grants: map[api.Principal][]access.Grant{"alice": {editor}}}, cookieOrBearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com", Version: "v0.7.0"}); err != nil {
		t.Fatal(err)
	}
	return rt
}

// called sends one request of the revision to /mcp, as the principal a bearer token names, and reads
// the answer back.
func called(t *testing.T, rt *api.Router, as, method string, params map[string]any) (int, map[string]any, *mcp.Error) {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    mcp.Revision,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("MCP-Protocol-Version", mcp.Revision)
	r.Header.Set("Mcp-Method", method)
	for _, field := range []string{"name", "uri"} {
		if v, ok := params[field].(string); ok {
			r.Header.Set("Mcp-Name", v)
		}
	}
	if as != "" {
		r.Header.Set("Authorization", "Bearer "+as)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	var out struct {
		Result map[string]any `json:"result"`
		Error  *mcp.Error     `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Result, out.Error
}

// The endpoint is a route of the API behind the authorisation hook: a request with no credential is
// refused before anything is read, and a session, which is the console's credential and no MCP
// client's, is answered as no credential at all.
func TestTheMCPEndpointTakesABearerToken(t *testing.T) {
	rt := platform(t)
	if code, _, _ := called(t, rt, "", "server/discover", nil); code != http.StatusUnauthorized {
		t.Errorf("no credential answered %d", code)
	}

	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`))
	r.AddCookie(&http.Cookie{Name: "session", Value: "alice"})
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("a session answered %d, WWW-Authenticate %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}

	if code, result, err := called(t, rt, "alice", "server/discover", nil); code != http.StatusOK || err != nil || result["instructions"] == "" {
		t.Errorf("a bearer token answered %d %+v %v", code, err, result)
	}

	var own bool
	for _, route := range rt.Routes() {
		if route.Method == "POST" && route.Pattern == "/mcp" {
			own = route.Own
		}
	}
	if !own {
		t.Errorf("/mcp is not registered as a route answering its caller as themselves: %+v", rt.Routes())
	}
}

// The revision has no GET stream and no session to end: /mcp answers 405 to anything but a POST,
// with or without a credential, and a browser calling from another origin is refused.
func TestTheMCPEndpointIsOnePOST(t *testing.T) {
	rt := platform(t)
	for _, method := range []string{"GET", "DELETE"} {
		for _, as := range []string{"", "alice"} {
			r := httptest.NewRequest(method, "/mcp", nil)
			if as != "" {
				r.Header.Set("Authorization", "Bearer "+as)
			}
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("a %s by %q answered %d", method, as, w.Code)
			}
		}
	}

	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer alice")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("another origin answered %d", w.Code)
	}

	if _, err := api.NewMCP(router(t, owning{}), api.MCPOptions{PublicURL: "http://agentiik.example.com"}); err == nil {
		t.Errorf("an endpoint was served on a public URL with no origin a browser can be checked against")
	}
}

// The discovery says what a workflow is, where its entry point is, and the order a client reads,
// validates and commits in, which is the field's purpose and all it carries; and it names the
// server with the program's version.
func TestTheDiscoveryOrientsAClient(t *testing.T) {
	_, result, _ := called(t, platform(t), "alice", "server/discover", nil)
	instructions, _ := result["instructions"].(string)
	for _, said := range []string{"git repository", "agentiik.yaml at the root", "workflow.language", "workflow.validate"} {
		if !strings.Contains(instructions, said) {
			t.Errorf("the instructions do not say %q: %s", said, instructions)
		}
	}
	info, _ := json.Marshal(result["_meta"])
	if string(info) != `{"io.modelcontextprotocol/serverInfo":{"name":"agentiik","version":"v0.7.0"}}` {
		t.Errorf("the server names itself %s", info)
	}
	if caps, _ := json.Marshal(result["capabilities"]); string(caps) != `{"resources":{},"tools":{}}` {
		t.Errorf("the platform server declares %s", caps)
	}
}

// offeredTo lists the tools offered to who, by name, and their hints.
func offeredTo(t *testing.T, rt *api.Router, who string) ([]string, map[string]map[string]any) {
	t.Helper()
	_, result, _ := called(t, rt, who, "tools/list", nil)
	tools, _ := result["tools"].([]any)
	var names []string
	hints := map[string]map[string]any{}
	for _, tool := range tools {
		tool := tool.(map[string]any)
		names = append(names, tool["name"].(string))
		hints[tool["name"].(string)], _ = tool["annotations"].(map[string]any)
	}
	return names, hints
}

// Any authenticated principal is offered what teaches the language and what every user may do,
// list namespaces and create one; a tool the caller cannot use anywhere is absent rather than
// refused, so that a principal who can write nowhere never sees workflow.commit.
func TestAToolTheCallerCannotUseIsAbsent(t *testing.T) {
	rt := platform(t)
	names, _ := offeredTo(t, rt, "nobody-granted-anything")
	if strings.Join(names, ",") != "workflow.language,workflow.schema,namespace.list,namespace.get,namespace.create" {
		t.Errorf("the tools offered to a principal holding nothing are %v", names)
	}
	names, _ = offeredTo(t, rt, "alice")
	for _, want := range []string{"workflow.validate", "workflow.commit", "workflow.create", "workflow.run", "run.get", "run.output", "secret.list"} {
		if !slices.Contains(names, want) {
			t.Errorf("an editor is not offered %s: %v", want, names)
		}
	}
	for _, absent := range []string{"workflow.delete", "grant.create", "namespace.quotas", "runner.list", "namespace.delete"} {
		if slices.Contains(names, absent) {
			t.Errorf("an editor is offered %s, which an editor holds nowhere", absent)
		}
	}
}

// "The read-only tools carry readOnlyHint, so a client can tell without being told", and the tools
// that remove something carry destructiveHint.
func TestEachToolSaysWhetherItReadsOrRemoves(t *testing.T) {
	rt, err := api.NewRouter(owning{}, cookieOrBearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	// An authorizer that says nothing of what anybody holds lists every tool.
	names, hints := offeredTo(t, rt, "anybody")
	readOnly := []string{"workflow.language", "workflow.schema", "workflow.validate", "namespace.list", "namespace.get", "grant.list", "secret.list", "workflow.list", "workflow.get", "run.list", "run.get", "run.logs", "run.output", "runner.list"}
	destructive := []string{"namespace.delete", "grant.revoke", "workflow.delete", "secret.remove"}
	for _, name := range names {
		h := hints[name]
		switch {
		case slices.Contains(readOnly, name):
			if h["readOnlyHint"] != true || h["destructiveHint"] != false {
				t.Errorf("%s reads and is not marked read-only: %v", name, h)
			}
		case slices.Contains(destructive, name):
			if h["readOnlyHint"] != false || h["destructiveHint"] != true {
				t.Errorf("%s removes something and is not marked destructive: %v", name, h)
			}
		default:
			if h["readOnlyHint"] != false || h["destructiveHint"] != false {
				t.Errorf("%s writes and is marked %v", name, h)
			}
		}
	}
	if len(names) < 29 {
		t.Errorf("an authorizer that says nothing lists %d tools: %v", len(names), names)
	}
}

// workflow.language answers a topic with its page and no topic with the orientation, each as
// agentiik/schemas generated it, and refuses a topic that is none by naming the ones that are.
func TestWorkflowLanguageTeachesATopicAtATime(t *testing.T) {
	rt := platform(t)
	text := func(result map[string]any) string {
		content, _ := result["content"].([]any)
		if len(content) != 1 {
			return ""
		}
		return content[0].(map[string]any)["text"].(string)
	}
	if _, result, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.language"}); err != nil || text(result) != language.Orientation() {
		t.Errorf("no topic answered %v %q", err, text(result))
	}
	page, _ := language.Page("triggers")
	if _, result, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.language", "arguments": map[string]any{"topic": "triggers"}}); err != nil || text(result) != page {
		t.Errorf("the triggers topic answered %v %q", err, text(result))
	}
	_, _, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.language", "arguments": map[string]any{"topic": "loops"}})
	if err == nil || err.Code != mcp.CodeInvalidParams || !strings.Contains(err.Message, "fan_out") {
		t.Errorf("a topic that is none answered %+v", err)
	}
	if _, _, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.language", "arguments": map[string]any{"topics": "steps"}}); err == nil || err.Code != mcp.CodeInvalidParams {
		t.Errorf("a misspelt argument answered %+v", err)
	}
}

// workflow.schema answers a part's document as text and as structured content, and refuses a part
// that is none, or none named.
func TestWorkflowSchemaAnswersAPart(t *testing.T) {
	rt := platform(t)
	want, _ := language.Schema("brick")
	_, result, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.schema", "arguments": map[string]any{"part": "brick"}})
	content, _ := result["content"].([]any)
	structured, _ := result["structuredContent"].(map[string]any)
	if err != nil || len(content) != 1 || content[0].(map[string]any)["text"] != string(want) || structured["$id"] != "https://schemas.agentiik.dev/brick.schema.json" {
		t.Errorf("the brick part answered %v, %d blocks, $id %v", err, len(content), structured["$id"])
	}
	for _, arguments := range []map[string]any{{"part": "wire"}, {}, {"part": "brick", "as": "yaml"}} {
		if _, _, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.schema", "arguments": arguments}); err == nil || err.Code != mcp.CodeInvalidParams {
			t.Errorf("%v answered %+v", arguments, err)
		}
	}
}

// The language and the schemas are resources too, for a client that prefers attaching documents
// to calling a tool: every topic and every part listed, each read as the tool answers it.
func TestTheLanguageIsReadAsResources(t *testing.T) {
	rt := platform(t)
	_, result, _ := called(t, rt, "alice", "resources/list", nil)
	if resources, _ := result["resources"].([]any); len(resources) != len(language.Topics())+len(language.Parts()) {
		t.Errorf("%d resources are listed", len(resources))
	}
	_, result, _ = called(t, rt, "alice", "resources/templates/list", nil)
	if templates, _ := result["resourceTemplates"].([]any); len(templates) != 2 {
		t.Errorf("the templates are %v", templates)
	}
	page, _ := language.Page("mcp")
	_, result, err := called(t, rt, "alice", "resources/read", map[string]any{"uri": "agentiik://language/mcp"})
	contents, _ := result["contents"].([]any)
	if err != nil || len(contents) != 1 || contents[0].(map[string]any)["text"] != page {
		t.Errorf("the mcp page read %v %v", err, contents)
	}
	schema, _ := language.Schema("envelope")
	_, result, err = called(t, rt, "alice", "resources/read", map[string]any{"uri": "agentiik://schema/envelope"})
	contents, _ = result["contents"].([]any)
	if err != nil || len(contents) != 1 || contents[0].(map[string]any)["text"] != string(schema) || contents[0].(map[string]any)["mimeType"] != "application/schema+json" {
		t.Errorf("the envelope part read %v %v", err, contents)
	}
	for _, uri := range []string{"agentiik://language/loops", "agentiik://schema/wire", "agentiik://run/01J", "file:///etc/passwd"} {
		if _, _, err := called(t, rt, "alice", "resources/read", map[string]any{"uri": uri}); err == nil || err.Code != mcp.CodeInvalidParams {
			t.Errorf("%s read %+v", uri, err)
		}
	}
}
