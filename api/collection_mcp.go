package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
const collectionInstructions = "Each tool runs a workflow of the installation as you, with its arguments as the workflow's inputs. A sync tool waits for the run and returns its output; an async tool returns the run's identifier at once, which the user's server's run_get follows."

// syncWaitCeiling is how long a sync call waits where its block writes no timeout: the ceiling
// itself, "because clients time out".
const syncWaitCeiling = 120 * time.Second

// treeBase is what a bundled schema's references are resolved against before they are rewritten,
// the base under which the engine compiles every schema a workflow writes: "./schemas/order.json"
// written in a workflow input is agk://repo/schemas/order.json, the file the bundle embeds by its
// path, schemas/order.json.
const treeBase = "agk://repo/"

// serveCollection answers one request to a collection's endpoint, as its owner presenting a bearer
// token.
func (m *MCP) serveCollection(w http.ResponseWriter, r *http.Request, caller Caller) {
	if _, bearer := bearerOf(r); !bearer {
		m.noBearer(w, r, "a collection's endpoint takes a bearer token, an API token or one issued for this installation, and a session is the console's")
		return
	}
	if caller.Narrowed() && caller.Audience == "" {
		// Before the collection is looked up, so that the answer is the same whatever it names. The
		// OAuth token issued for this collection is the one narrowed token that reaches it: its
		// audience is the collection, which Principals holds it to, and it keeps workflow:run,
		// which is all a call takes.
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
		t, err := c.tool(r.Context(), caller, col.ID, o, m.failureLog(r, o.tool))
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

// failureLog reads the end of the log a failed task wrote, for the collection's tool named, through
// the log stream route as the caller of r, as run_logs reads a step's log (tailed): masked as its
// runner masked it, as every log is, and none of it to a caller the route refuses, one who may not
// read the run's logs. A route that answers no stream, or a log that cannot be read, is no message.
func (m *MCP) failureLog(r *http.Request, tool string) failureLog {
	return func(run agk.RunID, step agk.Step, task agk.TaskID) []string {
		a, err := m.tailed(r, tool, "/api/v1/runs/"+segment(string(run))+"/steps/"+segment(string(step))+"/logs", failureLines, task)
		if err != nil || !a.streaming {
			return nil
		}
		return a.tail()
	}
}

// failureLog is the last lines a task of a run wrote, by its step and its key.
type failureLog func(run agk.RunID, step agk.Step, task agk.TaskID) []string

// failureLines and failureMost are how much of a failed task's log its tool's error carries: its last
// lines, where a brick says why it gave up, and of them at most about 2 KiB, enough for a model to
// correct itself and little enough that a failure does not hand it the log, which run_logs reads.
const (
	failureLines = 20
	failureMost  = 2 << 10
)

// tool is the tool one member offers, and the call that runs it, a failed run's message read with
// logs.
func (c *Collections) tool(ctx context.Context, caller Caller, collection string, o offer, logs failureLog) (mcp.Tool, error) {
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
	// Each input's schema is a document the engine compiles on its own, under treeBase.
	var documents []string
	for name := range properties {
		documents = append(documents, "/properties/"+pointerToken(name))
	}
	inputSchema, err := bundled(input, p.Files, documents...)
	if err != nil {
		return mcp.Tool{}, err
	}
	t := mcp.Tool{
		Name: o.tool, Title: block.Title, Description: block.Description,
		InputSchema: inputSchema,
	}
	if len(p.Output) > 0 && block.Mode != graph.ToolAsync {
		// What the result is: the output's envelope, whose items carry what the output's schema
		// says, since "a client is told the shape of a result and not only its text" and the
		// protocol holds a structured result to its outputSchema. An async tool answers the run it
		// started and never the output, so it publishes none: a structured result has to conform
		// to the outputSchema a tool publishes, and the run's identifier is no envelope.
		if t.OutputSchema, err = bundled(map[string]any{
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
		}, p.Files, "/properties/items/items/properties/data"); err != nil {
			return mcp.Tool{}, err
		}
	}
	if a := block.Annotations; a != (graph.Annotations{}) {
		t.Annotations = &mcp.Annotations{ReadOnlyHint: a.ReadOnlyHint, IdempotentHint: a.IdempotentHint, DestructiveHint: a.DestructiveHint, OpenWorldHint: a.OpenWorldHint}
	}
	t.Call = func(ctx context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
		return c.call(ctx, caller, collection, o, arguments, logs)
	}
	return t, nil
}

// bundled is a schema with every file of the tree it reaches embedded under $defs, each by its
// path, and every reference written in it rewritten into a pointer inside it, "#/$defs/<path>" and
// the place in the file, which every client resolves with no retrieval and no knowledge of a base.
// Each reference is first resolved as the engine resolves it, against treeBase moved by every $id
// met on the way to it; then the $ids, the anchors and each document's $schema are left out, since
// nothing is resolved against them any more and one left in would move the pointers beneath it.
// Kept, they defeated a client twice over: a file declaring an $id of its own was no longer the
// resource its path names, and Python's urljoin, which referencing and jsonschema resolve with,
// resolves nothing under the agk: scheme.
//
// documents are the places in root holding a schema the engine compiles as a document of its own,
// an input's or the output's, each resolving against treeBase alone. A reference that reaches no
// file the bundle holds refuses the bundle, and the tool with it, rather than publishing what a
// client would have to fetch; one under another scheme than agk is a meta-schema, which the engine
// knows without fetching it, as a client does, and is kept as written.
func bundled(root map[string]any, files map[string]any, documents ...string) (json.RawMessage, error) {
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	if len(files) > 0 {
		root["$defs"] = files
	}
	// Written and read back, numbers as written, so that what is rewritten is a copy: the files
	// are the publication kept for every list of the version.
	b, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var bundle any
	if err := d.Decode(&bundle); err != nil {
		return nil, err
	}

	// The resources a reference may name: each file by the URL its path is under and by every $id
	// it declares, and each document by treeBase and the $ids it declares, which only its own
	// references reach, since the engine compiles each on its own.
	type unit struct {
		at, base string
		own      *resources
	}
	tree := &resources{}
	var units []unit
	for _, at := range documents {
		own := &resources{}
		own.name(treeBase, at)
		units = append(units, unit{at: at, base: treeBase, own: own})
	}
	for _, path := range sortedKeys(files) {
		at := "/$defs/" + pointerToken(path)
		base := resourceOf(&url.URL{Scheme: "agk", Host: "repo", Path: "/" + path})
		tree.name(base, at)
		units = append(units, unit{at: at, base: base, own: tree})
	}
	for _, u := range units {
		node, _ := pointed(bundle, u.at)
		if err := eachSchema(node, u.at, u.base, func(o map[string]any, at, base string) error {
			if _, ok := o["$id"].(string); ok && !u.own.name(base, at) {
				return fmt.Errorf("api: two schemas of the tree declare %s, which a client could not tell apart", base)
			}
			for _, key := range []string{"$anchor", "$dynamicAnchor"} {
				if anchor, ok := o[key].(string); ok {
					u.own.anchor(base, anchor, at)
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for _, u := range units {
		node, _ := pointed(bundle, u.at)
		if err := eachSchema(node, u.at, u.base, func(o map[string]any, at, base string) error {
			if ref, ok := o["$ref"].(string); ok {
				to, err := inBundle(bundle, ref, base, u.own, tree)
				if err != nil {
					return fmt.Errorf("api: the schema at %s: %w", at, err)
				}
				o["$ref"] = to
			}
			delete(o, "$id")
			delete(o, "$anchor")
			delete(o, "$schema")
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return json.Marshal(bundle)
}

// resources are the schemas a reference resolves to by URL, each by the pointer to it in a bundle,
// and the anchors each declares.
type resources struct {
	at      map[string]string
	anchors map[string]map[string]string
}

// name records the resource at a URL, and answers false where another place already holds it.
func (r *resources) name(url, at string) bool {
	if r.at == nil {
		r.at = map[string]string{}
	}
	if held, ok := r.at[url]; ok && held != at {
		return false
	}
	r.at[url] = at
	return true
}

func (r *resources) anchor(url, anchor, at string) {
	if r.anchors == nil {
		r.anchors = map[string]map[string]string{}
	}
	if r.anchors[url] == nil {
		r.anchors[url] = map[string]string{}
	}
	r.anchors[url][anchor] = at
}

// inBundle is a reference as the pointer into the bundle to what it resolves to against base,
// looked up among a document's own resources first and then among the tree's.
func inBundle(bundle any, ref, base string, scopes ...*resources) (string, error) {
	from, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	written, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("%q is no reference: %w", ref, err)
	}
	u := from.ResolveReference(written)
	key := resourceOf(u)
	for _, s := range scopes {
		at, ok := s.at[key]
		if !ok {
			continue
		}
		switch fragment := u.Fragment; {
		case fragment == "":
		case strings.HasPrefix(fragment, "/"):
			at += fragment
		default:
			if at, ok = s.anchors[key][fragment]; !ok {
				return "", fmt.Errorf("%q names the anchor %s, which %s does not declare", ref, fragment, key)
			}
		}
		if _, ok := pointed(bundle, at); !ok {
			return "", fmt.Errorf("%q names %s, which holds nothing there", ref, u)
		}
		return (&url.URL{Fragment: at}).String(), nil
	}
	if u.Scheme != "agk" {
		return ref, nil
	}
	return "", fmt.Errorf("%q resolves to %s, which is no file the bundle holds", ref, key)
}

// resourceOf is the URL a resource is named by: the URL with no fragment, its path written as the
// engine's loader reads it, so that a path and a reference to it agree however either escapes it.
func resourceOf(u *url.URL) string {
	v := *u
	v.Fragment, v.RawFragment, v.RawPath = "", "", ""
	return v.String()
}

// The keywords of JSON Schema 2020-12 whose value is a schema, an object of schemas or a list of
// them, with the ones earlier drafts wrote, which a file may still carry: what eachSchema walks
// into, so that a $ref, an $id or an $anchor is read where it is a keyword, and never under enum,
// const, default or examples, where it is the value written.
var (
	schemaKeywords  = []string{"additionalProperties", "propertyNames", "items", "contains", "not", "if", "then", "else", "unevaluatedItems", "unevaluatedProperties", "contentSchema", "additionalItems"}
	schemasKeywords = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"}
	listKeywords    = []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"}
)

// eachSchema visits every schema object of node, the pointer to it and the base its references
// resolve against: base, moved by the $id of every object from node down to it, its own included.
func eachSchema(node any, at, base string, visit func(o map[string]any, at, base string) error) error {
	o, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if id, ok := o["$id"].(string); ok {
		from, err := url.Parse(base)
		if err != nil {
			return err
		}
		to, err := url.Parse(id)
		if err != nil {
			return fmt.Errorf("api: the schema at %s declares the $id %q, which is no URL: %w", at, id, err)
		}
		base = resourceOf(from.ResolveReference(to))
	}
	if err := visit(o, at, base); err != nil {
		return err
	}
	for _, key := range schemaKeywords {
		if _, list := o[key].([]any); !list {
			if err := eachSchema(o[key], at+"/"+pointerToken(key), base, visit); err != nil {
				return err
			}
		}
	}
	for _, key := range schemasKeywords {
		named, _ := o[key].(map[string]any)
		for _, name := range sortedKeys(named) {
			if err := eachSchema(named[name], at+"/"+pointerToken(key)+"/"+pointerToken(name), base, visit); err != nil {
				return err
			}
		}
	}
	for _, key := range listKeywords {
		listed, _ := o[key].([]any)
		for i, v := range listed {
			if err := eachSchema(v, at+"/"+pointerToken(key)+"/"+strconv.Itoa(i), base, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// pointed is what a JSON Pointer names in a document, and false where it names nothing.
func pointed(doc any, pointer string) (any, bool) {
	if pointer == "" {
		return doc, true
	}
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch v := doc.(type) {
		case map[string]any:
			var ok bool
			if doc, ok = v[token]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			doc = v[i]
		default:
			return nil, false
		}
	}
	return doc, true
}

// pointerToken is a name as one token of a JSON Pointer, as RFC 6901 escapes it.
func pointerToken(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
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
func (c *Collections) call(ctx context.Context, caller Caller, collection string, o offer, arguments json.RawMessage, logs failureLog) (*mcp.CallResult, error) {
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
			Content:           []mcp.Content{mcp.Text(fmt.Sprintf("Run %s of %s/%s started, at commit %s. Follow it with the user's server's run_get.", run, target.Namespace, target.Workflow, started.Commit))},
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
		// The output of one's own call is returned under workflow:run alone, "run:read_data not
		// asked", but "only an explicit deny removes, and it wins over any allow at any scope": a
		// caller denied run:read_data on the workflow is answered the run as an async call answers
		// it, asked when the output would be read, since a deny written meanwhile applies from the
		// next request.
		if block.Output != "" {
			denied, err := caller.Denied(ctx, RunReadData, target)
			if err != nil {
				return nil, err
			}
			if denied {
				return noOutput(fmt.Sprintf("Run %s of %s/%s succeeded. Its output %s is not returned to you: run:read_data is denied you on %s/%s, and a deny wins over the grant that let you call the tool. Calling it again would start another run, answered alike.", run, target.Namespace, target.Workflow, block.Output, target.Namespace, target.Workflow)), nil
			}
		}
		return c.succeeded(ctx, target.Namespace, detail, block.Output)
	case agk.Failed, agk.Cancelled, agk.TimedOut:
		return &mcp.CallResult{Content: []mcp.Content{mcp.Text(ended(detail, logs))}, StructuredContent: map[string]any{"run": string(run), "state": detail.State.String()}, IsError: true}, nil
	}
	// Still going when the wait is over: the run goes on, and the model is told how to follow it
	// rather than told it failed.
	return noOutput(fmt.Sprintf("Run %s of %s/%s is still %s after %s, the longest this call waits, and has not failed: it goes on. Follow it with the user's server's run_get rather than calling the tool again, which would start another run.", run, target.Namespace, target.Workflow, detail.State, wait)), nil
}

// noOutput answers a sync call whose answer is not the output it returns: its text alone, as a
// tool's error. A sync tool publishes the envelope as its outputSchema, and "the protocol holds a
// structured result to its outputSchema": structured content naming the run instead is refused by a
// client validating it, the answer with it, and a result carrying none that is not an error is
// refused too, by the revision's clients, of a tool publishing an outputSchema. An error is what a
// client hands the model as it is, so the text says what became of the run, which keeps the model
// from calling again for a run that goes on.
func noOutput(text string) *mcp.CallResult {
	return &mcp.CallResult{Content: []mcp.Content{mcp.Text(text)}, IsError: true}
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
// envelope as text beside it and the files it names embedded after, or where the tool returns no
// output, that the run succeeded.
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
	content := append([]mcp.Content{mcp.Text(strings.TrimSpace(text.String()))}, c.embedded(ctx, namespace, detail.Run, e)...)
	return &mcp.CallResult{Content: content, StructuredContent: structured}, nil
}

// embedded is the files an output's envelope names, as the content blocks that follow its text:
// each file of the call's own run as run_artifact answers it, text as text, an image as an image
// and any other file as an embedded resource, after a line naming it; and a line for each file not
// embedded, by its URI and its size, saying why.
//
// The output of one's own call is answered under workflow:run, run:read_data not asked, and a file
// it names is that output as much as an item's data is, or a call answers a model URIs no tool of
// the collection reads. So the files are read here, as the envelope is, rather than through the
// artifact route, which asks run:read_data; and they are read for the run the call started alone.
// A file of another run, which a cache hit or a call carries into an output naming it, is named
// and never read: the right a call answers under is over its own run, not over the one that made
// the file, which may be of another workflow or another namespace.
//
// What a call answers lands in the model's context, as what run_artifact answers does, which holds
// one file to mcpArtifactMost. A call embeds its files without the model asking for any, so the
// ceiling holds them all together, in the order the envelope names them, and a file that would
// carry them past it is named instead. A file with a fetch budget spends one, as any fetch does.
func (c *Collections) embedded(ctx context.Context, namespace string, run agk.RunID, e agk.Envelope) []mcp.Content {
	var blocks []mcp.Content
	var notes []string
	left := int64(mcpArtifactMost)
	seen := map[agk.URI]bool{}
	for _, item := range e.Items {
		for _, f := range item.Files {
			if seen[f.URI] {
				continue
			}
			seen[f.URI] = true
			if f.URI.Run != run {
				notes = append(notes, fmt.Sprintf("%s, %d bytes, is a file of run %s, which this output names and this call did not make: run_artifact on the user's server reads it, under run:read_data.", f.URI, f.Size, f.URI.Run))
				continue
			}
			var b []byte
			var kind string
			var err error
			if f.Size > left {
				// Said by the envelope as the reference says it, and nothing read to learn it.
				err = &pastCeiling{size: f.Size}
			} else {
				b, kind, err = c.fetched(ctx, namespace, f.URI, left)
			}
			var large *pastCeiling
			switch {
			case errors.As(err, &large) && large.size > mcpArtifactMost:
				notes = append(notes, fmt.Sprintf("%s, %d bytes, is above the %d bytes a call embeds, since what it answers lands in your context: it is fetched from GET /api/v1/artifacts/{uri} with an API token.", f.URI, large.size, mcpArtifactMost))
			case errors.As(err, &large):
				notes = append(notes, fmt.Sprintf("%s, %d bytes, would carry the files this call embeds past %d bytes together: run_artifact on the user's server reads it, under run:read_data.", f.URI, large.size, mcpArtifactMost))
			case errors.Is(err, db.ErrGone):
				notes = append(notes, fmt.Sprintf("%s, %d bytes, has expired or its fetches are spent: it existed, and is finished.", f.URI, f.Size))
			case errors.Is(err, db.ErrInFlight):
				notes = append(notes, fmt.Sprintf("%s, %d bytes, has every fetch left being served to somebody else, and one comes back if its transfer does not complete: run_artifact on the user's server reads it later, under run:read_data.", f.URI, f.Size))
			case err != nil:
				notes = append(notes, fmt.Sprintf("%s, %d bytes, could not be read.", f.URI, f.Size))
			default:
				left -= int64(len(b))
				blocks = append(blocks, mcp.Text(fmt.Sprintf("%s, %s, %d bytes:", f.URI, kind, len(b))), fileBlock(f.URI.String(), kind, b))
			}
		}
	}
	if len(notes) > 0 {
		blocks = append(blocks, mcp.Text("Not embedded:\n"+strings.Join(notes, "\n")))
	}
	return blocks
}

// pastCeiling is a file larger than what a call has left to embed, by its size.
type pastCeiling struct{ size int64 }

func (p *pastCeiling) Error() string { return fmt.Sprintf("the file is %d bytes", p.size) }

// fetched reads one file of the call's own run whole, refused above most bytes, as the artifact
// route answers a request through MCP: the bytes whatever the budget, of the media type its
// reference declares, a fetch of a budget held before they are read, and spent once all of them
// were and matched their digest or given back otherwise, on a context the call going away does
// not cancel.
func (c *Collections) fetched(ctx context.Context, namespace string, u agk.URI, most int64) ([]byte, string, error) {
	if c.objects == nil {
		return nil, "", errors.New("api: this installation has no object store attached, and an artifact is read from nowhere else")
	}
	var got db.Resolved
	err := c.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		got, err = ns.Resolve(ctx, u)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	if got.Size > most {
		return nil, "", &pastCeiling{size: got.Size}
	}
	kind := "application/octet-stream"
	if got.MediaType != "" {
		kind = got.MediaType
	}
	if got.Budgeted {
		var held time.Time
		err := c.pool.In(ctx, namespace, func(ctx context.Context, ns *db.NS) error {
			var err error
			got, held, err = ns.Reserve(ctx, u, fetchHold)
			return err
		})
		if err != nil {
			return nil, "", err
		}
		delivered := false
		defer func() {
			settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchSettling)
			defer cancel()
			c.pool.In(settle, namespace, func(ctx context.Context, ns *db.NS) error {
				if delivered {
					return ns.Delivered(ctx, u, held)
				}
				return ns.Release(ctx, u, held)
			})
		}()
		b, err := c.read(ctx, got)
		delivered = err == nil
		return b, kind, err
	}
	b, err := c.read(ctx, got)
	return b, kind, err
}

// read is the bytes behind a reference, refused where they are not all of what its digest names.
func (c *Collections) read(ctx context.Context, got db.Resolved) ([]byte, error) {
	rc, err := c.objects.Open(ctx, got.Key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, got.Size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if int64(len(b)) != got.Size || hex.EncodeToString(sum[:]) != got.Digest {
		return nil, fmt.Errorf("api: %s is not the %d bytes its digest names", got.URI, got.Size)
	}
	return b, nil
}

// ended says why a run ended without succeeding, for the model that called it to correct itself:
// "naming the failing step, its exit code and its message". The message is the end of the log of
// the task the step failed by, as logs reads it, the step's reason as the run detail words it, and
// the reason the run records where something other than its workflow ended it.
//
// The step's reason is said to whoever may call the tool, as its name and its exit code are: it
// names the rule that failed the step and never a value the step was handed, and for a step refused
// before any task started, its port fed what is no batch, it is all there is to say, with no exit
// code and no log behind it. The log is the route's to give, which may refuse it.
func ended(d db.RunDetail, logs failureLog) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s of %s ended %s.", d.Run, d.Workflow, d.State)
	var message []string
	for _, s := range d.Steps {
		if s.Verdict != agk.VerdictFailed {
			continue
		}
		fmt.Fprintf(&b, " Step %s failed", s.Step)
		if t, ok := failedBy(d.Tasks, s.Step); ok {
			if t.ExitCode != nil {
				fmt.Fprintf(&b, " with exit code %d", *t.ExitCode)
			}
			if logs != nil {
				message = logs(d.Run, s.Step, t.Task)
			}
		}
		if s.Reason != "" {
			b.WriteString(": " + s.Reason)
		}
		b.WriteString(".")
		break
	}
	if d.Reason != "" {
		b.WriteString(" " + d.Reason + ".")
	}
	if text := lastOf(message, failureMost); text != "" {
		b.WriteString(" Its log ends:\n" + text)
	}
	return b.String()
}

// failedBy is the task a step failed by: the last of its tasks that failed or was stopped at its
// deadline, and where none was, the last of them that exited, as a step whose tasks a cancel
// stopped has.
func failedBy(tasks []db.TaskSummary, step agk.Step) (db.TaskSummary, bool) {
	for _, failing := range []func(db.TaskSummary) bool{
		func(t db.TaskSummary) bool { return t.State == agk.TaskFailed || t.State == agk.TaskTimedOut },
		func(t db.TaskSummary) bool { return t.ExitCode != nil },
	} {
		for i := len(tasks) - 1; i >= 0; i-- {
			if tasks[i].Step == step && failing(tasks[i]) {
				return tasks[i], true
			}
		}
	}
	return db.TaskSummary{}, false
}

// lastOf is the end of lines at most most bytes long: whole lines where the last of them fits, the
// end of the last one, from where a character begins, where it alone does not.
func lastOf(lines []string, most int) string {
	text := strings.Join(lines, "\n")
	if len(text) <= most {
		return text
	}
	text = text[len(text)-most:]
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[i+1:]
	}
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return text
}
