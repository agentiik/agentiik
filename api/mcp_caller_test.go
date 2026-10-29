package api_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/internal/dbtest"
)

// inquiry is one thing the authorizer was asked: about whom, what over which target, and for which
// tool of the MCP server, where it was asked while a request the server made of the API was served,
// which carries the tool it was made for, and empty outside any such request.
type inquiry struct {
	who     api.Principal
	what    api.Permission
	over    api.Target
	through string
}

// made is one request the MCP server made of the API: the credential it presented, and the tool it
// was made for.
type made struct {
	credential, through string
}

// tracing is an authorizer that remembers everything it is asked, and the requests the MCP server
// makes of the API, which it is handed to observe. It says nothing of what a principal is granted,
// so that the user's server lists it every tool and each route decides each call.
type tracing struct {
	owningGrants

	mu        sync.Mutex
	inquiries []inquiry
	requests  []made
}

func (a *tracing) Allow(ctx context.Context, who api.Principal, what api.Permission, over api.Target) (bool, error) {
	a.asked(ctx, inquiry{who: who, what: what, over: over})
	return a.owningGrants.Allow(ctx, who, what, over)
}

// ownedAsked is what asking which namespaces a principal owns is recorded as.
const ownedAsked api.Permission = "(owned)"

func (a *tracing) Owned(ctx context.Context, who api.Principal) ([]string, error) {
	a.asked(ctx, inquiry{who: who, what: ownedAsked})
	return a.owningGrants.Owned(ctx, who)
}

func (a *tracing) asked(ctx context.Context, q inquiry) {
	q.through, _ = audit.ThroughOf(ctx)
	a.mu.Lock()
	a.inquiries = append(a.inquiries, q)
	a.mu.Unlock()
}

// observe is what the MCP server's requests of the API pass through on their way to it.
func (a *tracing) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		through, _ := audit.ThroughOf(r.Context())
		a.mu.Lock()
		a.requests = append(a.requests, made{credential: r.Header.Get("Authorization"), through: through})
		a.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// during is what was asked and requested while do ran, alone.
func (a *tracing) during(do func()) ([]inquiry, []made) {
	a.mu.Lock()
	a.inquiries, a.requests = nil, nil
	a.mu.Unlock()
	do()
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.inquiries), slices.Clone(a.requests)
}

// outside are the inquiries made outside every request the MCP server made of the API.
func outside(asked []inquiry) []inquiry {
	return slices.DeleteFunc(slices.Clone(asked), func(q inquiry) bool { return q.through != "" })
}

// askedAsTheCaller is an installation serving the user's server and collections, where alice holds
// every permission on finance and owns it, and the authorizer remembers what it is asked and what
// the MCP server requests of the API.
func askedAsTheCaller(t *testing.T) (*collected, *tracing) {
	t.Helper()
	finance := api.Target{Namespace: "finance"}
	var every []grant
	for _, p := range []api.Permission{api.WorkflowRead, api.WorkflowWrite, api.WorkflowRun, api.WorkflowDelete, api.RunRead, api.RunReadData, api.SecretUse, api.SecretWrite, api.GrantManage} {
		every = append(every, grant{p, finance})
	}
	a := &tracing{owningGrants: owningGrants{granted: granted{"alice": every}, owned: map[api.Principal][]string{"alice": {"finance"}}}}
	pool, super := dbtest.Open(t)
	c := collectingAs(t, pool, super, strings.ReplaceAll(workflowDocument, "monthly-invoicing", "payroll"), a, narrowing)
	api.ObserveRequests(c.mcp, a.observe)
	return c, a
}

// argued are the arguments of a call of the tool whose input schema is given: each property named
// takes its value from named, and every other one required the least its schema takes.
func argued(schema map[string]any, named map[string]any) map[string]any {
	properties, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	out := map[string]any{}
	for key, p := range properties {
		if v, ok := named[key]; ok {
			out[key] = v
		} else if slices.Contains(required, any(key)) {
			out[key] = least(p.(map[string]any))
		}
	}
	return out
}

