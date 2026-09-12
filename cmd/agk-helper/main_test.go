package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
)

// The three verbs are file reading and file writing, so every one of them is tested
// against a temporary directory laid out as /agk, with no container, no daemon and no
// network anywhere in reach. That is the same property the program itself has to have: a
// tool that needed a daemon to be tested would be a tool that needed one to run.

// The run this fixture is of. A ULID, because that is what AGK_RUN_ID carries.
const (
	fixtureRun  = "01JMZ8W4K2R7Q0E3N5T9ZQ4XKB"
	fixtureStep = "check-vat"
)

// harness is one temporary /agk, the environment a container would have been given, and
// the two streams the program writes on.
type harness struct {
	t    *testing.T
	env  env
	out  bytes.Buffer
	err  bytes.Buffer
	vars map[string]string
}

// newHarness lays the two halves of the contract down: /agk/in, and /agk/out with the
// two directories the runner creates before the container starts.
func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t: t,
		vars: map[string]string{
			EnvRunID:    fixtureRun,
			EnvStep:     fixtureStep,
			EnvAttempt:  "1",
			EnvOutPorts: "out,error",
		},
	}
	h.env = env{
		Root:   root,
		In:     strings.NewReader(""),
		Out:    &h.out,
		Err:    &h.err,
		Getenv: func(name string) string { return h.vars[name] },
		// Held still, because produced_at is the one member of the metadata the
		// runner restamps and a test that compared a clock would be testing the
		// clock.
		Now: func() time.Time { return time.Date(2026, 9, 10, 6, 0, 12, 418000000, time.UTC) },
	}
	for _, dir := range []string{h.env.inDir(), h.env.outPortsDir(), h.env.outFilesDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// mount writes one input envelope where the runner would have bound it.
func (h *harness) mount(port agk.Port, envelope agk.Envelope) {
	h.t.Helper()
	dir := filepath.Join(h.env.inDir(), string(port))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := envelope.Encode(&b); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, EnvelopeFile), b.Bytes(), 0o444); err != nil {
		h.t.Fatal(err)
	}
}

// stdin is what the container's standard input carries for the next call.
func (h *harness) stdin(s string) { h.env.In = strings.NewReader(s) }

// write puts a file in the container's /tmp, which is what a script's intermediate
// results are.
func (h *harness) write(name, content string) string {
	h.t.Helper()
	path := filepath.Join(h.t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return path
}

// run drives argv and returns the exit code, which is what a script reads.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	return run(h.env, args)
}

// ok runs and fails the test when the verb refused.
func (h *harness) ok(args ...string) {
	h.t.Helper()
	if code := h.run(args...); code != 0 {
		h.t.Fatalf("agk %s exited %d: %s", strings.Join(args, " "), code, h.err.String())
	}
}

// refused runs, requires exit 1, and returns what was written on standard error.
func (h *harness) refused(args ...string) string {
	h.t.Helper()
	code := h.run(args...)
	if code != 1 {
		h.t.Fatalf("agk %s exited %d, and a refusal exits 1: %s%s", strings.Join(args, " "), code, h.out.String(), h.err.String())
	}
	if h.out.Len() > 0 {
		h.t.Errorf("agk %s wrote %q on standard output while refusing, and a refusal never lands in a pipe", strings.Join(args, " "), h.out.String())
	}
	return h.err.String()
}

// port reads back the envelope the program wrote, through the same door the runner
// collects it through.
func (h *harness) port(port agk.Port) agk.Envelope {
	h.t.Helper()
	envelope, err := readEnvelope(portPath(h.env, port))
	if err != nil {
		h.t.Fatalf("reading back port %s: %s", port, err)
	}
	return envelope
}

// says fails unless the refusal names every one of these.
func says(t *testing.T, message string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not name %q: %s", want, message)
		}
	}
}

