package mcp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/mcp"
)

const origin = "https://agentiik.example.com"

func server(resources bool) *mcp.Server {
	return &mcp.Server{
		Info:         mcp.Implementation{Name: "agentiik", Version: "v0.7.0"},
		Instructions: "A workflow is a git repository.",
		Origin:       origin,
		Resources:    resources,
	}
}

// echo is a tool that answers its arguments back, and the surface it is offered on.
func surface() mcp.Surface {
	return mcp.Surface{
		Tools: []mcp.Tool{
			{
				Name: "b.echo", Description: "Answers its arguments back.", InputSchema: json.RawMessage(`{"type":"object"}`),
				Annotations: &mcp.Annotations{ReadOnlyHint: mcp.Hint(true)},
				Call: func(_ context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
					return &mcp.CallResult{Content: []mcp.Content{mcp.Text(string(arguments))}}, nil
				},
			},
			{
				Name: "a.fails", Description: "Fails the way a model can correct.", InputSchema: json.RawMessage(`{"type":"object"}`),
				Call: func(context.Context, json.RawMessage) (*mcp.CallResult, error) {
					return &mcp.CallResult{Content: []mcp.Content{mcp.Text("step normalize exited 2")}, IsError: true}, nil
				},
			},
			{
				Name: "c.refuses", Description: "Refuses its argument.", InputSchema: json.RawMessage(`{"type":"object"}`),
				Call: func(context.Context, json.RawMessage) (*mcp.CallResult, error) {
					return nil, mcp.InvalidParams("no topic is called nowhere")
				},
			},
			{
				Name: "d.breaks", Description: "Breaks.", InputSchema: json.RawMessage(`{"type":"object"}`),
				Call: func(context.Context, json.RawMessage) (*mcp.CallResult, error) {
					return nil, errors.New("the database at 10.0.0.7 refused user agentiik")
				},
			},
		},
		Resources: []mcp.Resource{{URI: "agentiik://language/steps", Name: "steps", MimeType: "text/markdown"}},
		Templates: []mcp.Template{{URITemplate: "agentiik://language/{topic}", Name: "language"}},
		Read: func(_ context.Context, uri string) ([]mcp.Contents, error) {
			if uri != "agentiik://language/steps" {
				return nil, mcp.InvalidParams("Resource not found: %s", uri)
			}
			return []mcp.Contents{{URI: uri, MimeType: "text/markdown", Text: "# `steps`"}}, nil
		},
	}
}

// request is one POST as a client of the revision sends it: its version in the header and in
// _meta, its method mirrored in Mcp-Method and, for a call or a read, its name in Mcp-Name.
func request(method string, params map[string]any) *http.Request {
	if params == nil {
		params = map[string]any{}
	}
	if _, set := params["_meta"]; !set {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcp.Revision,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", mcp.Revision)
	r.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		r.Header.Set("Mcp-Name", name)
	}
	if uri, ok := params["uri"].(string); ok {
		r.Header.Set("Mcp-Name", uri)
	}
	return r
}

// response is an answer read back.
type response struct {
	status int
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *mcp.Error      `json:"error"`
}

func served(t *testing.T, s *mcp.Server, r *http.Request) response {
	t.Helper()
	w := httptest.NewRecorder()
	s.Serve(w, r, surface())
	var out response
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("the answer is not JSON: %v: %s", err, w.Body)
		}
		if w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("the answer is %s", w.Header().Get("Content-Type"))
		}
	}
	out.status = w.Code
	return out
}

func refusedWith(t *testing.T, what string, got response, status, code int) {
	t.Helper()
	if got.status != status || got.Error == nil || got.Error.Code != code {
		t.Errorf("%s: answered %d with %+v, and not %d with code %d", what, got.status, got.Error, status, code)
	}
}

