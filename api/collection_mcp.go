package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/mcp"
	"github.com/agentiik/agentiik/schema"
	"github.com/agentiik/agentiik/trigger"
)

// A collection's endpoint, /mcp/collections/{id}: one tool per member that offers one, to the
// collection's owner alone. "A collection exposes tools and nothing else: no resources, no prompts,
// no tree. A resource would turn workflow:run into a read path into the repository."
//
// Each tool is read from the entry point at the head of its member's ref at every tools/list:
// "inputSchema: an object whose properties are the workflow's inputs, each with its own JSON Schema
// as it stands, the required ones required, and its references into the repository bundled in,
// since a client cannot read the tree"; outputSchema the envelope of the output it returns, its
// items' data held to the output's schema; and the block's annotations unchanged. "Nothing about a
// tool comes from the graph."
//
// A call is a run: "the calling principal's, with trigger_kind: mcp and the collection it came
// through", at the commit the member's ref points at when it is called, started by the one path
// every run takes, so that its inputs are bound before it exists, it counts against the namespace's
// quota and it holds the workflow's concurrency group as any run does.

// collectionInstructions is the discovery's orientation for a collection: what calling one of its
// tools does, and where a run started by one is followed.
const collectionInstructions = "Each tool runs a workflow of the installation as you, with its arguments as the workflow's inputs. A sync tool waits for the run and returns its output; an async tool returns the run's identifier at once, which the user's server's run.get follows."

// syncWaitCeiling is how long a sync call waits where its block writes no timeout: the ceiling
// itself, "because clients time out".
const syncWaitCeiling = 120 * time.Second

// treeBase is what a bundled schema's references resolve against, the base under which the engine
// compiles every schema a workflow writes: "./schemas/order.json" written in a workflow input is
// agk://repo/schemas/order.json, the $id the file is bundled under.
const treeBase = "agk://repo/"

// serveCollection answers one request to a collection's endpoint, as its owner presenting a bearer
// token.
func (m *MCP) serveCollection(w http.ResponseWriter, r *http.Request, caller Caller) {
	if _, bearer := bearerOf(r); !bearer {
		w.Header().Set("WWW-Authenticate", "Bearer")
		refuse(w, http.StatusUnauthorized, "a collection's endpoint takes a bearer token, an API token or one issued for this installation, and a session is the console's")
		return
	}
	if caller.Narrowed() {
		// Before the collection is looked up, so that the answer is the same whatever it names.
		refuse(w, http.StatusForbidden, narrowedReachesNoCollection)
		return
	}
	principal, _, ok := owned(caller)
	if !ok {
		refuse(w, http.StatusNotFound, noCollection)
		return
	}
	c := m.collections
	var col db.Collection
	err := c.pool.Installation(r.Context(), db.Collections, func(ctx context.Context, wide *db.Wide) error {
		var err error
		col, err = wide.Collection(ctx, principal, r.PathValue("id"))
		return err
	})
	if errors.Is(err, db.ErrNoCollection) {
		refuse(w, http.StatusNotFound, noCollection)
		return
	}
	if err != nil {
		refuse(w, http.StatusInternalServerError, "the collection could not be read")
		return
	}
	offers, err := c.offers(r.Context(), caller, col.Members)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "what the collection offers could not be read")
		return
	}
	var tools []mcp.Tool
	for _, o := range offers {
		if o.reason != "" {
			// "A member whose entry point at its ref declares no mcp block, or whose workflow the
			// owner may no longer run, is left out of tools/list and answered as a tool that does
			// not exist", and so are two whose tools go by one name, so that a call of the name
			// runs neither rather than whichever comes first.
			continue
		}
		t, err := c.tool(r.Context(), caller, col.ID, o)
		if err != nil {
			// A tool whose schemas cannot be read is left out rather than the collection answered
			// with nothing: the version holding it was accepted, so this is the installation's to
			// look at, and every other member still offers its own.
			continue
		}
		tools = append(tools, t)
	}
	m.collectionServer.Serve(w, r, mcp.Surface{Tools: tools})
}

