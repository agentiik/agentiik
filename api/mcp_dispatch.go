package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/mcp"
)

// A tool of the user's server is a route of the API. "Its tools mirror the API and hold no authority
// of their own: each call runs as the presenting principal, under the grants that govern the
// console." So a tool does not reach a store or ask the authorizer itself: it makes the request the
// route serves, through the router, with the credential the MCP request presented, and answers what
// the route answered. Whatever the route checks, a scope, a grant, a deny, a protected branch, the
// hook, the tool checks the same way, since it is the same code; and a refusal comes back in the
// route's own words.

// throughOf says which tool of the MCP server made a request, and false for one it did not make:
// "marked as arriving through MCP so git log and the audit log agree". A request the server makes
// carries the tool as package audit keeps it, so that every entry its act records names the tool.
func throughOf(ctx context.Context) (string, bool) {
	return audit.ThroughOf(ctx)
}

// toolAnswer is what a route answered a request the MCP server made.
type toolAnswer struct {
	status int
	header http.Header
	body   bytes.Buffer
}

func (a *toolAnswer) Header() http.Header { return a.header }
func (a *toolAnswer) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.body.Write(b)
}
func (a *toolAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

// dispatch makes one request of the API as the caller of the MCP request r, for the tool named, and
// answers what the route answered. The body is sent as JSON where there is one. Only the caller's
// credential is carried, a bearer token, which is the one credential the MCP endpoint takes.
func (m *MCP) dispatch(r *http.Request, tool, method, path string, query url.Values, body any) (*toolAnswer, error) {
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			return nil, err
		}
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	ctx := audit.Through(r.Context(), tool)
	req, err := http.NewRequestWithContext(ctx, method, target, &payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = r.RemoteAddr
	req.Host = r.Host
	a := &toolAnswer{header: http.Header{}}
	m.rt.ServeHTTP(a, req)
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a, nil
}

// segment is a name as one segment of a path, escaped.
func segment(name string) string { return url.PathEscape(name) }

// result is a route's answer as a tool's result: a success as its JSON, both as text and as
// structured content, a refusal the caller can act on as a result with isError, its sentence first,
// and a failure of the installation as an error, which the client is told only that the call could
// not be answered.
func result(a *toolAnswer) (*mcp.CallResult, error) {
	if a.status >= 500 {
		return nil, fmt.Errorf("api: the route answered %d: %.300s", a.status, a.body.String())
	}
	var structured any
	if a.body.Len() > 0 && strings.HasPrefix(a.header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(a.body.Bytes(), &structured); err != nil {
			return nil, err
		}
	}
	if a.status >= 400 {
		// A quota reached is said where a model reads it, as a collection's call says it: "with
		// retry_after_seconds in structuredContent: what a webhook answers as 429 with
		// Retry-After, said where a model reads it".
		if a.status == http.StatusTooManyRequests {
			if seconds, err := strconv.Atoi(a.header.Get("Retry-After")); err == nil {
				o, _ := structured.(map[string]any)
				if o == nil {
					o = map[string]any{}
				}
				o["retry_after_seconds"] = seconds
				structured = o
			}
		}
		return &mcp.CallResult{Content: []mcp.Content{mcp.Text(refusedText(a.status, structured))}, StructuredContent: structured, IsError: true}, nil
	}
	text := strings.TrimSpace(a.body.String())
	if text == "" {
		text = http.StatusText(a.status)
	}
	return &mcp.CallResult{Content: []mcp.Content{mcp.Text(text)}, StructuredContent: structured}, nil
}

// refusedText is a refusal as a model reads it: the API's sentence, and where it carries a problem
// of the hook's, where it is, what was expected there and the topic of workflow.language to read
// before correcting the draft.
func refusedText(status int, structured any) string {
	o, _ := structured.(map[string]any)
	said, _ := o["error"].(string)
	if said == "" {
		said = http.StatusText(status)
	}
	lines := []string{said}
	if detail, _ := o["detail"].(string); detail != "" {
		lines = append(lines, detail)
	}
	if rule, _ := o["rule"].(string); rule != "" {
		node, _ := o["pointer"].(string)
		if node == "" {
			node = "the document"
		}
		if file, _ := o["file"].(string); file != "" {
			node = file + " " + node
		}
		expected, _ := o["expected"].(string)
		topic, _ := o["topic"].(string)
		lines = append(lines, "at "+node+", expected "+expected, "read workflow.language with topic "+topic+" before correcting the draft")
	}
	return strings.Join(lines, "\n")
}
