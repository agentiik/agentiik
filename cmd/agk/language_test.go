package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/language"
	"github.com/agentiik/agentiik/mcp"
)

// agk language with no topic prints the orientation, "a minimal workflow and the list of topics",
// on standard output and nothing on standard error, reaching nothing: the Env carries no server
// and no lookup that would name one.
func TestAgkLanguageWithNoTopicPrintsTheOrientation(t *testing.T) {
	e, out, errs := reading(t)
	if code := run(t.Context(), e, []string{"language"}); code != exitSucceeded {
		t.Fatalf("agk language leaves with %d: %s", code, errs)
	}
	if out.String() != language.Orientation() {
		t.Errorf("agk language printed\n%s\nand the orientation is\n%s", out, language.Orientation())
	}
	if errs.String() != "" {
		t.Errorf("agk language wrote to standard error: %s", errs)
	}
	for _, topic := range language.Names() {
		if !strings.Contains(out.String(), "`"+topic+"`") {
			t.Errorf("the orientation agk language printed does not list %s", topic)
		}
	}
}

// agk language TOPIC prints that topic's page as the reference carries it, for every topic there
// is, so that a topic added upstream is printed with nothing added here.
func TestAgkLanguagePrintsATopicsPage(t *testing.T) {
	for _, topic := range language.Names() {
		e, out, errs := reading(t)
		if code := run(t.Context(), e, []string{"language", topic}); code != exitSucceeded {
			t.Errorf("agk language %s leaves with %d: %s", topic, code, errs)
			continue
		}
		page, _ := language.Page(topic)
		if out.String() != page {
			t.Errorf("agk language %s printed\n%s\nand its page is\n%s", topic, out, page)
		}
		if errs.String() != "" {
			t.Errorf("agk language %s wrote to standard error: %s", topic, errs)
		}
	}
}

// A topic that is none is the command line being wrong, exit 2, and the refusal names what was
// typed and every topic there is, on standard error, so that nothing reaches a pipe. Two topics are
// refused the same way, since a page is one topic's.
func TestAgkLanguageRefusesAnUnknownTopicNamingTheTopics(t *testing.T) {
	e, out, errs := reading(t)
	if code := run(t.Context(), e, []string{"language", "loops"}); code != exitUsage {
		t.Errorf("agk language loops leaves with %d, want %d", code, exitUsage)
	}
	if out.String() != "" {
		t.Errorf("a refusal reached standard output: %s", out)
	}
	if !strings.Contains(errs.String(), `"loops"`) {
		t.Errorf("the refusal does not name what was typed: %s", errs)
	}
	for _, topic := range language.Names() {
		if !strings.Contains(errs.String(), topic) {
			t.Errorf("the refusal does not name the topic %s: %s", topic, errs)
		}
	}

	e, out, errs = reading(t)
	if code := run(t.Context(), e, []string{"language", "steps", "ports"}); code != exitUsage {
		t.Errorf("agk language steps ports leaves with %d, want %d", code, exitUsage)
	}
	if out.String() != "" || !strings.Contains(errs.String(), "one topic") {
		t.Errorf("two topics answered %q on standard output and %q on standard error", out, errs)
	}

	// -h says what the verb takes, and is no mistake.
	e, _, errs = reading(t)
	if code := run(t.Context(), e, []string{"language", "-h"}); code != exitSucceeded {
		t.Errorf("agk language -h leaves with %d", code)
	}
	if !strings.Contains(errs.String(), "Usage:") || !strings.Contains(errs.String(), "fan_out") {
		t.Errorf("agk language -h said %q", errs)
	}
}

// agk language prints what workflow_language answers, asked of the API's own MCP server: the same
// orientation with no topic, the same page for a topic, and the same sentence for a topic that is
// none, so that a person and a model are taught one language and refused in one sentence.
func TestAgkLanguagePrintsWhatWorkflowLanguageAnswers(t *testing.T) {
	// The bearer names its principal and nothing checks it, which is what the router looks like
	// from a test of a route rather than of a credential.
	rt, err := api.NewRouter(nobody{}, func(r *http.Request) (api.Identity, error) {
		return api.Identity{Principal: api.Principal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.NewMCP(rt, api.MCPOptions{PublicURL: "https://agentiik.example.com"}); err != nil {
		t.Fatal(err)
	}
	ask := func(arguments map[string]any) (string, *mcp.Error) {
		t.Helper()
		params := map[string]any{
			"name":      "workflow_language",
			"arguments": arguments,
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    mcp.Revision,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(b)))
		r.Header.Set("MCP-Protocol-Version", mcp.Revision)
		r.Header.Set("Mcp-Method", "tools/call")
		r.Header.Set("Mcp-Name", "workflow_language")
		r.Header.Set("Authorization", "Bearer alice")
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		var answer struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error *mcp.Error `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
			t.Fatalf("workflow_language answered %d %s", w.Code, w.Body)
		}
		if answer.Error != nil {
			return "", answer.Error
		}
		if len(answer.Result.Content) != 1 {
			t.Fatalf("workflow_language answered %s", w.Body)
		}
		return answer.Result.Content[0].Text, nil
	}

	for _, topic := range append([]string{""}, language.Names()...) {
		args, arguments := []string{"language"}, map[string]any{}
		if topic != "" {
			args, arguments = append(args, topic), map[string]any{"topic": topic}
		}
		taught, refused := ask(arguments)
		if refused != nil {
			t.Errorf("workflow_language refused %q: %+v", topic, refused)
			continue
		}
		e, out, errs := reading(t)
		if code := run(t.Context(), e, args); code != exitSucceeded {
			t.Errorf("agk %s leaves with %d: %s", strings.Join(args, " "), code, errs)
		}
		if out.String() != taught {
			t.Errorf("agk %s printed\n%s\nand workflow_language answers\n%s", strings.Join(args, " "), out, taught)
		}
	}

	_, refused := ask(map[string]any{"topic": "loops"})
	e, _, errs := reading(t)
	run(t.Context(), e, []string{"language", "loops"})
	if refused == nil || refused.Code != mcp.CodeInvalidParams || strings.TrimSuffix(errs.String(), "\n") != refused.Message {
		t.Errorf("agk language loops said %q and workflow_language answered %+v", errs, refused)
	}
}

// nobody is an authorizer that allows nothing and says nobody owns anything: what the MCP endpoint
// needs to be served, since workflow_language is any authenticated principal's.
type nobody struct{}

func (nobody) Allow(context.Context, api.Principal, api.Permission, api.Target) (bool, error) {
	return false, nil
}

func (nobody) Owned(context.Context, api.Principal) ([]string, error) { return nil, nil }