// tool is the tool one member offers, and the call that runs it.
func (c *Collections) tool(ctx context.Context, caller Caller, collection string, o offer) (mcp.Tool, error) {
	block := o.graph.Workflow().MCP
	p, err := c.published(ctx, o)
	if err != nil {
		return mcp.Tool{}, err
	}
	properties := map[string]any{}
	var required []string
	for _, name := range sortedKeys(p.Inputs) {
		in := p.Inputs[name]
		var held any = true
		if len(in.Schema) > 0 {
			held = json.RawMessage(in.Schema)
		}
		properties[name] = held
		if in.Required && in.Default == nil {
			required = append(required, name)
		}
	}
	input := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		input["required"] = required
	}
	t := mcp.Tool{
		Name: o.tool, Title: block.Title, Description: block.Description,
		InputSchema: bundled(input, p.Files),
	}
	if len(p.Output) > 0 && block.Mode != graph.ToolAsync {
		// What the result is: the output's envelope, whose items carry what the output's schema
		// says, since "a client is told the shape of a result and not only its text" and the
		// protocol holds a structured result to its outputSchema. An async tool answers the run it
		// started and never the output, so it publishes none: a structured result has to conform
		// to the outputSchema a tool publishes, and the run's identifier is no envelope.
		t.OutputSchema = bundled(map[string]any{
			"type": "object", "required": []string{"meta", "items"},
			"properties": map[string]any{
				"meta": map[string]any{"type": "object"},
				"items": map[string]any{"type": "array", "items": map[string]any{
					"type": "object", "required": []string{"id", "data", "files"},
					"properties": map[string]any{
						"id": map[string]any{"type": "string"}, "data": json.RawMessage(p.Output),
						"files": map[string]any{"type": "array"},
					},
				}},
			},
		}, p.Files)
	}
	if a := block.Annotations; a != (graph.Annotations{}) {
		t.Annotations = &mcp.Annotations{ReadOnlyHint: a.ReadOnlyHint, IdempotentHint: a.IdempotentHint, DestructiveHint: a.DestructiveHint, OpenWorldHint: a.OpenWorldHint}
	}
	t.Call = func(ctx context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
		return c.call(ctx, caller, collection, o, arguments)
	}
	return t, nil
}

