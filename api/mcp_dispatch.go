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

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/mcp"
	"github.com/agentiik/agentiik/version"
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
	a := &toolAnswer{header: http.Header{}}
	if err := m.serveTo(a, r, tool, method, path, query, body); err != nil {
		return nil, err
	}
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a, nil
}

// serveTo is dispatch answering into w, for a tool that reads what the route writes as it is
// written rather than once it is all held.
func (m *MCP) serveTo(w http.ResponseWriter, r *http.Request, tool, method, path string, query url.Values, body any) error {
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			return err
		}
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	ctx := audit.Through(r.Context(), tool)
	// The surface the request is made for, which the token it carries was issued for where it is an
	// OAuth token: the route is the API's own, made inside the process for that surface, and the
	// token is accepted there for it alone, never forwarded anywhere else.
	if resource := servedResource(r, m.base); resource != "" {
		ctx = forResource(ctx, resource)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, &payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = r.RemoteAddr
	req.Host = r.Host
	m.api.ServeHTTP(w, req)
	return nil
}

// tailAnswer is what the log stream route answers run_logs: its events read as they are written,
// keeping the last lines asked for and nothing else, so that a log of any size is read in the room
// of those lines. It flushes as a connection would, doing nothing, since the stream route sends
// nothing past its headers to a writer that cannot, and run_logs then read every log as empty.
// Anything but a stream, a refusal, is held whole as toolAnswer holds it.
type tailAnswer struct {
	toolAnswer
	most int

	// of, where given, is the one task whose lines are kept, by its key: the stream sends a step's
	// dispatches one after the other, its attempts and its shards, and a failed run's message is
	// the failed task's own. Of a key handed out again after a loss, the last dispatch is the one
	// that ended the task, and keeping says the stream is in one of that key's.
	of      agk.TaskID
	keeping bool

	streaming bool
	partial   []byte
	event     string

	// ring holds the last lines read, most of them at most, the oldest at next once it is full.
	ring []string
	next int
}

// tailLineMost is the longest line of a stream tailAnswer reads, the longest a log line's event
// is once written as JSON; a longer one is skipped rather than held.
const tailLineMost = 4 << 20

func (a *tailAnswer) WriteHeader(status int) {
	a.toolAnswer.WriteHeader(status)
	a.streaming = a.status == http.StatusOK && strings.HasPrefix(a.header.Get("Content-Type"), "text/event-stream")
}

func (a *tailAnswer) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.WriteHeader(http.StatusOK)
	}
	if !a.streaming {
		return a.toolAnswer.Write(b)
	}
	rest := b
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			if len(a.partial)+len(rest) <= tailLineMost {
				a.partial = append(a.partial, rest...)
			} else {
				a.partial = a.partial[:0]
			}
			break
		}
		line := rest[:i]
		if len(a.partial) > 0 {
			line = append(a.partial, line...)
			a.partial = a.partial[:0]
		}
		a.read(string(line))
		rest = rest[i+1:]
	}
	return len(b), nil
}

// Flush is a connection's, which sends what was written: everything is read as it is written here.
func (a *tailAnswer) Flush() {}

// read takes one line of the stream: an event's kind, and a line's or a gap's data under it, and
// where the lines of one task are kept, a dispatch's under it, which begins that task's lines again.
func (a *tailAnswer) read(line string) {
	kept := a.of == "" || a.keeping
	switch {
	case strings.HasPrefix(line, "event: "):
		a.event = strings.TrimPrefix(line, "event: ")
	case strings.HasPrefix(line, "data: ") && a.event == "dispatch" && a.of != "":
		var d struct {
			IdempotencyKey agk.TaskID `json:"idempotency_key"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) == nil {
			if a.keeping = d.IdempotencyKey == a.of; a.keeping {
				a.ring, a.next = a.ring[:0], 0
			}
		}
	case strings.HasPrefix(line, "data: ") && a.event == "line" && kept:
		var l struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &l) == nil {
			a.keep(l.Text)
		}
	case strings.HasPrefix(line, "data: ") && a.event == "gap" && kept:
		a.keep("(lines written and not kept)")
	}
}

// keep holds a line, in place of the oldest once most are held.
func (a *tailAnswer) keep(text string) {
	if len(a.ring) < a.most {
		a.ring = append(a.ring, text)
		return
	}
	a.ring[a.next] = text
	a.next = (a.next + 1) % a.most
}

// tail is the lines held, oldest first.
func (a *tailAnswer) tail() []string {
	return append(append([]string{}, a.ring[a.next:]...), a.ring[:a.next]...)
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
// of the hook's, where it is, what was expected there and the topic of workflow_language to read
// before correcting the draft. A tag no pin holds is no fault of the draft, which a client of MCP
// could not correct by reading the language: it is told to pin the tag with workflow_pin, which is
// what the hook's own sentence, written for a pusher with agk at hand, cannot name.
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
	// A problem of the hook's names its topic; a run's input refused names a rule too, required,
	// schema or undeclared, beside the input, and its sentence says all there is.
	if rule, _ := o["rule"].(string); rule != "" && o["topic"] != nil {
		node, _ := o["pointer"].(string)
		if node == "" {
			node = "the document"
		}
		if file, _ := o["file"].(string); file != "" {
			node = file + " " + node
		}
		expected, _ := o["expected"].(string)
		topic, _ := o["topic"].(string)
		next := "read workflow_language with topic " + topic + " before correcting the draft"
		if rule == string(version.RuleImageNotPinned) {
			next = "the draft need not change: pin the tag with workflow_pin, which asks its registry for the digest it serves now and records it, then call again"
		}
		lines = append(lines, "at "+node+", expected "+expected, next)
	}
	return strings.Join(lines, "\n")
}
