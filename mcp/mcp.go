// Package mcp is the Model Context Protocol as Agentiik speaks it: revision 2026-07-28, over the
// Streamable HTTP transport, as a server and never as a client.
//
// "MCP puts no model in the engine. Both surfaces are servers: Agentiik publishes tools and answers
// calls, and any model lives in the client, beyond a protocol boundary the engine never crosses."
// This package is that boundary and nothing behind it. It reads a request, holds it to what the
// revision requires of a request (its version, its headers, its shape), and answers from a Surface:
// the tools and resources the caller of this one request is offered, which whoever serves the
// endpoint decides and hands in. It decides nothing about who may do what, because neither endpoint
// "holds rights of its own": each is a facade over the API, and a tool's work is the API's.
//
// The revision is stateless. There is no initialize, no session and no Mcp-Session-Id, and no GET
// stream: every request is a POST carrying its version and the client's capabilities in its own
// _meta, so a request is answered from itself alone. Every result carries resultType; a list, a
// discovery and a read carry the caching hints the revision requires.
//
// Standard library only: it is linked into the API process, and a protocol this size is cheaper to
// read here, rule by rule against the revision, than to trust a library tracking another one.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Revision is the one protocol revision served, the one the documentation names.
const Revision = "2026-07-28"

// MaxBody is the most a request's body may weigh. A request is one JSON-RPC message, and the
// heaviest the platform's tools take is a commit's files, which the API bounds far below this;
// anything over it is not a message a client meant to send.
const MaxBody = 8 << 20

// The JSON-RPC error codes the revision answers with: JSON-RPC 2.0's own, and the three the
// revision allocates from the range it reserves.
const (
	CodeParseError         = -32700
	CodeInvalidRequest     = -32600
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
	CodeInternalError      = -32603
	CodeHeaderMismatch     = -32020
	CodeUnsupportedVersion = -32022
)

// The _meta keys a request and a result carry.
const (
	metaVersion      = "io.modelcontextprotocol/protocolVersion"
	metaCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo   = "io.modelcontextprotocol/serverInfo"
)

// Implementation names the server, in every result's _meta and in its discovery.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Annotations are the protocol's hints about a tool, "hints to a client, never a permission".
type Annotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// Hint is a hint's value, for an Annotations literal.
func Hint(b bool) *bool { return &b }

// Tool is one tool a caller is offered: its definition as tools/list gives it, and its work.
type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  *Annotations    `json:"annotations,omitempty"`

	// Call does the tool's work with the arguments of one call, an object or nothing. It answers a
	// failure the caller can correct as a result with IsError set, an argument the tool cannot
	// take as an *Error with CodeInvalidParams, and anything else as an error, which the client is
	// told only that the call could not be answered.
	Call func(ctx context.Context, arguments json.RawMessage) (*CallResult, error) `json:"-"`
}

// Content is one block of a tool's result. Only text is sent: a tool's output is an envelope or a
// document, which is JSON, and an image or an audio clip is something no tool here produces.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text is a text block.
func Text(s string) Content { return Content{Type: "text", Text: s} }

// CallResult is what a tool answers a call with.
type CallResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Resource is one resource resources/list names.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Template is a family of resources, named by a URI template, that resources/templates/list names.
type Template struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Contents is what a read answers: a resource's text, and what it is.
type Contents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
}

// Surface is what one request's caller is offered. The tools are listed in the order given, which
// has to be the same from one request to the next for the same caller, since the revision asks for
// a deterministic order a client can cache and a model's prompt cache can reuse.
type Surface struct {
	Tools     []Tool
	Resources []Resource
	Templates []Template

	// Read answers resources/read for a URI, and an *Error with CodeInvalidParams for one that
	// names nothing the caller may read: the revision's answer to a resource not found, which is
	// the same whether it is absent or out of reach.
	Read func(ctx context.Context, uri string) ([]Contents, error)
}

// Error is a JSON-RPC error: a protocol error, as opposed to a tool's result that says it failed.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp: %d %s", e.Code, e.Message) }

// InvalidParams is the error a tool or a read answers an argument it cannot take with.
func InvalidParams(format string, a ...any) *Error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, a...)}
}