// least is a value a schema takes, for an argument that names nothing the test asks about.
func least(schema map[string]any) any {
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[0]
	}
	switch schema["type"] {
	case "integer", "number":
		if m, ok := schema["minimum"].(float64); ok {
			return m
		}
		return 1
	case "boolean":
		return false
	case "object":
		out := map[string]any{}
		if n, _ := schema["minProperties"].(float64); n > 0 {
			properties, _ := schema["properties"].(map[string]any)
			for _, key := range slices.Sorted(maps.Keys(properties)) {
				out[key] = least(properties[key].(map[string]any))
				break
			}
		}
		return out
	}
	return "nothing"
}

// "Its tools mirror the API and hold no authority of their own: each call runs as the presenting
// principal, under the grants that govern the console." Every tool the user's server lists, and every
// resource it reads, is answered by requests of the API, each presenting the caller's own credential
// and made for that tool, and asks the authorizer nothing itself: what is asked outside those requests
// while a call is answered is what a list asks, and about the caller alone. A tool that read a store,
// or asked the authorizer, in place of making the request its route serves fails it, answering with no
// request made or asking something of its own. The language and the schemas are the two that make
// none, since they answer from the reference the program carries.
func TestEveryToolOfTheUsersServerIsARouteAskedAsTheCaller(t *testing.T) {
	c, a := askedAsTheCaller(t)
	status, started := c.ask("POST", "/api/v1/finance/workflows/monthly-invoicing/runs", "alice",
		map[string]any{"inputs": map[string]any{"orders": []any{map[string]any{"order": "ORD-0001"}}}})
	if status != http.StatusAccepted {
		t.Fatalf("a run could not be started: %d %v", status, started)
	}
	run := started["run"].(string)
	collection := c.made("alice", "back-office")
	c.ask("PUT", "/api/v1/me/collections/"+collection+"/members/finance/monthly-invoicing", "alice", map[string]any{})

	var listed map[string]any
	asked, _ := a.during(func() { _, listed, _ = calledAt(t, c.rt, "/mcp", "alice", "tools/list", nil) })
	listing := outside(asked)
	tools, _ := listed["tools"].([]any)
	if len(tools) < 29 {
		t.Fatalf("the user's server lists %d tools to a caller its authorizer says nothing of, and it has more", len(tools))
	}
	// The tools that read first, naming what finance holds, and then those that change something,
	// naming nothing that exists, so that each is asked while the rest of the installation stands.
	reads := func(tool any) bool {
		hints, _ := tool.(map[string]any)["annotations"].(map[string]any)
		return hints["readOnlyHint"] == true
	}
	slices.SortStableFunc(tools, func(x, y any) int {
		switch {
		case reads(x) && !reads(y):
			return -1
		case reads(y) && !reads(x):
			return 1
		}
		return 0
	})
	files := map[string]any{"agentiik.yaml": toolDocument}
	present := map[string]any{"namespace": "finance", "workflow": "monthly-invoicing", "run": run, "collection": collection, "step": "normalize", "files": files}
	absent := map[string]any{"namespace": "nowhere", "workflow": "nothing", "run": "01M2ZZZZZZZZZZZZZZZZZZZZZZ", "collection": "01JR8V0000000000000000000A", "files": files}
	answeredAlone := []string{"workflow.language", "workflow.schema"}

	// held holds what one call asked and requested to the rule: the call named, whose requests are
	// made for through, and which makes none where it is answered alone.
	held := func(name, through string, alone bool, asked []inquiry, requested []made) {
		t.Helper()
		for _, m := range requested {
			if m != (made{credential: "Bearer alice", through: through}) {
				t.Errorf("%s made a request of the API presenting %q, for %q, where it presents the caller's credential for %s", name, m.credential, m.through, through)
			}
		}
		if alone != (len(requested) == 0) {
			t.Errorf("%s made %d requests of the API: everything but the language and the schemas is answered by its route", name, len(requested))
		}
		if own := outside(asked); !slices.Equal(own, listing) {
			t.Errorf("%s asked the authorizer %v outside any request of the API, where a list asks %v", name, own, listing)
		}
		for _, q := range asked {
			if q.who != "alice" {
				t.Errorf("%s asked the authorizer about %s, where it asks as its caller alone", name, q.who)
			}
		}
	}

	for _, tool := range tools {
		tool := tool.(map[string]any)
		name := tool["name"].(string)
		named := absent
		if reads(tool) {
			named = present
		}
		arguments := argued(tool["inputSchema"].(map[string]any), named)
		asked, requested := a.during(func() {
			if status, _, refusal := calledAt(t, c.rt, "/mcp", "alice", "tools/call", map[string]any{"name": name, "arguments": arguments}); status != http.StatusOK || refusal != nil {
				t.Errorf("%s called with %v answered %d %v", name, arguments, status, refusal)
			}
		})
		held(name, name, slices.Contains(answeredAlone, name), asked, requested)
	}

	// The resources, each read through its tool's route, the language and the schemas excepted.
	for uri, alone := range map[string]bool{
		"agentiik://finance/monthly-invoicing/tree/main/agentiik.yaml": false,
		"agentiik://run/" + run:   false,
		"agentiik://language/mcp": true,
		"agentiik://schema/brick": true,
	} {
		asked, requested := a.during(func() {
			if _, _, refusal := calledAt(t, c.rt, "/mcp", "alice", "resources/read", map[string]any{"uri": uri}); refusal != nil {
				t.Errorf("%s was read %v", uri, refusal)
			}
		})
		held(uri, "resources/read", alone, asked, requested)
	}
}