// bundled is a schema with every file of the tree it reaches embedded under $defs, each by its path
// and carrying the $id the engine compiled it under, and the whole given the base those $ids
// resolve against: a reference written "./schemas/order.json" then resolves to the file embedded
// beside it, as JSON Schema 2020-12 resolves an embedded resource, with nothing rewritten.
func bundled(root map[string]any, files map[string]any) json.RawMessage {
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = treeBase
	if len(files) > 0 {
		defs := map[string]any{}
		for path, doc := range files {
			if o, ok := doc.(map[string]any); ok {
				embedded := make(map[string]any, len(o)+1)
				for k, v := range o {
					embedded[k] = v
				}
				if _, has := embedded["$id"]; !has {
					embedded["$id"] = treeBase + path
				}
				doc = embedded
			}
			defs[path] = doc
		}
		root["$defs"] = defs
	}
	b, err := json.Marshal(root)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// publication is what one version's tool takes and gives, kept by the commit it was read at: a
// version never changes, so what was read of it once is what it is, and a list of a hundred
// members reads each tree once rather than at every list.
type publication struct {
	mu   sync.Mutex
	held map[string]trigger.Published
}

// publicationsKept is how many versions' tools are kept read: a collection holds a hundred members
// and a principal fifty collections, and what is not kept is read again.
const publicationsKept = 1024

// published reads what the member's tool takes and gives, from what is kept where it was read.
func (c *Collections) published(ctx context.Context, o offer) (trigger.Published, error) {
	block := o.graph.Workflow().MCP
	key := o.member.Namespace + "/" + o.member.Workflow + "@" + o.commit + "#" + block.Output
	c.kept.mu.Lock()
	p, ok := c.kept.held[key]
	c.kept.mu.Unlock()
	if ok {
		return p, nil
	}
	p, err := c.starter.Publication(ctx, trigger.Request{Namespace: o.member.Namespace, Workflow: o.member.Workflow, Commit: o.commit}, block.Output)
	if err != nil {
		return trigger.Published{}, err
	}
	c.kept.mu.Lock()
	if c.kept.held == nil || len(c.kept.held) >= publicationsKept {
		c.kept.held = map[string]trigger.Published{}
	}
	c.kept.held[key] = p
	c.kept.mu.Unlock()
	return p, nil
}

// call runs the member's workflow with the call's arguments as its inputs, and answers as its mode
// says: at once for async, and for sync once the run ends or the wait does.
func (c *Collections) call(ctx context.Context, caller Caller, collection string, o offer, arguments json.RawMessage) (*mcp.CallResult, error) {
	target := Target{Namespace: o.member.Namespace, Workflow: o.member.Workflow}
	// "Calling one of its tools requires workflow:run on the workflow, never workflow:read", asked
	// at every call, since a grant revoked takes the tool away from the next request.
	runs, err := caller.Holds(ctx, WorkflowRun, target)
	if err != nil {
		return nil, err
	}
	if !runs {
		return nil, mcp.InvalidParams("Unknown tool: %s", o.tool)
	}
	inputs := map[string]any{}
	if len(arguments) > 0 && string(arguments) != "null" {
		d := json.NewDecoder(bytes.NewReader(arguments))
		d.UseNumber()
		if err := d.Decode(&inputs); err != nil {
			return nil, mcp.InvalidParams("the arguments of %s are an object naming the workflow's inputs: %v", o.tool, err)
		}
	}
	block := o.graph.Workflow().MCP
	// The run's entry in the audit log says it came through MCP, by the tool it was called as,
	// beside the collection it came through.
	started, err := c.starter.Start(audit.Through(ctx, o.tool), trigger.Request{
		Namespace: target.Namespace, Workflow: target.Workflow,
		Kind: agk.TriggerMCP, By: string(caller.Principal),
		Commit: o.commit, Inputs: inputs,
		Collection: &db.CollectionCall{ID: collection, Tool: o.tool},
	})
	var refused *schema.InputRefusal
	var large *trigger.InputsTooLarge
	var reached *db.RunsPerHourReached
	switch {
	case errors.As(err, &refused):
		// "A protocol error. The input schema rejects it before any run exists."
		return nil, mcp.InvalidParams("the arguments of %s are refused: %v", o.tool, refused)
	case errors.As(err, &large):
		return nil, mcp.InvalidParams("the arguments of %s are refused: %s", o.tool, large.Why)
	case errors.As(err, &reached):
		// "A client calling in a loop meets the rate limit a webhook would": no run, and when one
		// more fits, told to the model as a result it can act on by waiting.
		return &mcp.CallResult{
			Content:           []mcp.Content{mcp.Text("No run was started: " + reached.Reason() + ".")},
			StructuredContent: map[string]any{"retry_after_seconds": reached.Seconds()},
			IsError:           true,
		}, nil
	case errors.Is(err, db.ErrWorkflowMoving):
		return &mcp.CallResult{Content: []mcp.Content{mcp.Text(movingSentence(target))}, IsError: true}, nil
	case err != nil:
		return nil, err
	}
	run := started.Run
	if block.Mode == graph.ToolAsync {
		return &mcp.CallResult{
			Content:           []mcp.Content{mcp.Text(fmt.Sprintf("Run %s of %s/%s started, at commit %s. Follow it with the user's server's run.get.", run, target.Namespace, target.Workflow, started.Commit))},
			StructuredContent: map[string]any{"run": string(run), "state": agk.Queued.String(), "commit": started.Commit},
		}, nil
	}

	wait := time.Duration(block.Timeout)
	if wait <= 0 || wait > syncWaitCeiling {
		wait = syncWaitCeiling
	}
	detail, err := c.awaited(ctx, target.Namespace, run, wait)
	if err != nil {
		return nil, err
	}
	switch detail.State {
	case agk.Succeeded:
		return c.succeeded(ctx, target.Namespace, detail, block.Output)
	case agk.Failed, agk.Cancelled, agk.TimedOut:
		return &mcp.CallResult{Content: []mcp.Content{mcp.Text(ended(detail))}, StructuredContent: map[string]any{"run": string(run), "state": detail.State.String()}, IsError: true}, nil
	}
	// Still going when the wait is over: the run goes on, and the model is told how to follow it
	// rather than told it failed.
	return &mcp.CallResult{
		Content:           []mcp.Content{mcp.Text(fmt.Sprintf("Run %s of %s/%s is still %s after %s, the longest a sync call waits. It goes on: follow it with the user's server's run.get.", run, target.Namespace, target.Workflow, detail.State, wait))},
		StructuredContent: map[string]any{"run": string(run), "state": detail.State.String()},
	}, nil
}

// awaited reads the run until it ends or the wait is over, looking again sooner at first and less
// often as it goes on, so that a run of a second answers in a second and one of two minutes is not
// asked about forty times a second.
func (c *Collections) awaited(ctx context.Context, namespace string, run agk.RunID, wait time.Duration) (db.RunDetail, error) {
	deadline := time.Now().Add(wait)
	pause := 100 * time.Millisecond
	for {
		var detail db.RunDetail
		err := c.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			detail, err = ns.RunDetail(ctx, run)
			return err
		})
		if err != nil {
			return db.RunDetail{}, err
		}
		if detail.State >= agk.Succeeded {
			return detail, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return detail, nil
		}
		select {
		case <-ctx.Done():
			return db.RunDetail{}, ctx.Err()
		case <-time.After(min(pause, left)):
		}
		pause = min(pause*2, 2*time.Second)
	}
}

