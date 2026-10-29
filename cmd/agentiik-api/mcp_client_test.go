package main

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// "Test the sequence with a fresh client: workflow.language, a draft, workflow.validate until it
// passes, workflow.commit, the workflow created first with workflow.create, which validating and
// committing ask for." The client is the conformance client, speaking HTTP to the installation serve
// builds, each request on its own connection and held to the revision, and it knows nothing but the
// endpoint's address and a token: the discovery orients it, tools/list tells it the tools and which
// of them only read, and the orientation gives it a complete minimal workflow to draft from. Its
// first draft carries two mistakes, a key misspelt and an output taken from a port the step does not
// publish; it validates before the workflow exists and is told so, creates it, and validates until
// the draft passes, reading at each refusal the topic it names and correcting the line it points at,
// the refusal saying where by a JSON Pointer and a line of the file. It commits the draft, naming no
// parent, the repository being empty, and reads back the version it made. brick.list and brick.get
// arrive with the catalog in v0.8.0, so the client drafts from workflow.language alone.
func TestAFreshClientWritesAWorkflowFromTheLanguageAlone(t *testing.T) {
	x := someTenants(t)
	server := httptest.NewServer(x.in.router)
	t.Cleanup(server.Close)
	client := &conformant{t: t, url: server.URL + "/mcp", token: x.as["alice"].bearer}
	text := func(a answer) string {
		t.Helper()
		content, _ := a.result["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("a result carries no content: %v", a.result)
		}
		return content[0].(map[string]any)["text"].(string)
	}
	failed := func(a answer) bool { return a.result["isError"] == true }
	structured := func(a answer) map[string]any {
		o, _ := a.result["structuredContent"].(map[string]any)
		return o
	}

	discovered := client.send("server/discover", nil)
	instructions, _ := discovered.result["instructions"].(string)
	for _, want := range []string{"agentiik.yaml", "workflow.language", "workflow.validate"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("the instructions do not name %s: %q", want, instructions)
		}
	}
	tools := client.listed()
	for name, reads := range map[string]bool{"workflow.language": true, "workflow.validate": true, "workflow.get": true, "workflow.create": false, "workflow.commit": false} {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("a fresh client is not offered %s: %v", name, tools)
		}
		if hints, _ := tool["annotations"].(map[string]any); (hints["readOnlyHint"] == true) != reads {
			t.Errorf("%s is marked %v, and it reads only: %v", name, hints, reads)
		}
	}

	// The orientation carries a complete minimal workflow, which the client drafts from.
	orientation := text(client.called(tools["workflow.language"], map[string]any{}))
	minimal := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindStringSubmatch(orientation)
	if minimal == nil {
		t.Fatalf("the orientation carries no workflow to draft from:\n%s", orientation)
	}
	intended := regexp.MustCompile(`(?m)^  name: .*$`).ReplaceAllString(minimal[1], "  name: greeting")
	intended = regexp.MustCompile(`(?m)^  namespace: .*$`).ReplaceAllString(intended, "  namespace: finance")
	// The mistakes the first draft carries: where a refusal points at each, the line written wrong,
	// the line meant, and a word the topic the refusal names says of it.
	mistakes := []struct{ at, wrong, right, word string }{
		{"/steps/greet/scirpt", "    scirpt:", "    script:", "script"},
		{"/outputs/greeting/from/port", "      port: result", "      port: out", "port"},
	}
	draft := intended
	for _, m := range mistakes {
		if !strings.Contains(draft, m.right) {
			t.Fatalf("the orientation's workflow carries no %q to get wrong:\n%s", m.right, draft)
		}
		draft = strings.Replace(draft, m.right, m.wrong, 1)
	}
	where := func(files map[string]any) map[string]any {
		return map[string]any{"namespace": "finance", "workflow": "greeting", "files": files}
	}

	// Validating and committing ask for a workflow that exists: validated first, it is not found.
	if early := client.called(tools["workflow.validate"], where(map[string]any{"agentiik.yaml": draft})); !failed(early) || !strings.Contains(text(early), "no such repository") {
		t.Errorf("a draft of a workflow that does not exist was answered %v", early.result)
	}
	if made := client.called(tools["workflow.create"], map[string]any{"namespace": "finance", "name": "greeting"}); failed(made) {
		t.Fatalf("workflow.create answered %s", text(made))
	}

	// Validated until it passes, each refusal read and the draft corrected where it points.
	var refusals []string
	for {
		validated := client.called(tools["workflow.validate"], where(map[string]any{"agentiik.yaml": draft}))
		if !failed(validated) {
			if valid, _ := structured(validated)["valid"].(bool); !valid {
				t.Fatalf("validate answered %v", validated.result)
			}
			break
		}
		if len(refusals) == len(mistakes) {
			t.Fatalf("the draft is refused once every mistake it carried is corrected:\n%s\n%s", text(validated), draft)
		}
		problem := structured(validated)
		pointer, _ := problem["pointer"].(string)
		topic, _ := problem["topic"].(string)
		line, _ := problem["line"].(float64)
		said := text(validated)
		if !strings.Contains(said, pointer) || !strings.Contains(said, "expected") || !strings.Contains(said, "topic "+topic) {
			t.Errorf("the refusal tells a model no place, expectation or topic: %q", said)
		}
		i := -1
		for j, m := range mistakes {
			if m.at == pointer {
				i = j
			}
		}
		if i < 0 {
			t.Fatalf("the refusal points at %s, where the draft carries no mistake: %s", pointer, said)
		}
		m := mistakes[i]
		if written := strings.Split(draft, "\n"); int(line) < 1 || int(line) > len(written) || written[int(line)-1] != m.wrong {
			t.Errorf("the refusal at %s names line %v, and the mistake is on another", pointer, line)
		}
		if page := text(client.called(tools["workflow.language"], map[string]any{"topic": topic})); !strings.Contains(page, m.word) {
			t.Errorf("the topic %s the refusal at %s named says nothing of %s", topic, pointer, m.word)
		}
		refusals = append(refusals, pointer)
		draft = strings.Replace(draft, m.wrong, m.right, 1)
	}
	if len(refusals) != len(mistakes) || draft != intended {
		t.Errorf("the draft passed after the refusals %v, and it carried %d mistakes", refusals, len(mistakes))
	}

	committed := client.called(tools["workflow.commit"], map[string]any{"namespace": "finance", "workflow": "greeting", "message": "A first workflow", "files": map[string]any{"agentiik.yaml": draft}})
	if failed(committed) {
		t.Fatalf("workflow.commit answered %s", text(committed))
	}
	commit, _ := structured(committed)["commit"].(string)
	read := structured(client.called(tools["workflow.get"], map[string]any{"namespace": "finance", "workflow": "greeting"}))
	head := read["repository"].(map[string]any)["head"]
	files, _ := read["files"].(map[string]any)
	if commit == "" || head != commit || files["agentiik.yaml"] != draft {
		t.Errorf("the workflow's head is %v holding %v, and the commit was %q of %q", head, files, commit, draft)
	}
}