// A collection's tool is no route of the API but a run, which every trigger starts by the one path
// runs take: its call is still asked as its caller and nobody else. Everything it asks the authorizer
// is about the caller, workflow:run on the member's workflow among it, asked again at the call rather
// than read from the list it was offered in, so that a grant revoked between the two leaves the tool
// unknown and starts no run; and the run it starts is the caller's, whom the controller asks about once
// more before letting it in, as it asks of a run any trigger creates.
func TestACollectionsCallIsAskedAsItsCaller(t *testing.T) {
	c, a := askedAsTheCaller(t)
	collection := c.made("alice", "back-office")
	c.ask("PUT", "/api/v1/me/collections/"+collection+"/members/finance/monthly-invoicing", "alice", map[string]any{})
	path := "/mcp/collections/" + collection
	call := map[string]any{"name": "create_invoices", "arguments": map[string]any{"orders": []any{map[string]any{"order": "ORD-0001"}}}}

	asked, requested := a.during(func() {
		if status, _, refusal := calledAt(t, c.rt, path, "alice", "tools/call", call); status != http.StatusOK || refusal != nil {
			t.Errorf("the call was answered %d %v", status, refusal)
		}
	})
	for _, m := range requested {
		if m.credential != "Bearer alice" {
			t.Errorf("the call made a request presenting %q", m.credential)
		}
	}
	runs := 0
	for _, q := range asked {
		if q.who != "alice" {
			t.Errorf("the call asked the authorizer about %s, where it asks as its caller alone", q.who)
		}
		if q.what == api.WorkflowRun && q.over == (api.Target{Namespace: "finance", Workflow: "monthly-invoicing"}) {
			runs++
		}
	}
	// Once for the tools the request is offered, and once more as the run is started.
	if runs < 2 {
		t.Errorf("the call asked workflow:run on finance/monthly-invoicing %d times: %v", runs, asked)
	}
	started := c.runs()
	if len(started) != 1 || started[0].TriggeredBy != "alice" || started[0].Trigger.String() != "mcp" {
		t.Fatalf("the call started %+v", started)
	}

	// workflow:run taken away between the list and the call.
	_, result, _ := calledAt(t, c.rt, path, "alice", "tools/list", nil)
	if tools, _ := result["tools"].([]any); len(tools) != 1 {
		t.Fatalf("the collection lists %v", result)
	}
	a.granted["alice"] = slices.DeleteFunc(a.granted["alice"], func(g grant) bool { return g.what == api.WorkflowRun })
	if _, _, refusal := calledAt(t, c.rt, path, "alice", "tools/call", call); refusal == nil || refusal.Code != -32602 || !strings.Contains(refusal.Message, "Unknown tool") {
		t.Errorf("a call by a caller no longer holding workflow:run was answered %v", refusal)
	}
	if after := c.runs(); len(after) != 1 {
		t.Errorf("a call by a caller no longer holding workflow:run started a run: %+v", after)
	}
}
