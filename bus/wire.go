package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/controller"
	"github.com/agentiik/agentiik/graph"
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

// TaskResult is agentiik/schemas wire.schema.json, $defs/taskResult: what a runner publishes when
// a task ends, and "the only message the controller reads to decide what happens next".
//
// It names the task twice, by dispatch and by key, and repeats nothing the key already spells. It
// carries no item and no artifact content: "what travels is a digest per port and a digest per
// artifact, which is what lets the controller schedule on metadata while the payload stays behind
// run:read_data". So a runner uploads what it produced before it reports, and the controller reads
// the envelopes back by digest.
type TaskResult struct {
	TaskID         string        `json:"task_id"`
	IdempotencyKey string        `json:"idempotency_key"`
	Runner         string        `json:"runner"`
	State          agk.TaskState `json:"state"`

	// ExitCode and the two instants describe a container, and are present exactly where one
	// ran: "succeeded and failed report an exit code and a span, lost reports neither, because
	// the point of lost is that there is no outcome to report".
	ExitCode   *int      `json:"exit_code,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`

	// Outputs is nil where a result says nothing about ports and empty where it says the task
	// published none. The two are different statements, and the wire requires the second of a
	// success "so that the controller can tell a step that published nothing from a runner that
	// said nothing".
	Outputs   []Output   `json:"outputs,omitzero"`
	Artifacts []Artifact `json:"artifacts,omitzero"`

	Log   *Log   `json:"log,omitempty"`
	Usage *Usage `json:"usage,omitempty"`
}

// Output is one port's envelope, named rather than carried, which is how an input is named on the
// way in.
type Output struct {
	Port   string `json:"port"`
	Digest string `json:"digest"`
	Items  int    `json:"items"`
}