// batch is a fixture envelope of n items, each carrying the rank it was written at.
func batch(port agk.Port, n int) agk.Envelope {
	e := agk.Envelope{
		Meta: agk.Meta{
			RunID:      fixtureRun,
			Step:       "normalize",
			Port:       port,
			Attempt:    1,
			Count:      n,
			ProducedAt: time.Date(2026, 9, 10, 5, 0, 0, 0, time.UTC),
		},
		Items: make([]agk.Item, n),
	}
	for i := range e.Items {
		e.Items[i] = agk.Item{
			ID:    "item-" + strconv.Itoa(i+1),
			Data:  map[string]any{"vat_number": "FR" + strconv.Itoa(40000+i)},
			Files: []agk.File{},
		}
	}
	return e
}

// lines splits what was written on standard output into its lines, which is what a pipe
// reads.
func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// object reads one line of JSON back, the way jq on the other side of the pipe would.
func object(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("the line %q is not one JSON object: %s", line, err)
	}
	return m
}

// TestTheUsageAndTheDispatchCannotDisagree holds the table to being the whole of what the
// program offers: a verb that is in the usage text and not in the dispatch would be a
// promise nothing keeps, and the documentation names exactly three.
func TestTheUsageAndTheDispatchCannotDisagree(t *testing.T) {
	if len(commands) != 3 {
		t.Fatalf("the table holds %d verbs, and the documentation names three: agk items, agk emit and agk attach", len(commands))
	}
	h := newHarness(t)
	h.ok("help")
	text := h.out.String()
	for _, c := range commands {
		if c.run == nil {
			t.Errorf("the verb %s is in the table with nothing to run", c.name)
		}
		if !strings.Contains(text, c.usage) {
			t.Errorf("the usage text does not carry %q", c.usage)
		}
		if !strings.HasPrefix(c.usage, "agk "+c.name) {
			t.Errorf("the verb %s is spelled %q in its own usage line", c.name, c.usage)
		}
	}
	// The three verbs are spelled the way the page spells them, and a fourth would be
	// a contract nobody agreed to mounted inside every container.
	for i, name := range []string{"items", "emit", "attach"} {
		if commands[i].name != name {
			t.Errorf("verb %d is %s, and the three are items, emit and attach", i, commands[i].name)
		}
	}
}

// TestAVerbNobodyOffersIsRefusedByName keeps the dispatch from falling through to
// something.
func TestAVerbNobodyOffersIsRefusedByName(t *testing.T) {
	h := newHarness(t)
	message := h.refused("publish")
	says(t, message, "publish", "verbs")
	says(t, message, "agk items", "agk emit", "agk attach")
}

// TestNoArgumentsIsTheUsageOnStandardError holds the one rule about what goes where:
// standard output carries the answer and standard error carries everything said on the way
// to it, so that agk items | jq is a pipe carrying items and nothing else.
func TestNoArgumentsIsTheUsageOnStandardError(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != 1 {
		t.Errorf("agk with no verb exited %d", code)
	}
	if h.out.Len() > 0 {
		t.Errorf("agk with no verb wrote %q on standard output", h.out.String())
	}
	says(t, h.err.String(), "agk items", BinPath)
}

// TestAVerbAskedForItsUsageAnswersOnStandardOutput keeps -h out of the refusal path: a
// person asking what a verb takes has not failed at anything, so it answers where an answer
// goes and exits 0.
func TestAVerbAskedForItsUsageAnswersOnStandardOutput(t *testing.T) {
	for _, c := range commands {
		for _, flag := range []string{"-h", "--help"} {
			h := newHarness(t)
			if code := h.run(c.name, flag); code != 0 {
				t.Errorf("agk %s %s exited %d: %s", c.name, flag, code, h.err.String())
			}
			if !strings.Contains(h.out.String(), c.usage) {
				t.Errorf("agk %s %s wrote %q", c.name, flag, h.out.String())
			}
		}
	}
}

// TestAFlagNobodyOffersIsRefusedRatherThanIgnored keeps a misspelling from being dropped,
// which for a flag that selects half a payload would be a batch published whole.
func TestAFlagNobodyOffersIsRefusedRatherThanIgnored(t *testing.T) {
	h := newHarness(t)
	says(t, h.refused("emit", "out", "--fliter", ".valid"), "fliter")
	says(t, h.refused("attach", "f", "--port", "out", "--mediatype", "text/csv"), "mediatype")
}
