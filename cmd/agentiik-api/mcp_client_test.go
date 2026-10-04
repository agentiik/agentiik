package main

import (
	"regexp"
	"strings"
	"testing"
)

// "Test the sequence with a fresh client: workflow.language, brick.list and brick.get, a draft,
// workflow.validate until it passes, workflow.commit." A client that knows nothing but the address
// discovers the server, reads the orientation, drafts from the minimal workflow it gives, is told
// what is wrong with a draft and which topic covers it, reads that topic, corrects the draft until
// it validates, commits it, and reads back the version it made. brick.list and brick.get wait on
// the catalog's route, which v0.8.0 serves.
func TestAFreshClientWritesAWorkflowFromTheLanguageAlone(t *testing.T) {
	x := someTenants(t)
	as := x.as["alice"]
	text := func(res map[string]any) string {
		t.Helper()
		content, _ := res["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("a result carries no content: %v", res)
		}
		return content[0].(map[string]any)["text"].(string)
	}
	call := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		res, err := x.mcpAsked(as, "tools/call", map[string]any{"name": name, "arguments": arguments})
		if err != nil {
			t.Fatalf("%s answered %v", name, err)
		}
		return res
	}

	discovered, err := x.mcpAsked(as, "server/discover", nil)
	if err != nil {
		t.Fatal(err)
	}
	instructions, _ := discovered["instructions"].(string)
	for _, want := range []string{"agentiik.yaml", "workflow.language", "workflow.validate"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("the instructions do not name %s: %q", want, instructions)
		}
	}

	// The orientation carries a complete minimal workflow, which the client drafts from.
	orientation := text(call("workflow.language", map[string]any{}))
	minimal := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindStringSubmatch(orientation)
	if minimal == nil {
		t.Fatalf("the orientation carries no workflow to draft from:\n%s", orientation)
	}
	draft := minimal[1]
	draft = regexp.MustCompile(`(?m)^  name: .*$`).ReplaceAllString(draft, "  name: greeting")
	draft = regexp.MustCompile(`(?m)^  namespace: .*$`).ReplaceAllString(draft, "  namespace: finance")

	if res := call("workflow.create", map[string]any{"namespace": "finance", "name": "greeting"}); res["isError"] == true {
		t.Fatalf("workflow.create answered %s", text(res))
	}
	where := func(files map[string]any) map[string]any {
		return map[string]any{"namespace": "finance", "workflow": "greeting", "files": files}
	}

	// A first draft with a key the language does not have: refused, with where, what was expected
	// and the topic to read, which the client reads.
	wrong := strings.Replace(draft, "    script:", "    scirpt:", 1)
	res := call("workflow.validate", where(map[string]any{"agentiik.yaml": wrong}))
	if res["isError"] != true {
		t.Fatalf("a draft with scirpt: was answered %v", res)
	}
	said := text(res)
	topic := regexp.MustCompile(`read workflow\.language with topic (\w+)`).FindStringSubmatch(said)
	if topic == nil || !strings.Contains(said, "expected") {
		t.Fatalf("the refusal names no place, expectation or topic: %q", said)
	}
	if page := text(call("workflow.language", map[string]any{"topic": topic[1]})); !strings.Contains(page, "script") {
		t.Errorf("the topic %s the refusal named says nothing of script:\n%s", topic[1], page)
	}

	// Corrected until it validates, as a client does: each refusal read, the draft changed. The
	// minimal workflow is the language's own, so the first correction is the last.
	files := map[string]any{"agentiik.yaml": draft}
	for attempt := 0; ; attempt++ {
		res = call("workflow.validate", where(files))
		if res["isError"] != true {
			break
		}
		if attempt == 2 {
			t.Fatalf("the orientation's own workflow does not validate:\n%s", text(res))
		}
	}
	if valid, _ := res["structuredContent"].(map[string]any)["valid"].(bool); !valid {
		t.Fatalf("validate answered %v", res)
	}

	res = call("workflow.commit", map[string]any{"namespace": "finance", "workflow": "greeting", "message": "A first workflow", "files": files})
	if res["isError"] == true {
		t.Fatalf("workflow.commit answered %s", text(res))
	}
	commit, _ := res["structuredContent"].(map[string]any)["commit"].(string)
	got := call("workflow.get", map[string]any{"namespace": "finance", "workflow": "greeting"})
	if head := got["structuredContent"].(map[string]any)["repository"].(map[string]any)["head"]; commit == "" || head != commit {
		t.Errorf("the workflow's head is %v, and the commit was %q", head, commit)
	}
}
