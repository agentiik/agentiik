package control

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/internal/fixtures"
	"github.com/agentiik/agentiik/runner"
)

// What a runner reads back is what the controller wrote.
//
// messageOf writes a graph.Task as the wire's task message and runner.TaskOf reads one back, with
// the envelopes the runner fetched for its input ports. The two are one translation in two
// directions, written in two packages that may not import each other's side, so nothing but a test
// holding both keeps a field from being written by one and dropped by the other. Here, over tasks
// drawn at random and over every task message of the corpus, the task comes back unchanged on
// every field the wire carries: everything but a call, which is never dispatched, and whether a
// result may be cached, which the controller decides from the cache key.

// alphabet is what a run identifier is minted in.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func pick[T any](r *rand.Rand, from ...T) T { return from[r.IntN(len(from))] }

func draw(r *rand.Rand, n int, of string) string {
	var b strings.Builder
	for range n {
		b.WriteByte(of[r.IntN(len(of))])
	}
	return b.String()
}

// someOf is none, or a few of from.
func someOf(r *rand.Rand, from ...string) []string {
	n := r.IntN(len(from) + 1)
	if n == 0 {
		return nil
	}
	out := append([]string(nil), from...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out[:n]
}

// aValue is a parameter value as JSON reads one back, since a message is read off the bus as JSON.
func aValue(r *rand.Rand, depth int) any {
	switch n := r.IntN(6); {
	case n == 0:
		return draw(r, 1+r.IntN(12), "abcdefghij-/:. ")
	case n == 1:
		return r.IntN(2) == 0
	case n == 2:
		return float64(r.IntN(100000)) / 100
	case n == 3 && depth < 2:
		return []any{aValue(r, depth+1), aValue(r, depth+1)}
	case n == 4 && depth < 2:
		return map[string]any{"k" + draw(r, 3, "abc"): aValue(r, depth+1)}
	}
	return nil
}

// drawnTask is a task the evaluator could have decided, drawn at random, with the envelopes on its
// input ports and the dispatch that publishes it.
func drawnTask(r *rand.Rand) (graph.Task, controller.Dispatch) {
	run := agk.RunID(draw(r, 26, alphabet))
	step := agk.Step(pick(r, "invoice", "normalize", "build", "fetch_2", "send-mail"))
	attempt := 1 + r.IntN(4)
	var shard agk.Shard
	if r.IntN(2) == 0 {
		shard.Of = 1 + r.IntN(16)
		shard.Index = 1 + r.IntN(shard.Of)
	}
	t := graph.Task{
		ID:        agk.NewTaskID(run, step, attempt, shard),
		Run:       run,
		Workflow:  pick(r, "monthly-invoicing", "etl", "a"),
		Namespace: pick(r, "finance", "ops"),
		Commit:    draw(r, 7+r.IntN(34), "0123456789abcdef"),
		Step:      step,
		Attempt:   attempt,
		Shard:     shard,
		Image:     "ghcr.io/acme/agk-" + string(step) + "@sha256:" + draw(r, 64, "0123456789abcdef"),
		Resources: graph.Resources{CPU: pick(r, "0.5", "1", "2"), Memory: pick(r, "256Mi", "1Gi"), PIDs: 1 + r.IntN(512)},
		Network:   pick(r, graph.NetworkNone, graph.NetworkInternal, graph.NetworkEgress),
		RunsOn:    someOf(r, "arch=amd64", "zone=dmz", "gpu=true"),
		Deadline:  time.Unix(1789000000+r.Int64N(1e7), r.Int64N(1e9)).In(time.FixedZone("CEST", 2*3600)),

		Idempotent: r.IntN(2) == 0,
	}
	if r.IntN(2) == 0 {
		t.Script = someOf(r, "set -eu", "make build", "cp dist/report.pdf /agk/out/ports/ok/")
		t.BeforeScript = someOf(r, "apk add --no-cache make")
		t.AfterScript = someOf(r, "cat /tmp/build.log || true")
		t.Shell = someOf(r, "/bin/sh", "-eu", "-c")
	}
	if n := r.IntN(4); n > 0 {
		t.Params = map[string]any{}
		for i := range n {
			t.Params[fmt.Sprintf("p%d", i)] = aValue(r, 0)
		}
	}
	for _, name := range someOf(r, "billing", "client-key", "smtp") {
		t.Secrets = append(t.Secrets, graph.SecretMount{Name: name, Mount: "/agk/secrets/" + name})
	}
	for _, port := range someOf(r, "ok", "rejected", "out", "error") {
		t.Outputs = append(t.Outputs, agk.Port(port))
	}
	// Each form a workflow may write: the short form, which only narrows the tree and names
	// neither where the file goes nor its mode; the long form relocating it, with or without a
	// mode; and the long form setting a mode where the file already is. Only a relocation names
	// where it goes, and the wire carries no to for the others.
	for _, from := range someOf(r, "config/rates.json", "scripts/", "templates/*.tmpl", "./sql/**") {
		f := graph.FileSelector{From: from}
		switch r.IntN(3) {
		case 1:
			f.To, f.Mode = "/agk/files/"+filepath.Base(from), pick(r, "", "0444", "0555")
		case 2:
			f.Mode = pick(r, "0444", "0600")
		}
		t.Files = append(t.Files, f)
	}
	if t.Network == graph.NetworkEgress {
		t.EgressAllow = someOf(r, "api.billing.example.com:443", "smtp.example.com:587")
	}
	if r.IntN(2) == 0 {
		t.Timeout = graph.Duration(time.Duration(1+r.IntN(7200)) * pick(r, time.Millisecond, time.Second, time.Minute))
	}
	if t.Idempotent && r.IntN(2) == 0 {
		t.CacheKey = t.Namespace + "/sha256:" + draw(r, 8, "0123456789abcdef")
	}

	d := controller.Dispatch{Task: t, Row: draw(r, 26, alphabet), Grant: "agkgrant_" + draw(r, 26, alphabet) + "_Zm9vYmFyYmF6cXV4MTIzNA"}
	for _, port := range someOf(r, "in", "orders", "refunds") {
		e := agk.Empty(run, "upstream", agk.Port(port), 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
		for i := range r.IntN(4) {
			e.Items = append(e.Items, agk.NewItem(map[string]any{"n": float64(i)}))
		}
		e.Meta.Count = len(e.Items)
		if t.Inputs == nil {
			t.Inputs, d.Inputs = map[agk.Port]agk.Envelope{}, map[agk.Port]controller.InputRef{}
		}
		t.Inputs[agk.Port(port)] = e
		d.Inputs[agk.Port(port)] = controller.InputRef{Digest: draw(r, 64, "0123456789abcdef"), Items: len(e.Items)}
	}
	d.Task = t
	return t, d
}

// overTheWire is a message as a runner takes it: written as JSON, read back as JSON.
func overTheWire(t *testing.T, m bus.TaskMessage) (bus.TaskMessage, []byte) {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back bus.TaskMessage
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	return back, body
}

func TestATaskReadBackByARunnerIsTheTaskTheControllerWrote(t *testing.T) {
	s := taskMessages(t)
	r := rand.New(rand.NewPCG(20260924, 216))
	for i := range 500 {
		want, d := drawnTask(r)
		m, err := messageOf(d)
		if err != nil {
			t.Fatalf("task %d: %s", i, err)
		}
		m, body := overTheWire(t, m)
		if err := validates(t, s, body); err != nil {
			t.Fatalf("task %d is written as a message the wire refuses: %s\n%s", i, err, body)
		}
		got, err := runner.TaskOf(m, want.Inputs)
		if err != nil {
			t.Fatalf("task %d does not read back: %s\n%s", i, err, body)
		}
		// The deadline is an instant, and the wire writes it in UTC. The dispatch is the row
		// the message went out as, which the controller holds beside the task and a runner
		// reads into it.
		want.Deadline = want.Deadline.UTC()
		want.Dispatch = d.Row
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("task %d reads back changed:\n got %+v\nwant %+v\n%s", i, got, want, body)
		}
	}
}

// Every task message of the corpus, read by a runner and written again by the controller, is the
// message it was.
func TestEveryTaskMessageOfTheCorpusIsWrittenAgainAsItWas(t *testing.T) {
	cases, err := fixtures.TaskMessages()
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, c := range cases {
		if !c.Valid {
			continue
		}
		read++
		t.Run(filepath.Base(c.File), func(t *testing.T) {
			body, err := fs.ReadFile(fixtures.FS, c.File)
			if err != nil {
				t.Fatal(err)
			}
			var m bus.TaskMessage
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			d := controller.Dispatch{Row: m.TaskID, Grant: m.Grant, Inputs: map[agk.Port]controller.InputRef{}}
			envelopes := map[agk.Port]agk.Envelope{}
			for _, in := range m.Inputs {
				e := agk.Empty(agk.RunID(m.RunID), "upstream", agk.Port(in.Port), 1, time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC))
				for range in.Items {
					e.Items = append(e.Items, agk.NewItem(nil))
				}
				e.Meta.Count = len(e.Items)
				envelopes[agk.Port(in.Port)] = e
				d.Inputs[agk.Port(in.Port)] = controller.InputRef{Digest: strings.TrimPrefix(in.Digest, "sha256:"), Items: in.Items}
			}
			if d.Task, err = runner.TaskOf(m, envelopes); err != nil {
				t.Fatalf("the message does not read back: %s", err)
			}
			again, err := messageOf(d)
			if err != nil {
				t.Fatal(err)
			}
			_, written := overTheWire(t, again)
			var was, is any
			json.Unmarshal(body, &was)
			json.Unmarshal(written, &is)
			if !reflect.DeepEqual(was, is) {
				t.Errorf("the message is written again as\n%s\nand was\n%s", written, body)
			}
		})
	}
	if read < 5 {
		t.Fatalf("read %d valid task messages, and the corpus holds five", read)
	}
}
