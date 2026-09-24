package control

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// What the controller puts on the bus, held to the document that describes it, and what it takes
// back.
//
// Package bus holds a result to the same document from the runner's side, as it is written and as
// it is read. What is held here is the translation, which is this package's: a dispatch written as
// a task message the wire accepts, and a result the wire describes taken as the answer it says.

// taskMessages compiles the wire's task message schema out of the vendored document.
func taskMessages(t *testing.T) *jsonschema.Schema { return wire(t, "taskMessage") }

// taskResults compiles the wire's result schema, which is what a runner sends back.
func taskResults(t *testing.T) *jsonschema.Schema { return wire(t, "taskResult") }

// wire compiles one message of the vendored wire document.
func wire(t *testing.T, message string) *jsonschema.Schema {
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
	s, err := c.Compile("wire.schema.json#/$defs/" + message)
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

// What a runner sends back, taken as the answer it says.
//
// The first version of package bus put controller.Answer on the queue as Go spells it: whole
// envelopes, no task_id, no key. The vendored corpus did not decode into it, so a runner written
// against the schema would have had every result taken off the queue as unreadable. Package bus
// holds its reader to the corpus now, and this holds what the controller is handed to the document
// it was read from.

// Every valid result in the corpus is taken as what it says, whole and with its outputs as digests.
func TestAResultIsAnsweredAsTheWireDescribesIt(t *testing.T) {
	cases, err := fixtures.TaskResults()
	if err != nil {
		t.Fatal(err)
	}
	answered := 0
	for _, c := range cases {
		if !c.Valid {
			continue
		}
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatalf("%s: %s", c.File, err)
		}
		var r bus.TaskResult
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("%s: %s", c.File, err)
		}
		a, err := answerOf(r)
		if err != nil {
			t.Errorf("%s, which covers %s, was not answered: %s", c.File, c.Covers, err)
			continue
		}
		saysWhatItSays(t, c.File, body, a)
		answered++
	}
	if answered < 3 {
		t.Fatalf("the vendored result corpus holds %d valid documents", answered)
	}
}