// "HTTP GET or DELETE to the MCP endpoint: respond with 405 Method Not Allowed." The revision has
// no stream to open with a GET and no session to end with a DELETE.
func TestOnlyAPOSTIsAnswered(t *testing.T) {
	for _, method := range []string{"GET", "DELETE", "PUT"} {
		w := httptest.NewRecorder()
		server(true).Serve(w, httptest.NewRequest(method, "/mcp", nil), surface())
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Errorf("a %s answered %d, Allow %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
}

// "If the Origin header is present and invalid, servers MUST respond with HTTP 403 Forbidden." A
// client that is no browser sends none and is answered; a page of the installation's own origin
// is answered; any other origin, or two of them, is refused before the body is read.
func TestAnOriginOtherThanTheInstallationsIsRefused(t *testing.T) {
	for _, origins := range [][]string{nil, {origin}} {
		r := request("server/discover", nil)
		for _, o := range origins {
			r.Header.Add("Origin", o)
		}
		if got := served(t, server(true), r); got.status != http.StatusOK || got.Error != nil {
			t.Errorf("Origin %v answered %d %+v", origins, got.status, got.Error)
		}
	}
	for _, origins := range [][]string{{"https://evil.example"}, {"http://agentiik.example.com"}, {"null"}, {origin, origin}} {
		r := request("server/discover", nil)
		for _, o := range origins {
			r.Header.Add("Origin", o)
		}
		got := served(t, server(true), r)
		refusedWith(t, "Origin "+strings.Join(origins, ","), got, http.StatusForbidden, mcp.CodeInvalidRequest)
		if got.ID != nil {
			t.Errorf("a refusal of the connection names the request %s", got.ID)
		}
	}
}

// The version a request speaks is in its header and in its _meta, the two agree, and a version
// the server does not speak is answered with the ones it does, so that a client of another
// revision can choose rather than guess.
func TestTheVersionIsDeclaredTwiceAndSpokenOnce(t *testing.T) {
	r := request("tools/list", nil)
	r.Header.Del("MCP-Protocol-Version")
	refusedWith(t, "no header", served(t, server(true), r), http.StatusBadRequest, mcp.CodeHeaderMismatch)

	r = request("tools/list", nil)
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	got := served(t, server(true), r)
	refusedWith(t, "an older revision", got, http.StatusBadRequest, mcp.CodeUnsupportedVersion)
	data, _ := json.Marshal(got.Error.Data)
	if string(data) != `{"requested":"2025-11-25","supported":["2026-07-28"]}` {
		t.Errorf("the versions offered are %s", data)
	}

	r = request("tools/list", map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2025-11-25",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}})
	refusedWith(t, "a _meta disagreeing with its header", served(t, server(true), r), http.StatusBadRequest, mcp.CodeHeaderMismatch)

	r = request("tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{}}})
	refusedWith(t, "no version in _meta", served(t, server(true), r), http.StatusBadRequest, mcp.CodeHeaderMismatch)

	r = request("tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcp.Revision}})
	refusedWith(t, "no capabilities in _meta", served(t, server(true), r), http.StatusBadRequest, mcp.CodeInvalidParams)
}

// The headers mirroring the body agree with it, "to prevent potential security vulnerabilities
// when different components in the network rely on different sources of truth", and a name
// written in the Base64 sentinel is read decoded.
func TestTheHeadersMirroringTheBodyAgreeWithIt(t *testing.T) {
	r := request("tools/list", nil)
	r.Header.Set("Mcp-Method", "tools/call")
	refusedWith(t, "another method in Mcp-Method", served(t, server(true), r), http.StatusBadRequest, mcp.CodeHeaderMismatch)

	r = request("tools/list", nil)
	r.Header.Del("Mcp-Method")
	refusedWith(t, "no Mcp-Method", served(t, server(true), r), http.StatusBadRequest, mcp.CodeHeaderMismatch)

	r = request("tools/call", map[string]any{"name": "b.echo"})
	r.Header.Set("Mcp-Name", "a.fails")
	refusedWith(t, "another tool in Mcp-Name", served(t, server(true), r), http.StatusBadRequest, mcp.CodeHeaderMismatch)

	r = request("resources/read", map[string]any{"uri": "agentiik://language/steps"})
	r.Header.Del("Mcp-Name")
	refusedWith(t, "a read with no Mcp-Name", served(t, server(true), r), http.StatusBadRequest, mcp.CodeHeaderMismatch)

	r = request("tools/call", map[string]any{"name": "b.echo"})
	r.Header.Set("Mcp-Name", "=?base64?"+base64.StdEncoding.EncodeToString([]byte("b.echo"))+"?=")
	if got := served(t, server(true), r); got.status != http.StatusOK || got.Error != nil {
		t.Errorf("a name in the Base64 sentinel answered %d %+v", got.status, got.Error)
	}
}

// A message that is no single JSON-RPC request is refused as the transport says, and a
// notification is accepted with nothing to say.
func TestAMessageIsOneRequestOrANotification(t *testing.T) {
	for what, body := range map[string]string{
		"a batch":          `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
		"no JSON":          `tools/list`,
		"another JSON-RPC": `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`,
		"a response":       `{"jsonrpc":"2.0","id":1,"result":{}}`,
		"a null id":        `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
		"an object id":     `{"jsonrpc":"2.0","id":{},"method":"tools/list"}`,
	} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.Header.Set("MCP-Protocol-Version", mcp.Revision)
		r.Header.Set("Mcp-Method", "tools/list")
		if got := served(t, server(true), r); got.status != http.StatusBadRequest || got.Error == nil {
			t.Errorf("%s answered %d %+v", what, got.status, got.Error)
		}
	}

	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`))
	w := httptest.NewRecorder()
	server(true).Serve(w, r, surface())
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Errorf("a notification answered %d %q", w.Code, w.Body)
	}

	r = httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"`+strings.Repeat("x", mcp.MaxBody)+`"}}`))
	w = httptest.NewRecorder()
	server(true).Serve(w, r, surface())
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the ceiling answered %d", w.Code)
	}
}

// server/discover is what the revision requires every server to answer: the versions it speaks,
// its capabilities, its identity and its instructions, with the caching hints, and every result
// carries resultType and names the server.
func TestDiscoverySaysWhatTheServerIs(t *testing.T) {
	got := served(t, server(true), request("server/discover", nil))
	if got.status != http.StatusOK || got.Error != nil || string(got.ID) != "7" {
		t.Fatalf("discovery answered %d %+v, id %s", got.status, got.Error, got.ID)
	}
	b, _ := json.Marshal(got.Result)
	want := `{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"agentiik","version":"v0.7.0"}},"cacheScope":"private","capabilities":{"resources":{},"tools":{}},"instructions":"A workflow is a git repository.","resultType":"complete","supportedVersions":["2026-07-28"],"ttlMs":0}`
	if string(b) != want {
		t.Errorf("discovery is\n%s\nand not\n%s", b, want)
	}

	got = served(t, server(false), request("server/discover", nil))
	if caps, _ := json.Marshal(got.Result["capabilities"]); string(caps) != `{"tools":{}}` {
		t.Errorf("a server with no resources declares %s", caps)
	}
}

// tools/list gives each tool's definition and never its work, in the order it was given, which is
// what a client caches by; a cursor is refused, since no list is paginated.
func TestToolsAreListedAsTheyWereGiven(t *testing.T) {
	got := served(t, server(true), request("tools/list", nil))
	tools, _ := got.Result["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "b.echo,a.fails,c.refuses,d.breaks" || got.Result["cacheScope"] != "private" {
		t.Errorf("the tools are %v, cached %v", names, got.Result["cacheScope"])
	}
	first, _ := json.Marshal(tools[0])
	if string(first) != `{"annotations":{"readOnlyHint":true},"description":"Answers its arguments back.","inputSchema":{"type":"object"},"name":"b.echo"}` {
		t.Errorf("a tool is listed as %s", first)
	}
	refusedWith(t, "a cursor", served(t, server(true), request("tools/list", map[string]any{"cursor": "x"})), http.StatusOK, mcp.CodeInvalidParams)
}

// A call answers the tool's result; a failure a model can correct is a result saying so; a tool
// nobody offered, an argument the tool refuses or arguments that are no object are protocol
// errors; and a tool that broke says only that, never what broke it.
func TestACallIsTheToolsAnswer(t *testing.T) {
	got := served(t, server(true), request("tools/call", map[string]any{"name": "b.echo", "arguments": map[string]any{"topic": "steps"}}))
	b, _ := json.Marshal(got.Result)
	if got.status != http.StatusOK || string(b) != `{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"agentiik","version":"v0.7.0"}},"content":[{"text":"{\"topic\":\"steps\"}","type":"text"}],"resultType":"complete"}` {
		t.Errorf("a call answered %d %s", got.status, b)
	}

	got = served(t, server(true), request("tools/call", map[string]any{"name": "a.fails"}))
	if got.Result["isError"] != true {
		t.Errorf("a failure is answered %+v", got.Result)
	}

	refusedWith(t, "a tool nobody offered", served(t, server(true), request("tools/call", map[string]any{"name": "workflow.commit"})), http.StatusOK, mcp.CodeInvalidParams)
	refusedWith(t, "an argument the tool refuses", served(t, server(true), request("tools/call", map[string]any{"name": "c.refuses"})), http.StatusOK, mcp.CodeInvalidParams)
	refusedWith(t, "arguments that are no object", served(t, server(true), request("tools/call", map[string]any{"name": "b.echo", "arguments": []int{1}})), http.StatusOK, mcp.CodeInvalidParams)

	got = served(t, server(true), request("tools/call", map[string]any{"name": "d.breaks"}))
	refusedWith(t, "a tool that broke", got, http.StatusOK, mcp.CodeInternalError)
	if got.Error != nil && strings.Contains(got.Error.Message, "10.0.0.7") {
		t.Errorf("a tool that broke told the client %q", got.Error.Message)
	}
}

// Resources are listed and read where the server offers them, a URI naming nothing is the
// revision's Invalid Params, and a server offering none does not have the methods at all.
func TestResourcesAreReadWhereTheServerOffersThem(t *testing.T) {
	got := served(t, server(true), request("resources/read", map[string]any{"uri": "agentiik://language/steps"}))
	b, _ := json.Marshal(got.Result["contents"])
	if string(b) != `[{"mimeType":"text/markdown","text":"# `+"`steps`"+`","uri":"agentiik://language/steps"}]` || got.Result["ttlMs"] != float64(0) {
		t.Errorf("a read answered %s", b)
	}
	refusedWith(t, "a URI naming nothing", served(t, server(true), request("resources/read", map[string]any{"uri": "agentiik://language/nowhere"})), http.StatusOK, mcp.CodeInvalidParams)
	for method, field := range map[string]string{"resources/list": "resources", "resources/templates/list": "resourceTemplates"} {
		if got := served(t, server(true), request(method, nil)); len(got.Result[field].([]any)) != 1 {
			t.Errorf("%s answered %+v", method, got.Result)
		}
		refusedWith(t, method+" on a server with no resources", served(t, server(false), request(method, nil)), http.StatusNotFound, mcp.CodeMethodNotFound)
	}
}

// A resource that is text is answered as text, an empty one included, and one that is not as its
// bytes in base64 under blob, never both, as the revision's two kinds of contents are.
func TestAResourceIsTextOrBlob(t *testing.T) {
	for _, c := range []struct {
		contents mcp.Contents
		want     string
	}{
		{mcp.Contents{URI: "agentiik://finance/w/tree/main/a.txt", MimeType: "text/plain", Text: "a"}, `{"uri":"agentiik://finance/w/tree/main/a.txt","mimeType":"text/plain","text":"a"}`},
		{mcp.Contents{URI: "agentiik://finance/w/tree/main/empty.txt", Text: ""}, `{"uri":"agentiik://finance/w/tree/main/empty.txt","text":""}`},
		{mcp.Contents{URI: "agentiik://finance/w/tree/main/logo.png", MimeType: "application/octet-stream", Blob: []byte{0x89, 'P', 'N', 'G'}}, `{"uri":"agentiik://finance/w/tree/main/logo.png","mimeType":"application/octet-stream","blob":"iVBORw=="}`},
	} {
		if b, err := json.Marshal(c.contents); err != nil || string(b) != c.want {
			t.Errorf("%s is written %s, %v, want %s", c.contents.URI, b, err, c.want)
		}
	}
}

// "If the server does not implement the requested RPC method, it MUST respond with 404 Not Found
// and a JSON-RPC error with code -32601." initialize among them: the revision has no handshake.
func TestAMethodTheServerDoesNotHaveIsNotFound(t *testing.T) {
	for _, method := range []string{"initialize", "ping", "prompts/list", "subscriptions/listen", "logging/setLevel"} {
		refusedWith(t, method, served(t, server(true), request(method, nil)), http.StatusNotFound, mcp.CodeMethodNotFound)
	}
}
