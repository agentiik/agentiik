package bus

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// What goes on the bus, held to the document that describes it.
//
// This is the test that did not exist, and its absence is the whole reason the first version of
// this package put graph.Task on the queue: a document the wire refuses in sixteen places, one of
// which was the input envelopes' items, which the page forbids in as many words. The workflow and
// envelope corpora were already checked here; the wire was not.

// taskMessages compiles the wire's task message schema out of the vendored document.
func taskMessages(t *testing.T) *jsonschema.Schema {
	t.Helper()
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	// Unmarshalled through the library's own reader, which keeps numbers as the draft
	// expects them, and pinned to 2020-12 rather than left to be guessed from a $schema the
	// compiler would then go and fetch.
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("wire.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("wire.schema.json#/$defs/taskMessage")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// validates reports what the schema says about one message.
func validates(t *testing.T, s *jsonschema.Schema, body []byte) error {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return s.Validate(v)
}

// The corpus first, so that a failure below is this package's and not the compiler's.
func TestTheVendoredCorpusIsWhatItSaysItIs(t *testing.T) {
	s := taskMessages(t)
	cases, err := fixtures.TaskMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 6 {
		t.Fatalf("the vendored task message corpus holds %d documents", len(cases))
	}
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatalf("%s: %s", c.File, err)
		}
		err = validates(t, s, body)
		switch {
		case c.Valid && err != nil:
			t.Errorf("%s should be accepted: %s", c.File, err)
		case !c.Valid && err == nil:
			t.Errorf("%s should be refused: %s", c.File, c.Rule)
		}
	}
}

// aDispatch is what the controller hands the bus for an ordinary brick step.
func aDispatch(t *testing.T) controller.Dispatch {
	t.Helper()
	task := graph.Task{
		ID:          agk.NewTaskID(aRun, "invoice", 2, agk.Shard{Index: 3, Of: 8}),
		Run:         aRun,
		Namespace:   "finance",
		Workflow:    "monthly-invoicing",
		Commit:      "a3f9c1e",
		Step:        "invoice",
		Attempt:     2,
		Shard:       agk.Shard{Index: 3, Of: 8},
		Image:       "ghcr.io/acme/agk-invoice@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc",
		Params:      map[string]any{"endpoint": "https://api.billing.example.com/v2"},
		Secrets:     []graph.SecretMount{{Name: "billing", Mount: "/agk/secrets/billing"}},
		Outputs:     []agk.Port{"out", "error"},
		Resources:   graph.Resources{CPU: "0.5", Memory: "256Mi", PIDs: 128},
		Network:     graph.NetworkEgress,
		EgressAllow: []string{"api.billing.example.com:443"},
		RunsOn:      []string{"zone=dmz", "arch=amd64"},
		Deadline:    time.Date(2026, 9, 10, 6, 12, 0, 0, time.UTC),
	}
	return controller.Dispatch{
		Task: task,
		Row:  "01M2AAZ9G62NQXFAFCXKRPJEH5",
		Grant: "agkgrant_01M2AAZ9G62NQXFAFCXKRPJEH5_" +
			"dGFza2dyYW50ZXhhbXBsZTAxMjM0NTY3ODlhYmNkZWZnaGk",
		Inputs: map[agk.Port]controller.InputRef{
			"in": {Digest: "7c2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c9f11", Items: 1},
		},
	}
}

// What this package publishes is what the document describes. The claim is not that the fields
// look right: it is that the schema, vendored from the repository that owns it, accepts the bytes.
func TestWhatIsPublishedIsWhatTheWireDescribes(t *testing.T) {
	s := taskMessages(t)

	m, err := messageOf(aDispatch(t))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := validates(t, s, body); err != nil {
		t.Fatalf("the message this package publishes is refused by the wire:\n%s\n\n%s", err, body)
	}

	// And the one rule the whole design turns on, checked by reading the bytes rather than by
	// trusting the type: no item of any input envelope is in there.
	var seen map[string]any
	if err := json.Unmarshal(body, &seen); err != nil {
		t.Fatal(err)
	}
	inputs, _ := seen["inputs"].([]any)
	if len(inputs) != 1 {
		t.Fatalf("the message carries %d inputs", len(inputs))
	}
	first, _ := inputs[0].(map[string]any)
	for _, forbidden := range []string{"items_data", "data", "files", "url", "meta"} {
		if _, there := first[forbidden]; there {
			t.Errorf("an input carries %q, and it is named by port, digest and count", forbidden)
		}
	}
}

