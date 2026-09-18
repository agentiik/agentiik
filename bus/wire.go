package bus

import (
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/controller"
)

// The wire, which is what actually travels.
//
// The domain types are not the message. graph.Task carries the whole input envelope, items and
// all, because that is what an evaluator hands a driver in one process; a task message carries a
// port, a digest and a count, because "A task message carries no business payload, no secret
// value and no URL that would work without the grant" and because "the entry is closed so that
// one cannot be written here at all: that is what makes a copy of the message worth nothing on
// its own".
//
// Publishing the domain type instead is the mistake this file exists to undo. It put the items
// on the queue, spelled half the fields the way Go spells them rather than the way the schema
// does, and dropped every value that happened to be a zero. None of it was visible, because
// nothing checked what went out against the document that describes it. A conformance test does
// now.

// TaskMessage is agentiik/schemas wire.schema.json, $defs/taskMessage.
//
// Written out rather than generated, because every field here is a decision somebody can read:
// what travels, what is a name rather than a value, and what a runner is trusted to work out.
type TaskMessage struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	RunID          string `json:"run_id"`
	Namespace      string `json:"namespace"`
	Workflow       string `json:"workflow"`
	Step           string `json:"step"`
	Attempt        int    `json:"attempt"`
	Shard          *Shard `json:"shard,omitempty"`
	Image          string `json:"image"`

	Script       []string `json:"script,omitempty"`
	BeforeScript []string `json:"before_script,omitempty"`
	AfterScript  []string `json:"after_script,omitempty"`
	Shell        []string `json:"shell,omitempty"`
	Files        []File   `json:"files,omitempty"`

	Params  map[string]any `json:"params"`
	Secrets []SecretMount  `json:"secrets"`
	Inputs  []Input        `json:"inputs"`
	Outputs []string       `json:"outputs"`

	Resources   Resources `json:"resources"`
	Network     string    `json:"network"`
	EgressAllow []string  `json:"egress_allow,omitempty"`
	RunsOn      []string  `json:"runs_on"`

	Timeout    string `json:"timeout,omitempty"`
	Idempotent bool   `json:"idempotent,omitempty"`
	CacheKey   string `json:"cache_key,omitempty"`

	Deadline string `json:"deadline"`
	Grant    string `json:"grant"`
}

// Shard is which piece of a fan-out this is, and how many there are.
type Shard struct {
	Index int `json:"index"`
	Of    int `json:"of"`
}

// File is one repository file the step asked for, and where the container sees it. The bytes are
// fetched from the tree by redeeming the grant.
type File struct {
	From string `json:"from"`
	To   string `json:"to"`
	Mode string `json:"mode,omitempty"`
}

// SecretMount is a name and a mount point and never a value.
type SecretMount struct {
	Name  string `json:"name"`
	Mount string `json:"mount"`
}

// Input is one port's envelope, named rather than carried.
type Input struct {
	Port   string `json:"port"`
	Digest string `json:"digest"`
	Items  int    `json:"items"`
}

// Resources are the ceilings the container is created with.
type Resources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	PIDs   int    `json:"pids"`
}

// messageOf turns what the evaluator decided into what the wire describes.
//
// Everything the schema requires is written, including the empty list and the zero, because a
// required key dropped for being empty is a message a closed document refuses: "a runner reading
// an absent field would be deciding something the controller had already decided".
func messageOf(d controller.Dispatch) (TaskMessage, error) {
	t := d.Task
	if d.Grant == "" {
		return TaskMessage{}, fmt.Errorf("task %s carries no grant, and a message without one asks a runner to do work it cannot fetch the inputs for", t.ID)
	}
	if d.Row == "" {
		return TaskMessage{}, fmt.Errorf("task %s names no row, and task_id is what a grant and a log are addressed by", t.ID)
	}
	if t.Deadline.IsZero() {
		return TaskMessage{}, fmt.Errorf("task %s carries no deadline, and the runner has nothing to stop the container at", t.ID)
	}

	m := TaskMessage{
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
		Secrets: []SecretMount{},
		Inputs:  []Input{},
		Outputs: []string{},

		Resources: Resources{
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
		m.Shard = &Shard{Index: t.Shard.Index, Of: t.Shard.Of}
	}
	if t.Timeout > 0 {
		m.Timeout = t.Timeout.String()
	}
	for _, f := range t.Files {
		m.Files = append(m.Files, File{From: f.From, To: f.To, Mode: f.Mode})
	}
	for _, s := range t.Secrets {
		m.Secrets = append(m.Secrets, SecretMount{Name: s.Name, Mount: s.Mount})
	}
	for _, port := range t.Outputs {
		m.Outputs = append(m.Outputs, string(port))
	}
	for _, port := range sortedPorts(d.Inputs) {
		in := d.Inputs[port]
		m.Inputs = append(m.Inputs, Input{
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
