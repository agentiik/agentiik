package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/agentiik/agentiik/internal/ulid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A task's progress, from the runner holding it to the controller: running once its container has
// started, publishing once it has exited. It travels on the runner's results subject, beside the
// results, and is never taken for one.

// aProgress is what the runner holding task says once its container is running.
//
// Its task_id is minted per call, for the reason aResult's is.
func aProgress(key agk.TaskID) TaskProgress {
	return TaskProgress{TaskID: ulid.New(), IdempotencyKey: string(key), Runner: "runner-dmz-02", Progress: agk.TaskRunning}
}

// examplesOf reads the examples one member of the vendored wire document gives of itself.
func examplesOf(t *testing.T, member string) [][]byte {
	t.Helper()
	doc, err := fixtures.Wire()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Examples []json.RawMessage `json:"examples"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(doc, &schema); err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, e := range schema.Defs[member].Examples {
		out = append(out, e)
	}
	if len(out) == 0 {
		t.Fatalf("the wire gives no example of %s", member)
	}
	return out
}

// What a runner says of a task in flight is what the document describes, in both states, and the
// wire's own examples are read as what they say.
func TestProgressIsWhatTheWireDescribes(t *testing.T) {
	s := wire(t, "taskProgress")
	for _, state := range []agk.TaskState{agk.TaskRunning, agk.TaskPublishing} {
		p := aProgress(aTask("invoice").ID)
		p.Progress = state
		body, err := p.encode()
		if err != nil {
			t.Errorf("%s could not be written: %s", state, err)
			continue
		}
		if err := validates(t, s, body); err != nil {
			t.Errorf("%s is refused by the wire:\n%s\n\n%s", state, err, body)
		}
		back, err := readProgress(body)
		if err != nil {
			t.Errorf("%s was written and could not be read back: %s", state, err)
		}
		if back != p {
			t.Errorf("%+v was read back as %+v", p, back)
		}
	}
	for _, body := range examplesOf(t, "taskProgress") {
		p, err := readProgress(body)
		if err != nil {
			t.Errorf("the wire's example %s was refused: %s", body, err)
			continue
		}
		again, err := p.encode()
		if err != nil || !sameDocument(t, body, again) {
			t.Errorf("the wire's example %s was written back out as %s (%v)", body, again, err)
		}
	}
}

// Two kinds of message share a runner's results subject, and neither is ever read as the other,
// by the schema or by the reader: every result in the corpus and every example of a result is no
// progress message, every example of a progress message is no result, and a document carrying both
// keywords is neither.
func TestAResultAndProgressAreNeverTakenForEachOther(t *testing.T) {
	results, progress := taskResults(t), wire(t, "taskProgress")

	cases, err := fixtures.TaskResults()
	if err != nil {
		t.Fatal(err)
	}
	var asResults [][]byte
	for _, c := range cases {
		body, err := fs.ReadFile(fixtures.FS, c.File)
		if err != nil {
			t.Fatal(err)
		}
		asResults = append(asResults, body)
	}
	asResults = append(asResults, examplesOf(t, "taskResult")...)
	for _, body := range asResults {
		if isProgress(body) {
			t.Errorf("a result is taken for progress: %s", body)
		}
		if err := validates(t, progress, body); err == nil {
			t.Errorf("the wire accepts a result as progress: %s", body)
		}
		if _, err := readProgress(body); err == nil {
			t.Errorf("a result is read as progress: %s", body)
		}
	}
	for _, body := range examplesOf(t, "taskProgress") {
		if !isProgress(body) {
			t.Errorf("progress is taken for a result: %s", body)
		}
		if err := validates(t, results, body); err == nil {
			t.Errorf("the wire accepts progress as a result: %s", body)
		}
		if _, err := readResult(body); err == nil {
			t.Errorf("progress is read as a result: %s", body)
		}
	}

	both, err := aProgress(aTask("invoice").ID).encode()
	if err != nil {
		t.Fatal(err)
	}
	both = append(bytes.TrimSuffix(both, []byte("}")), []byte(`,"state":"succeeded"}`)...)
	if !isProgress(both) {
		t.Errorf("a document carrying progress is taken for a result: %s", both)
	}
	if _, err := readProgress(both); err == nil {
		t.Errorf("a document carrying both keywords is read as progress: %s", both)
	}
	for name, s := range map[string]*jsonschema.Schema{"a result": results, "progress": progress} {
		if err := validates(t, s, both); err == nil {
			t.Errorf("the wire accepts a document carrying both keywords as %s", name)
		}
	}
}

// Progress the controller would refuse is refused before it is published, and read off the queue
// it is refused there too. Each rule is held by a case only it refuses.
func TestProgressTheControllerWouldRefuseIsNotPublished(t *testing.T) {
	s := wire(t, "taskProgress")
	for name, with := range map[string]func(*TaskProgress){
		"no task_id":                           func(p *TaskProgress) { p.TaskID = "" },
		"a task_id that is not an identifier":  func(p *TaskProgress) { p.TaskID = "01m2aaz9g62nqxfafcxkrpjeh5" },
		"a key that is not one":                func(p *TaskProgress) { p.IdempotencyKey = "invoice/1" },
		"no runner":                            func(p *TaskProgress) { p.Runner = "" },
		"a runner with a dot":                  func(p *TaskProgress) { p.Runner = "runner.dmz" },
		"an ending":                            func(p *TaskProgress) { p.Progress = agk.TaskSucceeded },
		"a state before a runner holds a task": func(p *TaskProgress) { p.Progress = agk.TaskDispatched },
	} {
		p := aProgress(aTask("invoice").ID)
		with(&p)
		if _, err := p.encode(); err == nil {
			t.Errorf("progress with %s would be published", name)
		}
		body, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readProgress(body); err == nil {
			t.Errorf("progress with %s was read", name)
		}
		if err := validates(t, s, body); err == nil {
			t.Errorf("the wire accepts progress with %s", name)
		}
	}
	refused := aProgress(aTask("invoice").ID)
	refused.Progress = agk.TaskFailed
	if err := (&Bus{}).Progress(t.Context(), refused); err == nil {
		t.Error("progress that is an ending was published")
	}

	body, err := aProgress(aTask("invoice").ID).encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string][]byte{
		"a field the wire does not describe": append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"at":"2026-09-10T06:41:09Z"}`)...),
		"a second document after the first":  append(append([]byte{}, body...), body...),
	} {
		if _, err := readProgress(doc); err == nil {
			t.Errorf("progress with %s was read", name)
		}
	}
}