// A script step, which the wire could not describe until it was widened, and which is the half of
// the engine the message used to drop on the floor.
func TestAScriptStepSurvivesTheWire(t *testing.T) {
	s := taskMessages(t)
	d := aDispatch(t)
	d.Task.Image = "docker.io/library/alpine@sha256:1ab74e66e7966eea770c1042664af5f550650f299ce00e02132ffa4fec5039cc"
	d.Task.Script = []string{"set -eu", "make build"}
	d.Task.BeforeScript = []string{"apk add --no-cache make"}
	d.Task.AfterScript = []string{"cat /tmp/build.log || true"}
	d.Task.Shell = []string{"/bin/sh", "-eu", "-c"}
	d.Task.Files = []graph.FileSelector{{From: "config/rates.json", To: "/agk/files/rates.json", Mode: "0444"}}
	d.Task.Timeout = graph.Duration(10 * time.Minute)
	d.Task.Idempotent = true
	d.Task.CacheKey = "finance/sha256:1ab74e66/9f2c1d07"

	m, err := messageOf(d)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := validates(t, s, body); err != nil {
		t.Fatalf("a script step is refused by the wire:\n%s\n\n%s", err, body)
	}

	var seen map[string]any
	if err := json.Unmarshal(body, &seen); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"script", "before_script", "after_script", "shell", "files", "timeout", "cache_key"} {
		if _, there := seen[want]; !there {
			t.Errorf("a script step travelled without %s, which is what the runner needs to run it", want)
		}
	}
}

// A message with nothing to say still says it, because a required key dropped for being empty is
// a message a closed document refuses.
func TestAnEmptyListTravelsAsAnEmptyList(t *testing.T) {
	s := taskMessages(t)
	d := aDispatch(t)
	d.Task.Params = nil
	d.Task.Secrets = nil
	d.Task.Outputs = nil
	d.Task.RunsOn = nil
	d.Task.EgressAllow = nil
	d.Task.Network = graph.NetworkNone
	d.Task.Shard = agk.Shard{}
	d.Inputs = nil

	m, err := messageOf(d)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := validates(t, s, body); err != nil {
		t.Fatalf("a message with nothing in it is refused by the wire:\n%s\n\n%s", err, body)
	}

	var seen map[string]any
	if err := json.Unmarshal(body, &seen); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"params", "secrets", "inputs", "outputs", "runs_on", "network"} {
		if _, there := seen[want]; !there {
			t.Errorf("%s was dropped for being empty, and the document requires it", want)
		}
	}
	if seen["network"] != "none" {
		t.Errorf("network travelled as %v, and the wire spells it none, internal or egress", seen["network"])
	}
}

// A dispatch missing what only the controller can supply is refused here rather than on the queue.
func TestAMessageWithoutItsGrantIsNotPublished(t *testing.T) {
	for _, c := range []struct {
		name string
		with func(*controller.Dispatch)
	}{
		{"no grant", func(d *controller.Dispatch) { d.Grant = "" }},
		{"no row", func(d *controller.Dispatch) { d.Row = "" }},
		{"no deadline", func(d *controller.Dispatch) { d.Task.Deadline = time.Time{} }},
	} {
		d := aDispatch(t)
		c.with(&d)
		if _, err := messageOf(d); err == nil {
			t.Errorf("a dispatch with %s was turned into a message", c.name)
		}
	}
}
