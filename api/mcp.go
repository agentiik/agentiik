package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agentiik/agentiik/access"
	"github.com/agentiik/agentiik/language"
	"github.com/agentiik/agentiik/mcp"
)

// The platform's MCP server, at /mcp: "Its tools mirror the API and hold no authority of their own:
// each call runs as the presenting principal, under the grants that govern the console."
//
// It is a route like any other, registered with Own: the caller is identified before anything is
// read, a request with no credential is refused, and the handler is given who asks and nothing it
// could act on in their stead. The protocol is package mcp's; what this file adds is who is offered
// what. A tool the caller cannot use is absent rather than refused, as the documentation decides and
// the revision allows, since "the set MAY vary by the authorization presented on the request".
//
// It takes a bearer token and nothing else: an OAuth token issued for it by the installation's own
// authorisation server (oauth.go), or "an ordinary API bearer token is also accepted, so a service
// account needs no browser". A session cookie is the console's credential, which no MCP client holds,
// and a request carrying one alone is answered as one carrying nothing. Where the installation issues
// OAuth tokens, a request answered 401 is told where to ask for one, its WWW-Authenticate naming the
// surface's Protected Resource Metadata.

// mcpInstructions is the discovery's orientation, and "it carries nothing else": what a workflow is,
// where its entry point is, and the order a client drafting one reads, validates and commits in.
const mcpInstructions = "A workflow is a git repository, and its entry point is agentiik.yaml at the root of the repository. Before drafting one, read the language with workflow_language: with no topic it gives an orientation, a complete minimal workflow and the list of topics. Before committing a draft, validate it with workflow_validate, which is the only authority on whether it is legal, and read the topic each error names."

// MCPOptions is what the platform's MCP server is built with.
type MCPOptions struct {
	// PublicURL is the installation's public URL, whose origin is the one a browser may call the
	// endpoint from.
	PublicURL string

	// Version is the program's, which the server names itself with.
	Version string

	// Collections are the principals' collections, each served at /mcp/collections/{id} beside the
	// user's server, and none where nil.
	Collections *Collections

	// OAuth is the installation's own authorisation server, which the surfaces' challenges name, and
	// nil where it issues no token, whose surfaces answer Bearer alone.
	OAuth *OAuth
}

// MCP is the platform's server.
type MCP struct {
	server mcp.Server

	// api is what the tools make their requests of, as the caller: the router, held as a handler
	// and as nothing more, so that all a tool can do is ask a route, which checks it as it checks
	// anybody, and never read a store or ask the authorizer in the caller's stead.
	api http.Handler

	// collections are what a collection's endpoint offers, and collectionServer the protocol it
	// speaks it in: the user's server's, offering no resources.
	collections      *Collections
	collectionServer mcp.Server

	// oauth is the authorisation server the challenges name, nil for none, and base the public URL
	// every surface's URL is under, which a request a tool makes carries as the resource it is for.
	oauth *OAuth
	base  string
}