// Progress reaches the controller, from the runner whose subject it came on, before the result it
// preceded and apart from it, once however often the runner publishes it.
func TestProgressComesBackToTheControllerBeforeTheResult(t *testing.T) {
	b := open(t)
	task := aTask(step(t))
	running := aProgress(task.ID)
	publishing := running
	publishing.Progress = agk.TaskPublishing
	result := aResult(task)
	result.TaskID = running.TaskID

	for _, p := range []TaskProgress{running, running, publishing} {
		if err := b.Progress(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Report(t.Context(), result); err != nil {
		t.Fatal(err)
	}

	got := reporting(t, b, func(heard) error { return nil })
	for i, want := range []agk.TaskState{agk.TaskRunning, agk.TaskPublishing, agk.TaskSucceeded} {
		select {
		case h := <-got:
			if h.sender != "runner-dmz-02" {
				t.Errorf("message %d came back from %s", i+1, h.sender)
			}
			switch {
			case want == agk.TaskSucceeded && h.progress != nil:
				t.Errorf("message %d was handed on as progress %+v, and it was the result", i+1, *h.progress)
			case want == agk.TaskSucceeded && (h.result.TaskID != result.TaskID || h.result.State != want):
				t.Errorf("message %d was handed on as %+v", i+1, h.result)
			case want != agk.TaskSucceeded && h.progress == nil:
				t.Errorf("message %d was handed on as the result %+v, and it was progress", i+1, h.result)
			case want != agk.TaskSucceeded && (*h.progress != TaskProgress{TaskID: running.TaskID, IdempotencyKey: running.IdempotencyKey, Runner: running.Runner, Progress: want}):
				t.Errorf("message %d was handed on as %+v, want %s", i+1, *h.progress, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("message %d never reached the controller", i+1)
		}
	}
	select {
	case h := <-got:
		t.Errorf("a fourth message reached the controller: %+v %+v", h.result, h.progress)
	case <-time.After(500 * time.Millisecond):
	}
}

// Progress no delivery would change is taken off the queue and said out loud, as a result is: one
// the controller refuses with Drop, and one nobody can read.
func TestProgressNoDeliveryWouldChangeIsTakenOffAndReported(t *testing.T) {
	b := open(t)
	trouble := make(chan error, 8)
	b.Trouble = func(_ string, err error) { trouble <- err }

	p := aProgress(aTask(step(t)).ID)
	if err := b.Progress(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	unreadable, err := p.encode()
	if err != nil {
		t.Fatal(err)
	}
	unreadable = append(bytes.TrimSuffix(unreadable, []byte("}")), []byte(`,"state":"succeeded"}`)...)
	if _, err := b.js.Publish(t.Context(), ResultSubject(p.Runner), unreadable); err != nil {
		t.Fatal(err)
	}

	seen := reporting(t, b, func(h heard) error {
		if h.progress == nil {
			// A result is not what this is about, and the refusal is for progress.
			return nil
		}
		return Drop(fmt.Errorf("%w: %s is bound to runner-lan-01", errTest, h.progress.IdempotencyKey))
	})
	select {
	case h := <-seen:
		if h.progress == nil || *h.progress != p {
			t.Fatalf("the controller was handed %+v %+v", h.result, h.progress)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the progress never reached the controller")
	}
	said := map[string]bool{}
	for len(said) < 2 {
		select {
		case err := <-trouble:
			switch {
			case errors.Is(err, errTest):
				said["dropped"] = true
			case strings.HasPrefix(err.Error(), "a progress message could not be read"):
				said["unreadable"] = true
			default:
				t.Errorf("what was said reads %q", err)
			}
		case h := <-seen:
			t.Fatalf("it was delivered again rather than taken off the queue: %+v", h.progress)
		case <-time.After(15 * time.Second):
			t.Fatalf("only %v was said", said)
		}
	}
	select {
	case h := <-seen:
		t.Errorf("it came round again: %+v", h.progress)
	case err := <-trouble:
		t.Errorf("something more was said: %q", err)
	case <-time.After(2 * time.Second):
	}
}

// A controller with nowhere to hand progress takes nothing, rather than acknowledging it unread or
// leaving it to come round for ever.
func TestResultsAreNotTakenWithNowhereToHandProgress(t *testing.T) {
	b := open(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := b.Reports(ctx, func(context.Context, string, TaskResult) error { return nil }, nil)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Error("results were taken with nowhere to hand progress")
	}
}