// saysWhatItSays holds an answer to the document it was read from, field by field, as the fixture
// spells each one.
func saysWhatItSays(t *testing.T, file string, body []byte, a controller.Answer) {
	t.Helper()
	var doc struct {
		TaskID         string `json:"task_id"`
		IdempotencyKey string `json:"idempotency_key"`
		Runner         string `json:"runner"`
		State          string `json:"state"`
		ExitCode       *int   `json:"exit_code"`
		StartedAt      string `json:"started_at"`
		FinishedAt     string `json:"finished_at"`
		Outputs        []struct {
			Port, Digest string
			Items        int
		} `json:"outputs"`
		Log *struct {
			URI       string `json:"uri"`
			Lines     int    `json:"lines"`
			Truncated bool   `json:"truncated"`
		} `json:"log"`
		Usage map[string]float64 `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	instant := func(s string) time.Time {
		if s == "" {
			return time.Time{}
		}
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}

	if a.Row != doc.TaskID || string(a.Result.Task) != doc.IdempotencyKey || a.Runner != doc.Runner || a.Result.State.String() != doc.State {
		t.Errorf("%s names dispatch %s of %s, by %s, %s, and was read as dispatch %s of %s, by %s, %s",
			file, doc.TaskID, doc.IdempotencyKey, doc.Runner, doc.State, a.Row, a.Result.Task, a.Runner, a.Result.State)
	}
	if exit := doc.ExitCode; (exit == nil && a.Result.ExitCode != 0) || (exit != nil && *exit != a.Result.ExitCode) {
		t.Errorf("%s exits %v and was read as exiting %d", file, exit, a.Result.ExitCode)
	}
	// An absent code is carried as 0, which is success, and the controller records no code
	// for a success or a failure whose container never started. So the 0 is harmless only on
	// an answer that says as much.
	if ended := a.Result.State == agk.TaskSucceeded || a.Result.State == agk.TaskFailed; doc.ExitCode == nil && ended && !a.Result.StartedAt.IsZero() {
		t.Errorf("%s reports no exit code and was read as a container that started and exited 0", file)
	}
	if !a.Result.StartedAt.Equal(instant(doc.StartedAt)) || !a.Result.FinishedAt.Equal(instant(doc.FinishedAt)) {
		t.Errorf("%s runs from %q to %q and was read as %s to %s", file, doc.StartedAt, doc.FinishedAt, a.Result.StartedAt, a.Result.FinishedAt)
	}
	if len(a.Result.Outputs) != 0 {
		t.Errorf("%s was handed on carrying envelopes", file)
	}
	if len(a.Outputs) != len(doc.Outputs) {
		t.Errorf("%s names %d ports and was read as naming %d", file, len(doc.Outputs), len(a.Outputs))
	} else {
		for i, o := range doc.Outputs {
			want := controller.Output{Port: agk.Port(o.Port), Digest: strings.TrimPrefix(o.Digest, "sha256:"), Items: o.Items}
			if a.Outputs[i] != want {
				t.Errorf("%s names %+v and was read as %+v", file, o, a.Outputs[i])
			}
		}
	}
	switch {
	case doc.Log == nil && (a.Log != agk.LogURI{} || a.LogLines != 0 || a.LogCut):
		t.Errorf("%s has no log and was read as having one at %s", file, a.Log)
	case doc.Log != nil && (a.Log.String() != doc.Log.URI || a.LogLines != doc.Log.Lines || a.LogCut != doc.Log.Truncated):
		t.Errorf("%s logs %+v and was read as %s, %d lines, cut %v", file, *doc.Log, a.Log, a.LogLines, a.LogCut)
	}
	if len(a.Usage) != len(doc.Usage) {
		t.Errorf("%s measures %v and was read as %v", file, doc.Usage, a.Usage)
	}
	for name, want := range doc.Usage {
		var got float64
		switch v := a.Usage[name].(type) {
		case float64:
			got = v
		case int64:
			got = float64(v)
		default:
			t.Errorf("%s measures %s as %v and was read as %v", file, name, want, a.Usage[name])
			continue
		}
		if got != want {
			t.Errorf("%s measures %s as %v and was read as %v", file, name, want, got)
		}
	}
}

// And the whole of it on a real bus: every result in the corpus published as a runner would, the
// ones the wire accepts handed to the controller as the answer they say, and the others taken off
// the queue and reported without the controller ever seeing them.
func TestAResultIsWhatTheWireDescribes(t *testing.T) {
	cases, err := fixtures.TaskResults()
	if err != nil {
		t.Fatal(err)
	}
	handsOn(t, cases)
}

// A fixture the corpus still files as invalid once its schema has come to accept it, which package
// bus names in outgrown, is read by the runner's half like any other document the schema accepts,
// and is handed to the controller. Counted as refused here, it would fail this package's tests for
// a reason they do not name while package bus's pass.
func TestAResultTheCorpusHasOutgrownIsHandedOn(t *testing.T) {
	cases, err := fixtures.TaskResults()
	if err != nil {
		t.Fatal(err)
	}
	filed := false
	for i := range cases {
		if cases[i].Valid {
			// The corpus as it stands when the schema moves and the fixture does not.
			cases[i].Valid, cases[i].Covers, cases[i].Rule = false, "", "a rule the schema has since dropped"
			filed = true
			break
		}
	}
	if !filed {
		t.Fatal("the vendored result corpus holds no valid document to file as invalid")
	}
	handsOn(t, cases)
}

// handsOn publishes every result of cases as a runner would, and holds what the controller is
// handed to the documents the wire accepts.
//
// Which those are is the schema's to say rather than the corpus's label. The two part when the
// schema moves and a fixture does not, and package bus is where that is held: its outgrown names
// each such fixture, and its tests fail the day the schema refuses one again. Reading the label
// here would take a second copy of that list, kept in step by hand, and a fixture on one and not
// the other would fail these tests on a result that was handed on as it should be.
func handsOn(t *testing.T, cases []fixtures.Case) {
	t.Helper()
	results := taskResults(t)
	b, url := served(t)
	trouble := make(chan error, 16)
	b.Trouble = func(_ string, err error) { trouble <- err }
	js := publishing(t, url)

	type sent struct {
		file string
		body []byte
	}
	heard := map[string]sent{}
	refused := 0
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		var named struct {
			TaskID string `json:"task_id"`
			Runner string `json:"runner"`
			State  string `json:"state"`
		}
		if err := json.Unmarshal(body, &named); err != nil {
			t.Fatal(err)
		}
		if validates(t, results, body) == nil {
			heard[named.TaskID+" "+named.State] = sent{c.File, body}
		} else {
			refused++
		}
		if _, err := js.Publish(t.Context(), bus.ResultSubject(named.Runner), body); err != nil {
			t.Fatal(err)
		}
	}

	got := answering(t, New(b), func(controller.Answer) error { return nil })
	deadline := time.After(15 * time.Second)
	for len(heard) > 0 || refused > 0 {
		select {
		case a := <-got:
			s, ok := heard[a.Row+" "+a.Result.State.String()]
			if !ok {
				t.Fatalf("the controller was handed %+v, which is not a result the wire accepts or was handed twice", a)
			}
			delete(heard, a.Row+" "+a.Result.State.String())
			saysWhatItSays(t, s.file, s.body, a)
		case err := <-trouble:
			if refused == 0 {
				t.Fatalf("one more result was taken off the queue than the wire refuses: %s", err)
			}
			refused--
		case <-deadline:
			t.Fatalf("%d results the wire accepts were never handed on and %d it refuses never reported", len(heard), refused)
		}
	}
}