// Artifact is one object a task wrote, by digest and size. The key names the algorithm, so the
// digest does not repeat it, "exactly as an envelope writes a file's digest".
type Artifact struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Log is where a task's output went, how much of it there is and whether it was cut.
type Log struct {
	URI       string `json:"uri"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
}

// Usage is what the container consumed, from one read of its statistics.
type Usage struct {
	CPUSeconds  float64 `json:"cpu_seconds"`
	MaxRSSBytes int64   `json:"max_rss_bytes"`
	ImagePullMS int64   `json:"image_pull_ms"`
}

// encode writes a result the way it travels, once it is one a controller would read.
func (r TaskResult) encode() ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// readResult reads one result off the bus and answers it as the controller takes it.
//
// Closed, as the document is. A field nobody here knows is refused rather than dropped, because a
// result that says more than the wire describes comes from a runner written against something
// else, and whatever the extra field meant would be lost without anybody hearing of it.
func readResult(body []byte) (controller.Answer, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var r TaskResult
	if err := dec.Decode(&r); err != nil {
		return controller.Answer{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return controller.Answer{}, errors.New("a result is one document, and this message carries more after it")
	}
	if err := r.check(); err != nil {
		return controller.Answer{}, err
	}
	return answerOf(r)
}

// check holds a result to the rules the controller acts on.
//
// Not the whole schema: the shape is JSON Schema's to state and the conformance test's to hold,
// against the vendored document. What is written out here is what a controller would otherwise
// read wrongly or not at all: which dispatch and which runner, whether it is an ending, what a
// container reported where one ran, and every name that is about to become an object key or a row.
// The runner's grammar is left out, because the wire prints lowercase names and a runner answers to
// the identifier the API minted it, which is a ULID.
func (r TaskResult) check() error {
	if !isULID(r.TaskID) {
		return fmt.Errorf("task_id %q is not a dispatch identifier: a result carries back the one its task message carried", r.TaskID)
	}
	if err := agk.TaskID(r.IdempotencyKey).Validate(); err != nil {
		return fmt.Errorf("idempotency_key: %w", err)
	}
	if r.Runner == "" {
		return fmt.Errorf("the result of %s names no runner, and it is the runner a result is taken from", r.IdempotencyKey)
	}
	if !r.State.Terminal() {
		return fmt.Errorf("the result of %s is %s, which is not one of the five endings a result reports: a heartbeat is what says a task is still going", r.IdempotencyKey, r.State)
	}

	// What a container reported, where one ran. started_at is what says one did, and the
	// rest hangs off it.
	ran := !r.StartedAt.IsZero()
	container := r.ExitCode != nil || !r.FinishedAt.IsZero() || r.Outputs != nil || r.Artifacts != nil || r.Usage != nil
	switch {
	case container && !ran:
		return fmt.Errorf("the result of %s describes a container and has no started_at, which is what says one started", r.IdempotencyKey)
	case r.ExitCode != nil && (*r.ExitCode < 0 || *r.ExitCode > 255):
		return fmt.Errorf("the result of %s exited %d, which no container exits with", r.IdempotencyKey, *r.ExitCode)
	case r.ExitCode != nil && r.FinishedAt.IsZero():
		return fmt.Errorf("the result of %s has an exit code and no finished_at, and an exit is a code and an instant", r.IdempotencyKey)
	case ran && (r.State == agk.TaskSucceeded || r.State == agk.TaskFailed) && (r.ExitCode == nil || r.FinishedAt.IsZero()):
		return fmt.Errorf("the result of %s is %s from a container that started, and that container exited: an exit is a code and an instant", r.IdempotencyKey, r.State)
	case r.State == agk.TaskSucceeded && (r.ExitCode == nil || *r.ExitCode != 0):
		return fmt.Errorf("the result of %s is succeeded, and success is exit code 0 and nothing else", r.IdempotencyKey)
	case r.State == agk.TaskSucceeded && r.Outputs == nil:
		return fmt.Errorf("the result of %s is succeeded and names no ports, and a success names every one, the empty list included", r.IdempotencyKey)
	case r.State == agk.TaskLost && (container || r.Log != nil):
		return fmt.Errorf("the result of %s is lost and describes an outcome, and the point of lost is that there is none", r.IdempotencyKey)
	}

	ports := make(map[string]bool, len(r.Outputs))
	for _, o := range r.Outputs {
		if err := agk.Port(o.Port).Validate(); err != nil {
			return fmt.Errorf("the result of %s: %w", r.IdempotencyKey, err)
		}
		if ports[o.Port] {
			return fmt.Errorf("the result of %s names port %s twice, and a port publishes one envelope", r.IdempotencyKey, o.Port)
		}
		ports[o.Port] = true
		if _, ok := hexOf(o.Digest); !ok {
			return fmt.Errorf("the result of %s names %q on port %s, and an envelope is named sha256: and sixty-four lowercase hexadecimal characters", r.IdempotencyKey, o.Digest, o.Port)
		}
		if o.Items < 0 {
			return fmt.Errorf("the result of %s counts %d items on port %s", r.IdempotencyKey, o.Items, o.Port)
		}
	}
	for _, a := range r.Artifacts {
		if !isHex64(a.SHA256) || a.Bytes < 0 {
			return fmt.Errorf("the result of %s names an artifact as %q of %d bytes, and one is sixty-four lowercase hexadecimal characters and a size", r.IdempotencyKey, a.SHA256, a.Bytes)
		}
	}
	if r.Log != nil {
		l, err := agk.ParseLogURI(r.Log.URI)
		if err != nil {
			return fmt.Errorf("the result of %s: %w", r.IdempotencyKey, err)
		}
		// A log is kept under its run's prefix, which is what a retention sweep over the
		// run's logs lists. One addressed under another run would outlive its own and be
		// swept with somebody else's.
		if run, _, _, _, err := agk.ParseTaskID(r.IdempotencyKey); err == nil && l.Run != run {
			return fmt.Errorf("the result of %s puts its log under run %s", r.IdempotencyKey, l.Run)
		}
		if r.Log.Lines < 0 {
			return fmt.Errorf("the result of %s counts %d lines of log", r.IdempotencyKey, r.Log.Lines)
		}
	}
	if u := r.Usage; u != nil && (u.CPUSeconds < 0 || u.MaxRSSBytes < 0 || u.ImagePullMS < 0) {
		return fmt.Errorf("the result of %s measures a negative usage", r.IdempotencyKey)
	}
	return nil
}

// answerOf is a result as the controller takes it.
//
// The artifacts are not handed on. The references the controller records are the files the
// published envelopes name, which carry the URI and the port whose retention they live by, and a
// digest and a size say neither; the list is held to its shape here and read by nothing yet.
func answerOf(r TaskResult) (controller.Answer, error) {
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
	}
	for _, o := range r.Outputs {
		digest, _ := hexOf(o.Digest)
		a.Outputs = append(a.Outputs, controller.Output{Port: agk.Port(o.Port), Digest: digest, Items: o.Items})
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
		// person and a console and never by the engine.
		a.Usage = map[string]any{
			"cpu_seconds":   r.Usage.CPUSeconds,
			"max_rss_bytes": r.Usage.MaxRSSBytes,
			"image_pull_ms": r.Usage.ImagePullMS,
		}
	}
	return a, nil
}

// isULID holds an identifier to the alphabet the engine mints in, and not to a length: the
// documentation prints shorter ones than it mints, and the wire's own pattern takes both.
func isULID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U':
		default:
			return false
		}
	}
	return true
}

// hexOf reads the hexadecimal out of a digest written as the wire writes one, sha256: and sixty-four
// lowercase characters, which is the form an object key is built from once the algorithm is off.
func hexOf(digest string) (string, bool) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !isHex64(hex) {
		return "", false
	}
	return hex, true
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
