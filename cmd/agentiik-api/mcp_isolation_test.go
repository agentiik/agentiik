package main

import (
	"encoding/json"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/agentiik/agentiik/mcp"
)

// What sharing does not expose, held against the user's MCP server as against the routes: "Answer the
// same 404 for absent and invisible workflows and namespaces on every tool and resource, so names
// cannot be discovered."

// mcpAsked sends one request of the revision to the user's server as as, and reads its result or its
// error back.
func (x *tenants) mcpAsked(as asker, method string, params map[string]any) (map[string]any, *mcp.Error) {
	x.t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    mcp.Revision,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		x.t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(x.t.Context(), "POST", "/mcp", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("MCP-Protocol-Version", mcp.Revision)
	r.Header.Set("Mcp-Method", method)
	for _, field := range []string{"name", "uri"} {
		if v, ok := params[field].(string); ok {
			r.Header.Set("Mcp-Name", v)
		}
	}
	r.Header.Set("Authorization", "Bearer "+as.bearer)
	w := httptest.NewRecorder()
	x.in.router.ServeHTTP(w, r)
	var out struct {
		Result map[string]any `json:"result"`
		Error  *mcp.Error     `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		x.t.Fatalf("%s answered %d %s, which is no JSON-RPC answer", method, w.Code, w.Body)
	}
	return out.Result, out.Error
}

// toolIdentities are the arguments that say which thing a tool is about, in the order a path names
// them, as identities are of a route.
var toolIdentities = []string{"collection", "namespace", "workflow", "run"}

// filler is an argument the schema takes, for one that names nothing the test asks about: the first
// of an enumeration, the least a number may be, an object with one key where it needs one.
func filler(schema map[string]any) any {
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[0]
	}
	switch schema["type"] {
	case "integer", "number":
		if least, ok := schema["minimum"].(float64); ok {
			return least
		}
		return 1
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		out := map[string]any{}
		if least, _ := schema["minProperties"].(float64); least > 0 {
			properties, _ := schema["properties"].(map[string]any)
			for _, key := range slices.Sorted(maps.Keys(properties)) {
				out[key] = filler(properties[key].(map[string]any))
				break
			}
		}
		return out
	}
	return "nothing"
}

// Every tool the user's server offers whose arguments name a collection, a namespace, a workflow or
// a run is called as a principal holding nothing there, once naming what does not exist and once
// naming what does, each identity named as it is in turn and the rest as nothing, and has to answer
// the two alike: the same result, word for word, or the same protocol error. The same of each
// resource naming a workflow or a run. mallory owns hr and so is offered every tool, and holds
// nothing in finance; oscar operates finance/payroll, which makes finance a namespace he sees and
// monthly-invoicing a workflow of it he does not.
func TestAnAbsentNameAndAnInvisibleOneAreAnsweredAlikeThroughMCP(t *testing.T) {
	x := someTenants(t)
	listed, rpcErr := x.mcpAsked(x.as["mallory"], "tools/list", nil)
	if rpcErr != nil {
		t.Fatalf("tools/list answered %v", rpcErr)
	}
	present := map[string]any{"collection": x.collection, "namespace": "finance", "workflow": "monthly-invoicing", "run": x.run}

	asked := 0
	for _, raw := range listed["tools"].([]any) {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		input, _ := tool["inputSchema"].(map[string]any)
		properties, _ := input["properties"].(map[string]any)
		var ids []string
		for _, id := range toolIdentities {
			if properties[id] != nil {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		required := map[string]bool{}
		listedRequired, _ := input["required"].([]any)
		for _, r := range listedRequired {
			required[r.(string)] = true
		}
		for _, c := range []struct{ who, within string }{{"mallory", ""}, {"oscar", "finance"}} {
			nothing := map[string]any{"collection": ulid.New(), "namespace": "nowhere", "workflow": "nothing", "run": ulid.New()}
			varying := ids
			if c.within != "" {
				nothing["namespace"] = c.within
				varying = slices.DeleteFunc(slices.Clone(ids), func(p string) bool { return p == "namespace" })
				if len(varying) == 0 {
					continue
				}
			}
			arguments := func(named map[string]any) map[string]any {
				out := map[string]any{}
				for key, schema := range properties {
					if v, ok := named[key]; ok {
						out[key] = v
					} else if required[key] {
						out[key] = filler(schema.(map[string]any))
					}
				}
				return out
			}
			noneResult, noneErr := x.mcpAsked(x.as[c.who], "tools/call", map[string]any{"name": name, "arguments": arguments(nothing)})
			for i := range varying {
				mixed := maps.Clone(nothing)
				for _, p := range varying[:i+1] {
					mixed[p] = present[p]
				}
				result, err := x.mcpAsked(x.as[c.who], "tools/call", map[string]any{"name": name, "arguments": arguments(mixed)})
				asked++
				a, _ := json.Marshal(map[string]any{"result": noneResult, "error": noneErr})
				b, _ := json.Marshal(map[string]any{"result": result, "error": err})
				if string(a) != string(b) {
					t.Errorf("%s as %s naming %v answered %s, and naming nothing %s", name, c.who, mixed, b, a)
				}
			}
		}
	}
	if asked < 20 {
		t.Fatalf("%d questions were asked of the tools, and the user's server offers more tools naming something than that", asked)
	}

	read := func(as asker, uri string) string {
		t.Helper()
		_, err := x.mcpAsked(as, "resources/read", map[string]any{"uri": uri})
		if err == nil {
			t.Errorf("%s was read by somebody it is hidden from", uri)
			return ""
		}
		b, _ := json.Marshal(mcp.Error{Code: err.Code, Message: strings.ReplaceAll(err.Message, uri, "<uri>")})
		return string(b)
	}
	for _, c := range []struct {
		who            string
		hidden, absent string
		alsoAbsent     []string
	}{
		{who: "mallory", hidden: "agentiik://finance/monthly-invoicing/tree/" + theCommit + "/agentiik.yaml",
			absent: "agentiik://nowhere/nothing/tree/" + theCommit + "/agentiik.yaml", alsoAbsent: []string{"agentiik://finance/nothing/tree/" + theCommit + "/agentiik.yaml"}},
		{who: "oscar", hidden: "agentiik://finance/monthly-invoicing/tree/" + theCommit + "/agentiik.yaml",
			absent: "agentiik://finance/nothing/tree/" + theCommit + "/agentiik.yaml"},
		{who: "mallory", hidden: "agentiik://run/" + x.run, absent: "agentiik://run/" + ulid.New()},
		{who: "oscar", hidden: "agentiik://run/" + x.run, absent: "agentiik://run/" + ulid.New()},
	} {
		hidden := read(x.as[c.who], c.hidden)
		for _, uri := range append([]string{c.absent}, c.alsoAbsent...) {
			if absent := read(x.as[c.who], uri); hidden != absent {
				t.Errorf("as %s, %s is refused with %s, and %s with %s", c.who, c.hidden, hidden, uri, absent)
			}
		}
	}

	// And the owner reads what the others were refused, so that the refusals above were about who
	// asked and not about a URI nobody could read.
	if _, err := x.mcpAsked(x.as["alice"], "resources/read", map[string]any{"uri": "agentiik://finance/monthly-invoicing/tree/" + theCommit + "/agentiik.yaml"}); err != nil {
		t.Errorf("alice could not read her own workflow's file: %v", err)
	}
	if _, err := x.mcpAsked(x.as["alice"], "resources/read", map[string]any{"uri": "agentiik://run/" + x.run}); err != nil {
		t.Errorf("alice could not read her own run: %v", err)
	}
}
