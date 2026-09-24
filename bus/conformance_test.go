package bus

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// What goes on the bus, held to the document that describes it.
//
// This is the test that did not exist, and its absence is the whole reason the first version of
// this package put graph.Task on the queue: a document the wire refuses in sixteen places, one of
// which was the input envelopes' items, which the page forbids in as many words. The workflow and
// envelope corpora were already checked here; the wire was not.
//
// A task message is written by package bus/control, and its tests hold what it writes to the
// document. What is held here is what a runner writes, a result, and how one is read back.

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

// What a runner sends back, held to the same document.
//
// The result was the half nobody checked, and the first version of this package put
// controller.Answer on the queue as Go spells it: whole envelopes, no task_id, no key. The vendored
// corpus did not decode into it, so a runner written against the schema would have had every
// result taken off the queue as unreadable. The corpus is what the reader is held to now.

// outgrown names the fixtures the corpus calls invalid and its own schema accepts, and says why.
//
// A fixture lands here when the schema moves and the fixture does not. It is held to the schema
// rather than to its own label, and the test fails the day the schema refuses it again, which is
// the day the entry has to go. It is empty: the one it held, a cancelled result filed as a run's
// state, was rewritten around a state only a run has when the schemas caught up.
var outgrown = map[string]string{}

// Every result in the corpus is read the way the corpus says it is: a valid one is read as what it
// says, and an invalid one is refused. A valid one written back out as it was read is the document
// it was read from, so reading and writing are one shape. How the controller takes what was read,
// with its outputs as digests, is package bus/control's to say and its tests to hold.
func TestAResultIsReadAsTheWireDescribesIt(t *testing.T) {
	s := taskResults(t)
	cases, err := fixtures.TaskResults()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 5 {
		t.Fatalf("the vendored result corpus holds %d documents", len(cases))
	}
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatalf("%s: %s", c.File, err)
		}
		bySchema := validates(t, s, body)
		r, byReader := readResult(body)

		if why, stale := outgrown[c.File]; stale {
			if bySchema != nil {
				t.Errorf("%s is refused by its schema again, so the corpus caught up and the exception for it (%s) can go: %s", c.File, why, bySchema)
			}
			continue
		}
		if !c.Valid {
			if bySchema == nil {
				t.Errorf("%s should be refused by the schema: %s", c.File, c.Rule)
			}
			if byReader == nil {
				t.Errorf("%s was read as %+v, and it is refused because %s", c.File, r, c.Rule)
			}
			continue
		}
		if bySchema != nil {
			t.Errorf("%s should be accepted by the schema: %s", c.File, bySchema)
		}
		if byReader != nil {
			t.Errorf("%s, which covers %s, was refused: %s", c.File, c.Covers, byReader)
			continue
		}
		again, err := r.encode()
		if err != nil {
			t.Errorf("%s could not be written back out: %s", c.File, err)
			continue
		}
		if !sameDocument(t, body, again) {
			t.Errorf("%s was written back out as another document:\n%s\n\n%s", c.File, body, again)
		}
	}
}