// NewMCP builds the platform's server and registers it at /mcp, on POST. The router answers any
// other method at /mcp with 405 as the revision asks, since the route is served on POST alone.
func NewMCP(rt *Router, o MCPOptions) (*MCP, error) {
	origin, err := originOf(o.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("api: the MCP endpoint checks the Origin of a browser's request against the installation's: %w", err)
	}
	version := o.Version
	if version == "" {
		version = "(devel)"
	}
	m := &MCP{api: rt, server: mcp.Server{
		Info:         mcp.Implementation{Name: "agentiik", Version: version},
		Instructions: mcpInstructions,
		Origin:       origin,
		Resources:    true,
	}}
	m.oauth, m.base = o.OAuth, strings.TrimRight(o.PublicURL, "/")
	guard := Own{}
	if m.oauth != nil {
		guard.Challenge = m.challenge
	}
	if err := rt.HandleOwn("POST", mcpPath, guard, m.serve); err != nil {
		return nil, err
	}
	if o.Collections != nil {
		m.collections = o.Collections
		m.collectionServer = mcp.Server{
			Info:         mcp.Implementation{Name: "agentiik", Version: version},
			Instructions: collectionInstructions,
			Origin:       origin,
		}
		if err := rt.HandleOwn("POST", collectionsPath+"{id}", guard, m.serveCollection); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// challenge is the WWW-Authenticate a surface answers a request identifying nobody with: the one the
// authorisation server writes for the surface the request is to, with error="invalid_token" where a
// token came and opened nothing.
func (m *MCP) challenge(r *http.Request, invalid bool) string {
	why := ""
	if invalid {
		why = "invalid_token"
	}
	return m.oauth.Challenge(r.URL.Path, why, nil)
}

// noBearer answers a request to a surface that carries a session and no bearer token: 401, asking for
// a token as a request carrying nothing is asked.
func (m *MCP) noBearer(w http.ResponseWriter, r *http.Request, why string) {
	if m.oauth != nil {
		w.Header().Set("WWW-Authenticate", m.challenge(r, false))
	} else {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	refuse(w, http.StatusUnauthorized, why)
}

// insufficientScope is the refusal of a call or a read an OAuth token's scope does not reach and its
// person does.
const insufficientScope = "the token was not allowed a permission this needs, which its person holds: the client asks them for the scopes WWW-Authenticate names, beside those it holds, and calls again"

// serve answers one request to /mcp, as the caller who presented a bearer token.
//
// A call or a read by an OAuth token needing a permission its scope lacks, and its person holds, is
// answered 403 before anything is asked of the API: "the server SHOULD respond with HTTP 403
// Forbidden", WWW-Authenticate's error="insufficient_scope" and "scope, specifying the minimum scopes
// needed for the operation", "all scopes required for the current operation in a single challenge".
// What it is a call or a read of is read from Mcp-Method and Mcp-Name, which the transport has mirror
// the body and package mcp refuses where they do not.
func (m *MCP) serve(w http.ResponseWriter, r *http.Request, caller Caller) {
	if _, bearer := bearerOf(r); !bearer {
		m.noBearer(w, r, "the MCP endpoint takes a bearer token, an API token or one issued for this installation, and a session is the console's")
		return
	}
	surface, up, err := m.offered(r, caller)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "what the caller holds could not be read, and the tools offered are what it holds")
		return
	}
	if needed := up.neededBy(r); len(needed) > 0 && m.oauth != nil {
		w.Header().Set("WWW-Authenticate", m.oauth.Challenge(mcpPath, "insufficient_scope", permissionNames(needed)))
		refuse(w, http.StatusForbidden, insufficientScope)
		return
	}
	m.server.Serve(w, r, surface)
}

// stepUp is what an OAuth token could be allowed that its scope lacks: the permissions each tool its
// person may use needs and the token does not reach, and those a read of the tree, of a run or of a
// run's file needs, none where the token reaches it or its person does not.
type stepUp struct {
	tools               map[string][]Permission
	tree, run, artifact []Permission
}

// neededBy is what the request needs that the token lacks: a call of a tool, by its name or a
// former one, or a read of a resource, as its Mcp-Method and Mcp-Name say, and nothing for any other.
func (s stepUp) neededBy(r *http.Request) []Permission {
	name := r.Header.Get("Mcp-Name")
	switch r.Header.Get("Mcp-Method") {
	case "tools/call":
		if now := renamed(name); now != "" {
			name = now
		}
		return s.tools[name]
	case "resources/read":
		switch {
		case strings.HasPrefix(name, "agentiik://run/"):
			return s.run
		case strings.HasPrefix(name, "agk://run/"):
			return s.artifact
		case strings.HasPrefix(name, "agentiik://language/"), strings.HasPrefix(name, "agentiik://schema/"):
			return nil
		case strings.HasPrefix(name, "agentiik://"):
			return s.tree
		}
	}
	return nil
}

// needed is what a caller holding have needs besides to be offered a tool whole offers it: each
// permission that, added wherever whole holds it, offers the tool, or where no one of them alone does,
// every one whole holds and have lacks.
func needed(offered func(Effective) bool, have, whole Effective) []Permission {
	var out []Permission
	for _, p := range access.Permissions {
		if offered(adding(have, whole, p)) {
			out = append(out, p)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, p := range access.Permissions {
		if holds(p)(whole) && !holds(p)(have) {
			out = append(out, p)
		}
	}
	return out
}

// adding is have with p added at every scope where whole holds it.
func adding(have, whole Effective, p Permission) Effective {
	out := Effective{Admin: have.Admin, Groups: have.Groups, Permissions: map[access.Scope]access.Set{}}
	for at, set := range have.Permissions {
		out.Permissions[at] = set
	}
	for at, set := range whole.Permissions {
		if set.Has(p) {
			out.Permissions[at] = access.SetOf(append(out.Permissions[at].Permissions(), p)...)
		}
	}
	return out
}

// offered is what one caller is offered: "the tool list is filtered by the caller's permissions: a
// tool the caller cannot use is absent rather than refused, as the protocol allows". The language
// and the schemas are any authenticated principal's, since they are what every client needs before
// it can ask for anything else and they say nothing about any namespace. Every other tool is listed
// to a caller holding, somewhere, what its route needs, as GET /api/v1/me answers what they hold;
// the route still decides each call, over the namespace and the workflow it names.
//
// An authorizer that does not say what a principal is granted, as a router built for a test may
// carry, lists every tool, each refused by its route as anybody is.
//
// An OAuth token is listed what its person holds through the nine permissions, whichever its scope
// keeps, and answered what it could be allowed besides (stepUp): a permission its person holds is
// one its client can ask them for, which the revision has a client do on a 403 naming it and call
// again, so the tool is offered with the way to it rather than hidden. What no scope gives, an
// administrator's tools and the collections', stays absent, since the token is narrowed whatever it
// is allowed. An API token narrowed by a scope is listed what it keeps, since nothing can widen it.
func (m *MCP) offered(r *http.Request, caller Caller) (mcp.Surface, stepUp, error) {
	tools := []mcp.Tool{workflowLanguage(), workflowSchema()}
	effective, err := caller.Effective(r.Context())
	known := err == nil
	if err != nil && caller.standings != nil {
		return mcp.Surface{}, stepUp{}, err
	}
	listed := effective
	var up stepUp
	if known && caller.Audience != "" {
		whole, err := caller.throughEveryPermission().Effective(r.Context())
		if err != nil {
			return mcp.Surface{}, stepUp{}, err
		}
		listed, up.tools = whole, map[string][]Permission{}
	}
	use := func(offered func(Effective) bool) bool { return !known || offered(listed) }
	// offer says whether a tool is offered, and where the token reaches less than its person, what
	// it would need to be allowed to call it.
	offer := func(name string, offered func(Effective) bool) bool {
		if !use(offered) {
			return false
		}
		if up.tools != nil && !offered(effective) {
			up.tools[name] = needed(offered, effective, listed)
		}
		return true
	}
	if offer("workflow_validate", holds(WorkflowRead)) {
		tools = append(tools, m.workflowValidate(r))
	}
	if offer("workflow_commit", holds(WorkflowWrite)) {
		tools = append(tools, m.workflowCommit(r))
	}
	// Left out even where the authorizer does not say what is granted, since what keeps these
	// credentials from those routes is what they are and not a grant: a narrowed token's scope, a
	// service account's being nobody's person, and the bootstrap token's being nobody at all.
	_, _, owner := owned(caller)
	account := strings.Contains(string(caller.Principal), "/")
	for _, t := range userTools() {
		if t.own && (caller.Narrowed() || !owner) {
			continue
		}
		if t.person && (caller.Narrowed() || account) {
			continue
		}
		if offer(t.name, t.offered) {
			tools = append(tools, m.tool(r, t))
		}
	}
	// The resources, each listed to whoever holds its tool's permission somewhere, as the tool is,
	// and read through its tool's route, which decides each read.
	templates := []mcp.Template{
		{URITemplate: "agentiik://language/{topic}", Name: "language", Title: "A page of the language reference", Description: "One topic of the workflow language, as workflow_language answers it, for a client that prefers attaching documents to calling a tool.", MimeType: "text/markdown"},
		{URITemplate: "agentiik://schema/{part}", Name: "schema", Title: "A schema document", Description: "The JSON Schema 2020-12 document of the entry point (workflow), a brick manifest (brick) or an envelope (envelope), as workflow_schema answers it.", MimeType: "application/schema+json"},
	}
	if use(holds(WorkflowRead)) {
		templates = append(templates, treeTemplate)
		if up.tools != nil && !holds(WorkflowRead)(effective) {
			up.tree = needed(holds(WorkflowRead), effective, listed)
		}
	}
	if use(holds(RunRead)) {
		templates = append(templates, runTemplate)
		if up.tools != nil && !holds(RunRead)(effective) {
			up.run = needed(holds(RunRead), effective, listed)
		}
	}
	if use(holds(RunReadData)) {
		templates = append(templates, artifactTemplate)
		if up.tools != nil && !holds(RunReadData)(effective) {
			up.artifact = needed(holds(RunReadData), effective, listed)
		}
	}
	return mcp.Surface{
		Tools:     tools,
		Resources: languageResources(),
		Templates: templates,
		Read:      m.reader(r),
		Renamed:   renamed,
	}, up, nil
}

// renamed is the name a tool goes by now, for a call by the dotted name it went by until v0.7.2,
// run.list for run_list, and "" for any other name. The protocol allows a dot in a tool's name and
// the Anthropic Messages API does not, holding a tool's name to ^[a-zA-Z0-9_-]{1,64}$, so a client
// of that API renamed run.list to run_list and was answered that no such tool exists; every tool
// is named with an underscore since, as a collection's already were. A client calls by the names it
// was listed and keeps them, so the dotted name is still answered, unlisted, for the rest of the
// v0.7 line, and v0.8.0 removes it. No tool's name holds a dot now, so none is shadowed.
func renamed(former string) string {
	if !strings.Contains(former, ".") {
		return ""
	}
	return strings.ReplaceAll(former, ".", "_")
}

// readOnly are the annotations of a tool that reads and changes nothing: "the read-only tools carry
// readOnlyHint, so a client can tell without being told", and reading twice reads the same.
func readOnly() *mcp.Annotations {
	return &mcp.Annotations{ReadOnlyHint: mcp.Hint(true), DestructiveHint: mcp.Hint(false), IdempotentHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)}
}

// workflowLanguage is workflow_language: "a topic returns its page, no topic returns the
// orientation, a minimal workflow and the topic list", each page the one agentiik/schemas generates
// from the schema the validator enforces.
func workflowLanguage() mcp.Tool {
	topics, _ := json.Marshal(language.Names())
	return mcp.Tool{
		Name:        "workflow_language",
		Title:       "Learn the workflow language",
		Description: "The workflow language, one topic at a time: a summary, the keywords the topic covers, the JSON Schema fragment governing them and worked examples written where they go in agentiik.yaml. With no topic, an orientation: what the language is, one complete minimal workflow and the topics. Read it before drafting a workflow, and read the topic a validation error names before correcting it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string","description":"The topic to read. Leave it out for the orientation.","enum":` + string(topics) + `}},"additionalProperties":false}`),
		Annotations: readOnly(),
		Call: func(_ context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
			var in struct {
				Topic *string `json:"topic"`
			}
			if err := strictly(arguments, &in); err != nil {
				return nil, mcp.InvalidParams("workflow_language takes one argument, topic: %v", err)
			}
			if in.Topic == nil {
				return &mcp.CallResult{Content: []mcp.Content{mcp.Text(language.Orientation())}}, nil
			}
			page, ok := language.Page(*in.Topic)
			if !ok {
				return nil, mcp.InvalidParams("%s", language.Unknown(*in.Topic))
			}
			return &mcp.CallResult{Content: []mcp.Content{mcp.Text(page)}}, nil
		},
	}
}

// workflowSchema is workflow_schema: "the JSON Schema 2020-12 document for the entry point, a brick
// manifest or an envelope", "for local validation or type generation". It answers the document as
// text and as structured content, which the revision lets be any JSON value.
func workflowSchema() mcp.Tool {
	var parts []string
	for _, p := range language.Parts() {
		parts = append(parts, p.Name)
	}
	names, _ := json.Marshal(parts)
	return mcp.Tool{
		Name:        "workflow_schema",
		Title:       "Get a schema document",
		Description: "The JSON Schema 2020-12 document of the entry point (workflow), a brick manifest (brick) or an envelope (envelope), for validating locally or generating types. A document the schema accepts can still be refused, for an input it reads and never declares or a cycle in its graph: workflow_validate is the only authority on whether a draft is legal.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"part":{"type":"string","description":"The document to answer: workflow for agentiik.yaml, brick for /agk/brick.yaml, envelope for what travels along a port.","enum":` + string(names) + `}},"required":["part"],"additionalProperties":false}`),
		Annotations: readOnly(),
		Call: func(_ context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
			var in struct {
				Part *string `json:"part"`
			}
			if err := strictly(arguments, &in); err != nil || in.Part == nil {
				return nil, mcp.InvalidParams("workflow_schema takes one argument, part, which is one of %s", strings.Join(parts, ", "))
			}
			b, ok := language.Schema(*in.Part)
			if !ok {
				return nil, mcp.InvalidParams("there is no part called %q: the parts are %s", *in.Part, strings.Join(parts, ", "))
			}
			return &mcp.CallResult{Content: []mcp.Content{mcp.Text(string(b))}, StructuredContent: json.RawMessage(b)}, nil
		},
	}
}

