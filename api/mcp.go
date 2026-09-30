package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
// It takes a bearer token and nothing else. "An ordinary API bearer token is also accepted, so a
// service account needs no browser", beside the OAuth tokens a later release accepts; a session
// cookie is the console's credential, which no MCP client holds, and a request carrying one alone is
// answered as one carrying nothing.

// mcpInstructions is the discovery's orientation, and "it carries nothing else": what a workflow is,
// where its entry point is, and the order a client drafting one reads, validates and commits in.
const mcpInstructions = "A workflow is a git repository, and its entry point is agentiik.yaml at the root of the repository. Before drafting one, read the language with workflow.language: with no topic it gives an orientation, a complete minimal workflow and the list of topics. Before committing a draft, validate it with workflow.validate, which is the only authority on whether it is legal, and read the topic each error names."

// MCPOptions is what the platform's MCP server is built with.
type MCPOptions struct {
	// PublicURL is the installation's public URL, whose origin is the one a browser may call the
	// endpoint from.
	PublicURL string

	// Version is the program's, which the server names itself with.
	Version string
}

// MCP is the platform's server.
type MCP struct {
	server mcp.Server
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
	m := &MCP{server: mcp.Server{
		Info:         mcp.Implementation{Name: "agentiik", Version: version},
		Instructions: mcpInstructions,
		Origin:       origin,
		Resources:    true,
	}}
	if err := rt.HandleOwn("POST", "/mcp", Own{}, m.serve); err != nil {
		return nil, err
	}
	return m, nil
}

// serve answers one request to /mcp, as the caller who presented a bearer token.
func (m *MCP) serve(w http.ResponseWriter, r *http.Request, caller Caller) {
	if _, bearer := bearerOf(r); !bearer {
		w.Header().Set("WWW-Authenticate", "Bearer")
		refuse(w, http.StatusUnauthorized, "the MCP endpoint takes a bearer token, an API token or one issued for this installation, and a session is the console's")
		return
	}
	m.server.Serve(w, r, m.offered(caller))
}

// offered is what one caller is offered. Every tool and resource here is one "any authenticated
// principal" may use, since the language and the schemas are what every client needs before it
// can ask for anything else and they say nothing about any namespace.
func (m *MCP) offered(Caller) mcp.Surface {
	return mcp.Surface{
		Tools:     []mcp.Tool{workflowLanguage(), workflowSchema()},
		Resources: languageResources(),
		Templates: []mcp.Template{
			{URITemplate: "agentiik://language/{topic}", Name: "language", Title: "A page of the language reference", Description: "One topic of the workflow language, as workflow.language answers it, for a client that prefers attaching documents to calling a tool.", MimeType: "text/markdown"},
			{URITemplate: "agentiik://schema/{part}", Name: "schema", Title: "A schema document", Description: "The JSON Schema 2020-12 document of the entry point (workflow), a brick manifest (brick) or an envelope (envelope), as workflow.schema answers it.", MimeType: "application/schema+json"},
		},
		Read: readLanguage,
	}
}

// readOnly are the annotations of a tool that reads and changes nothing: "the read-only tools carry
// readOnlyHint, so a client can tell without being told", and reading twice reads the same.
func readOnly() *mcp.Annotations {
	return &mcp.Annotations{ReadOnlyHint: mcp.Hint(true), DestructiveHint: mcp.Hint(false), IdempotentHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)}
}

// workflowLanguage is workflow.language: "a topic returns its page, no topic returns the
// orientation, a minimal workflow and the topic list", each page the one agentiik/schemas generates
// from the schema the validator enforces.
func workflowLanguage() mcp.Tool {
	topics, _ := json.Marshal(language.Names())
	return mcp.Tool{
		Name:        "workflow.language",
		Title:       "Learn the workflow language",
		Description: "The workflow language, one topic at a time: a summary, the keywords the topic covers, the JSON Schema fragment governing them and worked examples written where they go in agentiik.yaml. With no topic, an orientation: what the language is, one complete minimal workflow and the topics. Read it before drafting a workflow, and read the topic a validation error names before correcting it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string","description":"The topic to read. Leave it out for the orientation.","enum":` + string(topics) + `}},"additionalProperties":false}`),
		Annotations: readOnly(),
		Call: func(_ context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
			var in struct {
				Topic *string `json:"topic"`
			}
			if err := strictly(arguments, &in); err != nil {
				return nil, mcp.InvalidParams("workflow.language takes one argument, topic: %v", err)
			}
			if in.Topic == nil {
				return &mcp.CallResult{Content: []mcp.Content{mcp.Text(language.Orientation())}}, nil
			}
			page, ok := language.Page(*in.Topic)
			if !ok {
				return nil, mcp.InvalidParams("there is no topic called %q: the topics are %s", *in.Topic, strings.Join(language.Names(), ", "))
			}
			return &mcp.CallResult{Content: []mcp.Content{mcp.Text(page)}}, nil
		},
	}
}

// workflowSchema is workflow.schema: "the JSON Schema 2020-12 document for the entry point, a brick
// manifest or an envelope", "for local validation or type generation". It answers the document as
// text and as structured content, which the revision lets be any JSON value.
func workflowSchema() mcp.Tool {
	var parts []string
	for _, p := range language.Parts() {
		parts = append(parts, p.Name)
	}
	names, _ := json.Marshal(parts)
	return mcp.Tool{
		Name:        "workflow.schema",
		Title:       "Get a schema document",
		Description: "The JSON Schema 2020-12 document of the entry point (workflow), a brick manifest (brick) or an envelope (envelope), for validating locally or generating types. A document the schema accepts can still be refused, for an input it reads and never declares or a cycle in its graph: workflow.validate is the only authority on whether a draft is legal.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"part":{"type":"string","description":"The document to answer: workflow for agentiik.yaml, brick for /agk/brick.yaml, envelope for what travels along a port.","enum":` + string(names) + `}},"required":["part"],"additionalProperties":false}`),
		Annotations: readOnly(),
		Call: func(_ context.Context, arguments json.RawMessage) (*mcp.CallResult, error) {
			var in struct {
				Part *string `json:"part"`
			}
			if err := strictly(arguments, &in); err != nil || in.Part == nil {
				return nil, mcp.InvalidParams("workflow.schema takes one argument, part, which is one of %s", strings.Join(parts, ", "))
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