// succeeded answers a run that ended well: its output's envelope as structuredContent, with the
// envelope as text beside it, or where the tool returns no output, that the run succeeded.
func (c *Collections) succeeded(ctx context.Context, namespace string, detail db.RunDetail, output string) (*mcp.CallResult, error) {
	if output == "" {
		return &mcp.CallResult{
			Content:           []mcp.Content{mcp.Text(fmt.Sprintf("Run %s succeeded.", detail.Run))},
			StructuredContent: map[string]any{"run": string(detail.Run), "state": detail.State.String()},
		}, nil
	}
	var out db.Output
	err := c.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		out, err = ns.Output(ctx, detail.Run, output)
		return err
	})
	if err != nil {
		return nil, err
	}
	if c.objects == nil {
		return nil, errors.New("api: this installation has no object store attached, and an envelope is read from nowhere else")
	}
	storage, err := c.pool.Storage(ctx, namespace)
	if err != nil {
		return nil, err
	}
	e, err := artifact.GetEnvelope(ctx, c.objects, storage, out.Envelope.Digest, c.limits)
	if err != nil {
		return nil, err
	}
	var text bytes.Buffer
	if _, err := e.Encode(&text); err != nil {
		return nil, err
	}
	var structured any
	if err := json.Unmarshal(text.Bytes(), &structured); err != nil {
		return nil, err
	}
	return &mcp.CallResult{Content: []mcp.Content{mcp.Text(strings.TrimSpace(text.String()))}, StructuredContent: structured}, nil
}

// ended says why a run ended without succeeding, for the model that called it to correct itself:
// "naming the failing step, its exit code and its message", the message being the reason the run
// records where something other than its workflow ended it.
func ended(d db.RunDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s of %s ended %s.", d.Run, d.Workflow, d.State)
	for _, s := range d.Steps {
		if s.Verdict != agk.VerdictFailed {
			continue
		}
		fmt.Fprintf(&b, " Step %s failed", s.Step)
		for i := len(d.Tasks) - 1; i >= 0; i-- {
			if t := d.Tasks[i]; t.Step == s.Step && t.ExitCode != nil {
				fmt.Fprintf(&b, " with exit code %d", *t.ExitCode)
				break
			}
		}
		b.WriteString(".")
		break
	}
	if d.Reason != "" {
		b.WriteString(" " + d.Reason + ".")
	}
	return b.String()
}