// Server is one MCP endpoint: what it says of itself, and the origin a browser may call it from.
type Server struct {
	Info Implementation

	// Instructions is the discovery's orientation, "natural-language guidance for LLMs on how to
	// use this server effectively", and nothing else.
	Instructions string

	// Origin is the installation's own origin, scheme and host, the one Origin a request may carry.
	Origin string

	// Resources says whether the server offers resources at all. A workflow's server does not: "a
	// resource would turn workflow:run into a read path into the repository".
	Resources bool
}

// request is one JSON-RPC message as it arrives.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// meta is what every request's params carry.
type meta struct {
	Meta map[string]json.RawMessage `json:"_meta"`
}

// Serve answers one request to the endpoint, from the surface its caller is offered.
//
// The HTTP status speaks of the transport and the body of the call: a request the transport refuses,
// a version the server does not speak, a header that disagrees with the body or a message that is
// no JSON-RPC request, is a 400, a method the server does not have a 404, and anything a method
// answers, an error of its own included, a 200. That is the split the revision draws, and it is the
// one a client falling back to an older revision reads: a 400 or a 404 whose body is a JSON-RPC error
// is a modern server telling it what to put right.
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, surface Surface) {
	if r.Method != http.MethodPost {
		// "HTTP GET or DELETE to the MCP endpoint: respond with 405 Method Not Allowed." There is
		// no stream to open and no session to end.
		w.Header().Set("Allow", http.MethodPost)
		answer(w, http.StatusMethodNotAllowed, nil, nil, &Error{Code: CodeInvalidRequest, Message: "the endpoint takes a POST, one JSON-RPC request each: revision " + Revision + " opens no stream and keeps no session"})
		return
	}
	// "Servers MUST validate the Origin header on all incoming connections to prevent DNS
	// rebinding attacks. If the Origin header is present and invalid, servers MUST respond with
	// HTTP 403 Forbidden." A client that is no browser sends none; one that is may call from the
	// installation's own pages and from nowhere else.
	if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || origins[0] != s.Origin) {
		answer(w, http.StatusForbidden, nil, nil, &Error{Code: CodeInvalidRequest, Message: "a browser may call this endpoint from the installation's own origin and from no other"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		answer(w, http.StatusBadRequest, nil, nil, &Error{Code: CodeInvalidRequest, Message: "the request's body could not be read"})
		return
	}
	if len(body) > MaxBody {
		answer(w, http.StatusRequestEntityTooLarge, nil, nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("a request weighs at most %d bytes", MaxBody)})
		return
	}
	var in request
	if err := json.Unmarshal(body, &in); err != nil {
		// A batch is an array, which JSON-RPC allows and this revision does not: "The body of the
		// HTTP POST MUST be a single JSON-RPC request or notification."
		answer(w, http.StatusBadRequest, nil, nil, &Error{Code: CodeParseError, Message: "the body is not one JSON-RPC message"})
		return
	}
	switch {
	case in.JSONRPC != "2.0":
		answer(w, http.StatusBadRequest, nil, nil, &Error{Code: CodeInvalidRequest, Message: `a message carries "jsonrpc": "2.0"`})
		return
	case in.Result != nil || in.Error != nil || in.Method == nil:
		answer(w, http.StatusBadRequest, nil, nil, &Error{Code: CodeInvalidRequest, Message: "a client sends requests and notifications, never a response"})
		return
	case in.ID == nil:
		// A notification. The revision defines none a client sends over this transport, and one
		// the server does not act on is still accepted, as "the server MUST return 202 Accepted".
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id := in.ID
	if !validID(id) {
		answer(w, http.StatusBadRequest, nil, nil, &Error{Code: CodeInvalidRequest, Message: "a request's id is a string or a number"})
		return
	}
	method := *in.Method

	if refusal := s.checked(r, method, in.Params); refusal != nil {
		answer(w, http.StatusBadRequest, id, nil, refusal)
		return
	}

	result, refusal := s.answered(r.Context(), method, in.Params, surface)
	switch {
	case refusal != nil && refusal.Code == CodeMethodNotFound:
		answer(w, http.StatusNotFound, id, nil, refusal)
	case refusal != nil:
		answer(w, http.StatusOK, id, nil, refusal)
	default:
		result["resultType"] = "complete"
		result["_meta"] = map[string]any{metaServerInfo: s.Info}
		answer(w, http.StatusOK, id, result, nil)
	}
}

// checked holds a request to what the revision asks of every request before its method is read:
// the version it declares, in its header and in its _meta, and the headers that mirror its body.
// It answers nil for a request that holds, and the error to refuse it with otherwise.
func (s *Server) checked(r *http.Request, method string, params json.RawMessage) *Error {
	header := r.Header.Values("MCP-Protocol-Version")
	if len(header) != 1 {
		return &Error{Code: CodeHeaderMismatch, Message: "a request carries one MCP-Protocol-Version header"}
	}
	// A version the server does not speak is answered as such before anything else is read, so
	// that a client of another revision learns which one to speak rather than which header of
	// its own it got wrong.
	if header[0] != Revision {
		return &Error{Code: CodeUnsupportedVersion, Message: "Unsupported protocol version", Data: map[string]any{"supported": []string{Revision}, "requested": header[0]}}
	}
	var m meta
	if len(params) > 0 && json.Unmarshal(params, &m) != nil {
		return &Error{Code: CodeInvalidRequest, Message: "a request's params are an object"}
	}
	var declared string
	if raw, ok := m.Meta[metaVersion]; !ok || json.Unmarshal(raw, &declared) != nil || declared != header[0] {
		return &Error{Code: CodeHeaderMismatch, Message: "the MCP-Protocol-Version header does not match the protocol version the request's _meta declares"}
	}
	if methods := r.Header.Values("Mcp-Method"); len(methods) != 1 || methods[0] != method {
		return &Error{Code: CodeHeaderMismatch, Message: "the Mcp-Method header does not match the request's method"}
	}
	if field := named(method); field != "" {
		var p map[string]json.RawMessage
		_ = json.Unmarshal(params, &p)
		var body string
		if raw, ok := p[field]; !ok || json.Unmarshal(raw, &body) != nil {
			return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf("%s takes params.%s, a string", method, field)}
		}
		names := r.Header.Values("Mcp-Name")
		if len(names) != 1 || decoded(names[0]) != body {
			return &Error{Code: CodeHeaderMismatch, Message: fmt.Sprintf("the Mcp-Name header does not match params.%s", field)}
		}
	}
	var capabilities map[string]json.RawMessage
	if raw, ok := m.Meta[metaCapabilities]; !ok || json.Unmarshal(raw, &capabilities) != nil || capabilities == nil {
		return &Error{Code: CodeInvalidParams, Message: "a request declares the client's capabilities in _meta, an empty object for none"}
	}
	return nil
}