// strictly decodes a call's arguments, refusing a key the tool does not take rather than ignoring
// it: a model that misspells an argument is better told than answered as if it had not written it.
// No arguments at all are an empty object.
func strictly(arguments json.RawMessage, into any) error {
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	d := json.NewDecoder(bytes.NewReader(arguments))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	if d.More() {
		return errors.New("the arguments are more than one object")
	}
	return nil
}

// languageResources are the pages and the schema parts, each named where a client lists resources.
func languageResources() []mcp.Resource {
	var out []mcp.Resource
	for _, t := range language.Topics() {
		out = append(out, mcp.Resource{URI: "agentiik://language/" + t.Name, Name: t.Name, Description: t.Summary, MimeType: "text/markdown"})
	}
	for _, p := range language.Parts() {
		out = append(out, mcp.Resource{URI: "agentiik://schema/" + p.Name, Name: p.Name, Title: p.Title, Description: p.Summary, MimeType: "application/schema+json"})
	}
	return out
}

// readLanguage reads agentiik://language/{topic} and agentiik://schema/{part}, and answers any other
// URI as not found.
func readLanguage(_ context.Context, uri string) ([]mcp.Contents, error) {
	if topic, ok := strings.CutPrefix(uri, "agentiik://language/"); ok {
		if page, ok := language.Page(topic); ok {
			return []mcp.Contents{{URI: uri, MimeType: "text/markdown", Text: page}}, nil
		}
	}
	if part, ok := strings.CutPrefix(uri, "agentiik://schema/"); ok {
		if b, ok := language.Schema(part); ok {
			return []mcp.Contents{{URI: uri, MimeType: "application/schema+json", Text: string(b)}}, nil
		}
	}
	return nil, mcp.InvalidParams("Resource not found: %s", uri)
}
