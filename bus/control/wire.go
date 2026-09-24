package control

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
)

// The wire from the controller's side: what it decided, written as the task message package bus
// describes, and a result read back as the answer it takes. The types are package bus's, where a
// runner reads them, and bus/wire.go says why they are not the domain types.

// messageOf turns what the evaluator decided into what the wire describes.
//
// Everything the schema requires is written, including the empty list and the zero, because a
// required key dropped for being empty is a message a closed document refuses: "a runner reading
// an absent field would be deciding something the controller had already decided".
func messageOf(d controller.Dispatch) (bus.TaskMessage, error) {
	t := d.Task
	if d.Grant == "" {
		return bus.TaskMessage{}, fmt.Errorf("task %s carries no grant, and a message without one asks a runner to do work it cannot fetch the inputs for", t.ID)
	}
	if d.Row == "" {
		return bus.TaskMessage{}, fmt.Errorf("task %s names no row, and task_id is what a grant and a log are addressed by", t.ID)
	}
	if t.Deadline.IsZero() {
		return bus.TaskMessage{}, fmt.Errorf("task %s carries no deadline, and the runner has nothing to stop the container at", t.ID)
	}

	m := bus.TaskMessage{
		TaskID:         d.Row,
		IdempotencyKey: string(t.ID),
		RunID:          string(t.Run),
		Namespace:      t.Namespace,
		Workflow:       t.Workflow,
		Step:           string(t.Step),
		Attempt:        t.Attempt,
		Image:          t.Image,

		Script:       t.Script,
		BeforeScript: t.BeforeScript,
		AfterScript:  t.AfterScript,
		Shell:        t.Shell,

		Params:  orEmptyMap(t.Params),
		Secrets: []bus.SecretMount{},
		Inputs:  []bus.Input{},
		Outputs: []string{},

		Resources: bus.Resources{
			CPU:    t.Resources.CPU,
			Memory: t.Resources.Memory,
			PIDs:   t.Resources.PIDs,
		},
		Network:     t.Network.String(),
		EgressAllow: t.EgressAllow,
		RunsOn:      orEmptyList(t.RunsOn),

		Idempotent: t.Idempotent,
		CacheKey:   t.CacheKey,
		Deadline:   t.Deadline.UTC().Format(time.RFC3339Nano),
		Grant:      d.Grant,
	}
	// The workflow travels as name@commit, which is one string a person reads and one thing a
	// runner fetches the tree at.
	if t.Commit != "" {
		m.Workflow = t.Workflow + "@" + t.Commit
	}
	if !t.Shard.IsZero() {
		m.Shard = &bus.Shard{Index: t.Shard.Index, Of: t.Shard.Of}
	}
	if t.Timeout > 0 {
		m.Timeout = t.Timeout.String()
	}
	for _, f := range t.Files {
		m.Files = append(m.Files, bus.File{From: f.From, To: f.To, Mode: f.Mode})
	}
	for _, s := range t.Secrets {
		m.Secrets = append(m.Secrets, bus.SecretMount{Name: s.Name, Mount: s.Mount})
	}
	for _, port := range t.Outputs {
		m.Outputs = append(m.Outputs, string(port))
	}
	for _, port := range sortedPorts(d.Inputs) {
		in := d.Inputs[port]
		m.Inputs = append(m.Inputs, bus.Input{
			Port: string(port), Digest: "sha256:" + in.Digest, Items: in.Items,
		})
	}
	return m, nil
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orEmptyList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// sortedPorts keeps one message one message: Go randomises map iteration, and two encodings of
// one task that differed only in the order of a list would be two messages to anything comparing
// them.
func sortedPorts(m map[agk.Port]controller.InputRef) []agk.Port {
	out := make([]agk.Port, 0, len(m))
	for port := range m {
		out = append(out, port)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// answerOf is a result as the controller takes it, once package bus has read it and held it to the
// rules a result is checked against, which is how Answers is handed one.
//
// The artifacts are not handed on. The references the controller records are the files the
// published envelopes name, which carry the URI and the port whose retention they live by, and a
// digest and a size say neither; the list is held to its shape by the reader and read by nothing
// yet.
func answerOf(r bus.TaskResult) (controller.Answer, error) {
	a := controller.Answer{
		Result: graph.Result{
			Task:       agk.TaskID(r.IdempotencyKey),
			State:      r.State,
			StartedAt:  r.StartedAt,
			FinishedAt: r.FinishedAt,
		},
		Row:    r.TaskID,
		Runner: r.Runner,
	}
	if r.ExitCode != nil {
		a.Result.ExitCode = *r.ExitCode
	} else {
		a.Result.NoExitCode = true
	}
	for _, o := range r.Outputs {
		// The reader held the digest to sha256: and sixty-four lowercase hexadecimal
		// characters, and the controller names an envelope by the characters alone.
		a.Outputs = append(a.Outputs, controller.Output{Port: agk.Port(o.Port), Digest: strings.TrimPrefix(o.Digest, "sha256:"), Items: o.Items})
	}
	if r.Log != nil {
		l, err := agk.ParseLogURI(r.Log.URI)
		if err != nil {
			return controller.Answer{}, err
		}
		a.Log, a.LogLines, a.LogCut = l, r.Log.Lines, r.Log.Truncated
	}
	if r.Usage != nil {
		// Under the names the wire gives them, since the column holding it is read by a
		// person and a console and never by the engine, and without the two sampled figures
		// where the runner read no sample, as the wire carries them.
		a.Usage = map[string]any{"image_pull_ms": r.Usage.ImagePullMS}
		if r.Usage.CPUSeconds != nil && r.Usage.MaxRSSBytes != nil {
			a.Usage["cpu_seconds"] = *r.Usage.CPUSeconds
			a.Usage["max_rss_bytes"] = *r.Usage.MaxRSSBytes
		}
	}
	return a, nil
}
