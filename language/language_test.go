package language_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/language"
)

// The topics are the sixteen the documentation names, in its order: "repository, inputs, outputs,
// triggers, steps, ports, needs, merge, fan_out, retry, expressions, secrets, files, script,
// includes, mcp". A client is told these names by the orientation and asks for one of them, so a
// topic dropped or renamed upstream is a change a person has to see here.
func TestTheTopicsAreTheDocumentations(t *testing.T) {
	want := []string{"repository", "inputs", "outputs", "triggers", "steps", "ports", "needs", "merge", "fan_out", "retry", "expressions", "secrets", "files", "script", "includes", "mcp"}
	got := language.Names()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the topics are %v, and the documentation names %v", got, want)
	}
}

// Every topic has its page, and the page is the one topics.json names for it, headed by its name;
// a name that is none has no page.
func TestEveryTopicHasItsPage(t *testing.T) {
	for _, topic := range language.Topics() {
		page, ok := language.Page(topic.Name)
		if !ok || !strings.HasPrefix(page, "# `"+topic.Name+"`\n") {
			t.Errorf("%s has no page of its own: %q", topic.Name, head(page))
		}
		for _, section := range []string{"## Keywords", "## Schema", "## Examples"} {
			if !strings.Contains(page, "\n"+section+"\n") {
				t.Errorf("the page of %s has no %s", topic.Name, section)
			}
		}
		if topic.Summary == "" || !strings.Contains(page, topic.Summary) {
			t.Errorf("the summary of %s, %q, is not on its page", topic.Name, topic.Summary)
		}
	}
	for _, none := range []string{"", "Triggers", "trigger", "index", "topics", "../schemas/brick.schema.json"} {
		if _, ok := language.Page(none); ok {
			t.Errorf("%q has a page, and it is no topic", none)
		}
	}
}

// The orientation says what the language is, shows one complete workflow and lists every topic
// and every part, which is everything a client asking with no topic needs to ask the next thing.
func TestTheOrientationNamesEveryTopicAndPart(t *testing.T) {
	o := language.Orientation()
	if !strings.Contains(o, "## A complete minimal workflow\n\n```yaml\napiVersion: agentiik.dev/v1\n") {
		t.Errorf("the orientation shows no complete workflow:\n%s", o)
	}
	for _, topic := range language.Topics() {
		if !strings.Contains(o, "- `"+topic.Name+"`: ") {
			t.Errorf("the orientation does not list %s", topic.Name)
		}
	}
	for _, part := range language.Parts() {
		if !strings.Contains(o, "- `"+part.Name+"`, ") {
			t.Errorf("the orientation does not list the part %s", part.Name)
		}
	}
}

// The parts are the three the documentation names, "the entry point, a brick manifest or an
// envelope", each a JSON Schema 2020-12 document published under the $id the index gives it.
func TestEachPartIsTheDocumentItNames(t *testing.T) {
	var names []string
	for _, part := range language.Parts() {
		names = append(names, part.Name)
		b, ok := language.Schema(part.Name)
		if !ok {
			t.Errorf("%s is listed and not carried", part.Name)
			continue
		}
		var doc struct {
			Schema string `json:"$schema"`
			ID     string `json:"$id"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Errorf("%s is not JSON: %v", part.Name, err)
		}
		if doc.Schema != "https://json-schema.org/draft/2020-12/schema" || doc.ID != part.ID || part.ID != "https://schemas.agentiik.dev/"+part.File {
			t.Errorf("%s is %s under %s, and the index says %s", part.Name, doc.Schema, doc.ID, part.ID)
		}
	}
	if strings.Join(names, ",") != "workflow,brick,envelope" {
		t.Errorf("the parts are %v", names)
	}
	for _, none := range []string{"", "wire", "openapi", "workflow.schema.json", "../reference/index.md"} {
		if _, ok := language.Schema(none); ok {
			t.Errorf("%q is served as a part", none)
		}
	}
}

// A place in agentiik.yaml is explained by the topic whose path pattern matches the most of it, so
// that a validation error naming a pointer can name the page that explains the fix: the step's
// retry is the retry topic's rather than the steps topic's, a hidden block's is the same as a
// step's, and a key nothing knows is the document's.
func TestAPlaceIsExplainedByTheTopicMatchingMostOfIt(t *testing.T) {
	for pointer, want := range map[string]string{
		"":                                      "repository",
		"/apiVersion":                           "repository",
		"/not-a-keyword":                        "repository",
		"/inputs/orders/schema":                 "inputs",
		"/outputs/invoices/retain":              "outputs",
		"/on/schedule/0/cron":                   "triggers",
		"/on/webhook/1/auth":                    "triggers",
		"/steps/normalize/image":                "steps",
		"/steps/normalize/outputs/0":            "ports",
		"/steps/issue/needs/0/port":             "needs",
		"/steps/issue/merge":                    "merge",
		"/steps/issue/strategy/fan_out":         "fan_out",
		"/steps/issue/retry/backoff/max":        "retry",
		"/.base/retry/on/1":                     "retry",
		"/defaults/retry/max":                   "retry",
		"/steps/issue/if":                       "needs",
		"/secrets/0":                            "secrets",
		"/steps/issue/files/0/to":               "files",
		"/steps/issue/script/2":                 "script",
		"/include/0/workflow":                   "includes",
		"/mcp/tools/0/timeout":                  "mcp",
		"/mcp/tools/0/annotations/readOnlyHint": "mcp",
	} {
		if got := language.TopicOf(pointer); got != want {
			t.Errorf("%q is explained by %q, and not %q", pointer, got, want)
		}
	}
}

func head(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