// sameDocument says whether two encodings are one document, whatever the order of their keys.
func sameDocument(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

// What a runner reports is what the document describes, in each of the shapes a result takes: a
// task that ran, one that never reached a container, one stopped at its deadline, and one a runner
// recovers as lost. A success that published nothing still says so.
func TestWhatIsReportedIsWhatTheWireDescribes(t *testing.T) {
	s := taskResults(t)
	task := aTask("invoice")
	ran := aResult(task)

	nothing := aResult(task)
	nothing.Outputs = []Output{}

	unreached := aResult(task)
	unreached.State = agk.TaskFailed
	unreached.ExitCode, unreached.StartedAt, unreached.FinishedAt = nil, time.Time{}, time.Time{}
	unreached.Outputs, unreached.Usage = nil, nil
	unreached.Log.Lines = 3

	stopped := aResult(task)
	stopped.State = agk.TaskTimedOut
	stopped.ExitCode = nil

	lost := TaskResult{
		TaskID: ran.TaskID, IdempotencyKey: ran.IdempotencyKey, Runner: ran.Runner,
		State: agk.TaskLost, StartedAt: ran.StartedAt,
	}

	for name, r := range map[string]TaskResult{
		"a task that ran": ran, "a success that published nothing": nothing,
		"a task that never reached a container": unreached, "a task stopped at its deadline": stopped,
		"a task recovered as lost": lost,
	} {
		body, err := r.encode()
		if err != nil {
			t.Errorf("%s could not be written: %s", name, err)
			continue
		}
		if err := validates(t, s, body); err != nil {
			t.Errorf("%s is refused by the wire:\n%s\n\n%s", name, err, body)
		}
		if _, err := readResult(body); err != nil {
			t.Errorf("%s was written and could not be read back: %s", name, err)
		}
	}

	body, err := nothing.encode()
	if err != nil {
		t.Fatal(err)
	}
	var seen map[string]any
	if err := json.Unmarshal(body, &seen); err != nil {
		t.Fatal(err)
	}
	if outputs, there := seen["outputs"].([]any); !there || len(outputs) != 0 {
		t.Errorf("a success that published nothing travelled with outputs %v, and it says so with an empty list", seen["outputs"])
	}
}

// A result the controller would refuse is refused before it is published, where the error reaches
// the runner that wrote it, rather than on the queue, where all that is left is to take it off. And
// read off the queue, it is refused there too. Each rule is held by a case only it refuses.
func TestAResultTheControllerWouldRefuseIsNotReported(t *testing.T) {
	task := aTask("invoice")
	artifact := strings.Repeat("c1f4", 16)
	for _, c := range []struct {
		name string
		with func(*TaskResult)
	}{
		{"no task_id", func(r *TaskResult) { r.TaskID = "" }},
		{"a task_id that is not an identifier", func(r *TaskResult) { r.TaskID = "01m2aaz9g62nqxfafcxkrpjeh5" }},
		{"a key that is not one", func(r *TaskResult) { r.IdempotencyKey = "invoice/1" }},
		{"no runner", func(r *TaskResult) { r.Runner = "" }},
		{"a state it passes through", func(r *TaskResult) { r.State = agk.TaskRunning }},
		{"a success with another exit code", func(r *TaskResult) { exit := 1; r.ExitCode = &exit }},
		{"a success that names no ports", func(r *TaskResult) { r.Outputs = nil }},
		{"an exit code with no container started", func(r *TaskResult) { r.StartedAt = time.Time{} }},
		{"an exit with no instant", func(r *TaskResult) { r.FinishedAt = time.Time{} }},
		{"a deadline's exit code with no instant", func(r *TaskResult) { r.State, r.FinishedAt = agk.TaskTimedOut, time.Time{} }},
		{"a failure from a container that started and never exited", func(r *TaskResult) {
			r.State, r.ExitCode, r.FinishedAt = agk.TaskFailed, nil, time.Time{}
		}},
		{"an exit code no container exits with", func(r *TaskResult) { exit := 256; r.State, r.ExitCode = agk.TaskFailed, &exit }},
		{"a loss that describes an outcome", func(r *TaskResult) { r.State = agk.TaskLost }},
		{"a loss that describes nothing but its log", func(r *TaskResult) {
			*r = TaskResult{TaskID: r.TaskID, IdempotencyKey: r.IdempotencyKey, Runner: r.Runner, State: agk.TaskLost, StartedAt: r.StartedAt, Log: r.Log}
		}},
		{"a port twice", func(r *TaskResult) { r.Outputs = append(r.Outputs, r.Outputs[0]) }},
		{"a port that is not a name", func(r *TaskResult) { r.Outputs[0].Port = "ok,error" }},
		{"a digest without its algorithm", func(r *TaskResult) { r.Outputs[0].Digest = strings.TrimPrefix(r.Outputs[0].Digest, "sha256:") }},
		{"a digest that would leave its prefix", func(r *TaskResult) { r.Outputs[0].Digest = "sha256:../../other/sha256/x" }},
		{"a negative count", func(r *TaskResult) { r.Outputs[0].Items = -1 }},
		{"an artifact that is not a digest", func(r *TaskResult) { r.Artifacts = []Artifact{{SHA256: "C1F4", Bytes: 1}} }},
		{"an artifact of negative size", func(r *TaskResult) { r.Artifacts = []Artifact{{SHA256: artifact, Bytes: -1}} }},
		{"a log that is not a log URI", func(r *TaskResult) { r.Log.URI = "https://logs.example.com/x" }},
		{"a log of negative length", func(r *TaskResult) { r.Log.Lines = -1 }},
		{"a log under another run", func(r *TaskResult) {
			other, _ := agk.NewLogURI(agk.NewTaskID(agk.NewRunID(), "invoice", 1, agk.Shard{}))
			r.Log.URI = other.String()
		}},
		{"a negative usage", func(r *TaskResult) { r.Usage.CPUSeconds = -1 }},
	} {
		r := aResult(task)
		r.Outputs = append([]Output(nil), r.Outputs...)
		log, usage := *r.Log, *r.Usage
		r.Log, r.Usage = &log, &usage
		c.with(&r)
		if _, err := r.encode(); err == nil {
			t.Errorf("a result with %s would be published", c.name)
		}
		if body, err := json.Marshal(r); err == nil {
			if _, err := readResult(body); err == nil {
				t.Errorf("a result with %s was read", c.name)
			}
		}
	}

	// Report is encode and then the bus, and a result encode refuses never reaches the bus,
	// which a Bus with no connection would find out about the hard way.
	refused := aResult(task)
	refused.State = agk.TaskRunning
	if err := (&Bus{}).Report(t.Context(), refused); err == nil {
		t.Error("a result that is not an ending was published")
	}

	// What only a document can say: more than the wire describes, and more than one of it.
	body, err := aResult(task).encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string][]byte{
		"a field the wire does not describe": append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"host":"runner-dmz-02.example.com"}`)...),
		"a second document after the first":  append(append([]byte{}, body...), body...),
	} {
		if _, err := readResult(doc); err == nil {
			t.Errorf("a result with %s was read", name)
		}
	}
}

// And the whole of it on a real bus: every result in the corpus published as a runner would, the
// valid ones handed to the controller as the result they say, and the invalid ones taken off the
// queue and reported without the controller ever seeing them.
func TestAResultIsWhatTheWireDescribes(t *testing.T) {
	b := open(t)
	trouble := make(chan error, 16)
	b.Trouble = func(_ string, err error) { trouble <- err }

	cases, err := fixtures.TaskResults()
	if err != nil {
		t.Fatal(err)
	}
	type sent struct {
		file string
		body []byte
	}
	valid := map[string]sent{}
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
		if _, stale := outgrown[c.File]; c.Valid || stale {
			valid[named.TaskID+" "+named.State] = sent{c.File, body}
		} else {
			refused++
		}
		if _, err := b.js.Publish(t.Context(), ResultSubject(named.Runner), body); err != nil {
			t.Fatal(err)
		}
	}

	got := reporting(t, b, func(heard) error { return nil })
	deadline := time.After(15 * time.Second)
	for len(valid) > 0 || refused > 0 {
		select {
		case h := <-got:
			s, ok := valid[h.result.TaskID+" "+h.result.State.String()]
			if !ok {
				t.Fatalf("the controller was handed %+v, which is not a result the corpus holds as valid or was handed twice", h.result)
			}
			delete(valid, h.result.TaskID+" "+h.result.State.String())
			if back, err := json.Marshal(h.result); err != nil || !sameDocument(t, s.body, back) {
				t.Errorf("%s was handed on as another document:\n%s\n\n%s", s.file, s.body, back)
			}
			if h.sender != h.result.Runner {
				t.Errorf("%s was handed on from %s, and was published on %s's subject", s.file, h.sender, h.result.Runner)
			}
		case err := <-trouble:
			if refused == 0 {
				t.Fatalf("one more result was taken off the queue than the corpus refuses: %s", err)
			}
			refused--
		case <-deadline:
			t.Fatalf("%d valid results were never handed on and %d invalid ones never reported", len(valid), refused)
		}
	}
}