// named is the field of params the Mcp-Name header mirrors, for the methods it is required on.
func named(method string) string {
	switch method {
	case "tools/call", "prompts/get":
		return "name"
	case "resources/read":
		return "uri"
	}
	return ""
}

// decoded is a header value as the client meant it: one written in the revision's Base64 sentinel,
// =?base64?...?=, decoded, and any other as it stands. A sentinel that does not decode is kept as
// it stands too, so that it matches no body and the request is refused as a mismatch.
func decoded(v string) string {
	inner, ok := strings.CutPrefix(v, "=?base64?")
	if !ok {
		return v
	}
	inner, ok = strings.CutSuffix(inner, "?=")
	if !ok {
		return v
	}
	b, err := base64.StdEncoding.DecodeString(inner)
	if err != nil {
		return v
	}
	return string(b)
}

// answered is a method's answer: its result, or the error it refuses with.
func (s *Server) answered(ctx context.Context, method string, params json.RawMessage, surface Surface) (map[string]any, *Error) {
	switch method {
	case "server/discover":
		capabilities := map[string]any{"tools": map[string]any{}}
		if s.Resources {
			capabilities["resources"] = map[string]any{}
		}
		return cacheable(map[string]any{
			"supportedVersions": []string{Revision},
			"capabilities":      capabilities,
			"instructions":      s.Instructions,
		}), nil

	case "tools/list":
		if err := noCursor(params); err != nil {
			return nil, err
		}
		tools := surface.Tools
		if tools == nil {
			tools = []Tool{}
		}
		return cacheable(map[string]any{"tools": tools}), nil

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(params, &p) != nil {
			return nil, InvalidParams("tools/call takes a name and an object of arguments")
		}
		if len(p.Arguments) > 0 && string(p.Arguments) != "null" && p.Arguments[0] != '{' {
			return nil, InvalidParams("the arguments of a call are an object")
		}
		i := slices.IndexFunc(surface.Tools, func(t Tool) bool { return t.Name == p.Name })
		if i < 0 {
			// The same answer for a tool that does not exist and one the caller is not offered:
			// "an unusable tool is absent rather than refused", from a call as from the list.
			return nil, InvalidParams("Unknown tool: %s", p.Name)
		}
		out, err := surface.Tools[i].Call(ctx, p.Arguments)
		if err != nil {
			var refused *Error
			if errors.As(err, &refused) {
				return nil, refused
			}
			return nil, &Error{Code: CodeInternalError, Message: "the call could not be answered"}
		}
		if out.Content == nil {
			out.Content = []Content{}
		}
		result := map[string]any{"content": out.Content}
		if out.StructuredContent != nil {
			result["structuredContent"] = out.StructuredContent
		}
		if out.IsError {
			result["isError"] = true
		}
		return result, nil

	case "resources/list", "resources/templates/list", "resources/read":
		if !s.Resources {
			return nil, &Error{Code: CodeMethodNotFound, Message: "this server offers no resources"}
		}
		switch method {
		case "resources/list":
			if err := noCursor(params); err != nil {
				return nil, err
			}
			resources := surface.Resources
			if resources == nil {
				resources = []Resource{}
			}
			return cacheable(map[string]any{"resources": resources}), nil
		case "resources/templates/list":
			if err := noCursor(params); err != nil {
				return nil, err
			}
			templates := surface.Templates
			if templates == nil {
				templates = []Template{}
			}
			return cacheable(map[string]any{"resourceTemplates": templates}), nil
		}
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(params, &p)
		if surface.Read == nil {
			return nil, InvalidParams("Resource not found: %s", p.URI)
		}
		contents, err := surface.Read(ctx, p.URI)
		if err != nil {
			var refused *Error
			if errors.As(err, &refused) {
				return nil, refused
			}
			return nil, &Error{Code: CodeInternalError, Message: "the resource could not be read"}
		}
		return cacheable(map[string]any{"contents": contents}), nil
	}
	return nil, &Error{Code: CodeMethodNotFound, Message: "Method not found: " + method}
}

