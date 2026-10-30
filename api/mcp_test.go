package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	return calledAt(t, rt, "/mcp", as, method, params)
}

// calledAt is called at another MCP endpoint than the user's server, a collection's.
func calledAt(t *testing.T, rt *api.Router, path, as, method string, params map[string]any) (int, map[string]any, *mcp.Error) {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    mcp.Revision,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
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
	for _, said := range []string{"git repository", "agentiik.yaml at the root", "workflow_language", "workflow_validate"} {
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

// Any authenticated principal is offered what teaches the language and what every user may do:
// list namespaces and create one, and keep collections of their own, adding to one only a workflow
// they may run; a tool the caller cannot use anywhere is absent rather than refused, so that a
// principal who can write nowhere never sees workflow_commit.
func TestAToolTheCallerCannotUseIsAbsent(t *testing.T) {
	rt := platform(t)
	names, _ := offeredTo(t, rt, "nobody-granted-anything")
	if strings.Join(names, ",") != "workflow_language,workflow_schema,namespace_list,namespace_get,namespace_create,collection_list,collection_get,collection_create,collection_update,collection_delete,collection_remove" {
		t.Errorf("the tools offered to a principal holding nothing are %v", names)
	}
	names, _ = offeredTo(t, rt, "alice")
	for _, want := range []string{"workflow_validate", "workflow_commit", "workflow_create", "workflow_run", "run_get", "run_output", "secret_list"} {
		if !slices.Contains(names, want) {
			t.Errorf("an editor is not offered %s: %v", want, names)
		}
	}
	for _, absent := range []string{"workflow_delete", "grant_create", "namespace_quotas", "runner_list", "namespace_delete"} {
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
	readOnly := []string{"workflow_language", "workflow_schema", "workflow_validate", "namespace_list", "namespace_get", "grant_list", "secret_list", "workflow_list", "workflow_get", "run_list", "run_get", "run_logs", "run_output", "run_artifact", "runner_list", "collection_list", "collection_get"}
	destructive := []string{"namespace_delete", "grant_revoke", "workflow_delete", "secret_remove", "collection_delete"}
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

// Every tool is named as the Anthropic Messages API takes a tool's name, ^[a-zA-Z0-9_-]{1,64}$,
// which holds no dot. A call by the dotted name a tool went by until v0.7.2 is still answered as a
// call of the tool, unlisted, since a client calls by the names it was listed; the dotted name of a
// tool the caller is not offered is refused as any name nobody offered is, by the name called.
func TestEveryToolIsNamedAsTheMessagesAPITakesIt(t *testing.T) {
	rt, err := api.NewRouter(owning{}, cookieOrBearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	taken := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	names, _ := offeredTo(t, rt, "anybody")
	for _, name := range names {
		if !taken.MatchString(name) {
			t.Errorf("%s is a name the Messages API refuses", name)
		}
	}

	rt = platform(t)
	page, _ := language.Page("triggers")
	if _, result, refusal := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow.language", "arguments": map[string]any{"topic": "triggers"}}); refusal != nil || textOf(result) != page {
		t.Errorf("a call by workflow.language answered %+v %v", refusal, result)
	}
	for _, name := range []string{"workflow.commit", "workflow.nothing", "workflow_language.x"} {
		_, _, refusal := called(t, rt, "nobody-granted-anything", "tools/call", map[string]any{"name": name})
		if refusal == nil || refusal.Code != mcp.CodeInvalidParams || refusal.Message != "Unknown tool: "+name {
			t.Errorf("a call by %s, which names no tool offered, answered %+v", name, refusal)
		}
	}
}

// secret_declare names the stores a declaration may name as its route takes them, builtin, env and
// vault, and no other: its description gave one no release defines, which a model reading it tried
// and was refused. A store off the list is the protocol's error, before anything is asked.
func TestSecretDeclareNamesTheStoresItsRouteTakes(t *testing.T) {
	rt, err := api.NewRouter(owning{}, cookieOrBearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	_, result, _ := called(t, rt, "alice", "tools/list", nil)
	var provider map[string]any
	for _, tool := range result["tools"].([]any) {
		if tool := tool.(map[string]any); tool["name"] == "secret_declare" {
			provider, _ = tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["provider"].(map[string]any)
		}
	}
	enum, _ := provider["enum"].([]any)
	if !slices.Equal(enum, []any{api.ProviderBuiltin, api.ProviderEnv, api.ProviderVault}) {
		t.Errorf("secret_declare's provider is %v", provider)
	}
	described, _ := provider["description"].(string)
	for _, store := range []string{api.ProviderBuiltin, api.ProviderEnv, api.ProviderVault} {
		if !strings.Contains(described, store) {
			t.Errorf("secret_declare's provider is described without %s: %q", store, described)
		}
	}
	if strings.Contains(strings.ToLower(described), "aws") {
		t.Errorf("secret_declare's provider is described with a store no release defines: %q", described)
	}
	_, _, refusal := called(t, rt, "alice", "tools/call", map[string]any{"name": "secret_declare", "arguments": map[string]any{
		"namespace": "finance", "name": "billing", "provider": "aws-secrets-manager", "path": "finance/billing",
	}})
	if refusal == nil || refusal.Code != mcp.CodeInvalidParams {
		t.Errorf("a store off the list was answered %v", refusal)
	}
}

// workflow_language answers a topic with its page and no topic with the orientation, each as
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
	if _, result, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow_language"}); err != nil || text(result) != language.Orientation() {
		t.Errorf("no topic answered %v %q", err, text(result))
	}
	page, _ := language.Page("triggers")
	if _, result, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow_language", "arguments": map[string]any{"topic": "triggers"}}); err != nil || text(result) != page {
		t.Errorf("the triggers topic answered %v %q", err, text(result))
	}
	_, _, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow_language", "arguments": map[string]any{"topic": "loops"}})
	if err == nil || err.Code != mcp.CodeInvalidParams || !strings.Contains(err.Message, "fan_out") {
		t.Errorf("a topic that is none answered %+v", err)
	}
	if _, _, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow_language", "arguments": map[string]any{"topics": "steps"}}); err == nil || err.Code != mcp.CodeInvalidParams {
		t.Errorf("a misspelt argument answered %+v", err)
	}
}

// workflow_schema answers a part's document as text and as structured content, and refuses a part
// that is none, or none named.
func TestWorkflowSchemaAnswersAPart(t *testing.T) {
	rt := platform(t)
	want, _ := language.Schema("brick")
	_, result, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow_schema", "arguments": map[string]any{"part": "brick"}})
	content, _ := result["content"].([]any)
	structured, _ := result["structuredContent"].(map[string]any)
	if err != nil || len(content) != 1 || content[0].(map[string]any)["text"] != string(want) || structured["$id"] != "https://schemas.agentiik.dev/brick.schema.json" {
		t.Errorf("the brick part answered %v, %d blocks, $id %v", err, len(content), structured["$id"])
	}
	for _, arguments := range []map[string]any{{"part": "wire"}, {}, {"part": "brick", "as": "yaml"}} {
		if _, _, err := called(t, rt, "alice", "tools/call", map[string]any{"name": "workflow_schema", "arguments": arguments}); err == nil || err.Code != mcp.CodeInvalidParams {
			t.Errorf("%v answered %+v", arguments, err)
		}
	}
}

// The language and the schemas are resources too, for a client that prefers attaching documents
// to calling a tool: every topic and every part listed, each read as the tool answers it. A file of
// a repository, a run and a run's file are listed beside them to whoever holds what workflow_get,
// run_get and run_artifact need somewhere, as the tools are.
func TestTheLanguageIsReadAsResources(t *testing.T) {
	rt := platform(t)
	_, result, _ := called(t, rt, "alice", "resources/list", nil)
	if resources, _ := result["resources"].([]any); len(resources) != len(language.Topics())+len(language.Parts()) {
		t.Errorf("%d resources are listed", len(resources))
	}
	for as, want := range map[string]string{"alice": "language schema tree run artifact", "nobody": "language schema"} {
		_, result, _ = called(t, rt, as, "resources/templates/list", nil)
		var names []string
		for _, template := range result["resourceTemplates"].([]any) {
			names = append(names, template.(map[string]any)["name"].(string))
		}
		if strings.Join(names, " ") != want {
			t.Errorf("%s is listed the templates %v, want %s", as, names, want)
		}
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