// cacheable adds the caching hints the revision requires on a discovery, a list and a read.
//
// Every answer here went to one authenticated caller and may differ for the next, since a tool
// list is "filtered by effective permissions on every request", so it is private, never held by
// an intermediary for somebody else; and stale at once, since a grant revoked "revokes one grant,
// from the next request", and a client holding a list for a minute would go on offering a model
// what the caller no longer holds.
func cacheable(result map[string]any) map[string]any {
	result["ttlMs"] = 0
	result["cacheScope"] = "private"
	return result
}

// noCursor refuses a cursor on a list, since no list is paginated and a client can only have made
// one up.
func noCursor(params json.RawMessage) *Error {
	var p struct {
		Cursor *string `json:"cursor"`
	}
	if json.Unmarshal(params, &p) == nil && p.Cursor != nil {
		return InvalidParams("no list here is paginated, so no cursor was issued")
	}
	return nil
}

// validID says whether a request's id is one JSON-RPC allows: a string or a number, never null.
func validID(id json.RawMessage) bool {
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// answer writes one JSON-RPC response, or an error with no id where the request's could not be
// read, as the transport allows.
func answer(w http.ResponseWriter, status int, id json.RawMessage, result map[string]any, refusal *Error) {
	out := map[string]any{"jsonrpc": "2.0"}
	if id != nil {
		out["id"] = id
	}
	if refusal != nil {
		out["error"] = refusal
	} else {
		out["result"] = result
	}
	b, err := json.Marshal(out)
	if err != nil {
		b = []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"the answer could not be encoded"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}
