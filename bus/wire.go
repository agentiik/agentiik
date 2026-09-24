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
//
// The types are here and the translation is not. Writing what the controller decided as a task
// message, and reading a result back as the answer the controller takes, name the controller's
// types, and package bus/control does both. What stays is what a runner needs: the message it
// takes, the result it reports, and the rules a result is held to on the way out and on the way
// back.

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
//
// To is left out where the step asked for no relocation, as a selector's short form never does and
// its long form need not: the file is then where it is under /agk/repo/, and an empty to is one the
// wire refuses. A runner reads one left out as the empty string, which is no relocation.
type File struct {
	From string `json:"from"`
	To   string `json:"to,omitempty"`
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
//
// Each is left out where nothing decided it, neither the step nor its pool's ceiling: "a ceiling
// the controller did not decide is absent rather than guessed at", and the runner falls back to its
// own policy. Written empty instead, "cpu": "" and "pids": 0 are values the wire's patterns and
// its minimum refuse, so the message would be one no reader of the wire accepts.
type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	PIDs   int    `json:"pids,omitempty"`
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
	// ran: "succeeded and failed report an exit code and a span; timed_out and cancelled report
	// both wherever a container started, since a stopped container exits too; lost reports
	// neither, because the point of lost is that there is no outcome to report".
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

// Usage is what the container consumed and what its image cost to pull.
//
// The two figures spent inside the container are sampled from the daemon's statistics while it
// runs, and travel together or not at all: a container that exited before the first sample was
// read carries neither, because a zero there would be a measurement nobody made. They are pointers
// so that a zero the daemon did count, a container that spent no CPU worth a sample, still travels
// as one. The pull is timed on this side before the container starts, and is always there.
type Usage struct {
	CPUSeconds  *float64 `json:"cpu_seconds,omitempty"`
	MaxRSSBytes *int64   `json:"max_rss_bytes,omitempty"`
	ImagePullMS int64    `json:"image_pull_ms"`
}

// Check holds a result to the rules Report holds it to before it goes out, which is how a runner
// that keeps a result to publish later refuses to keep one no publication would ever take.
func (r TaskResult) Check() error { return r.check() }

// encode writes a result the way it travels, once it is one a controller would read.
func (r TaskResult) encode() ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// readResult reads one result off the bus, as the wire describes it.
//
// Closed, as the document is. A field nobody here knows is refused rather than dropped, because a
// result that says more than the wire describes comes from a runner written against something
// else, and whatever the extra field meant would be lost without anybody hearing of it. Closed as
// far as encoding/json closes a document, which is not quite as far as the schema: a field spelled
// in another case is read as the field it spells, and a null where an optional field could be is
// read as its absence. Neither says anything the field would not.
func readResult(body []byte) (TaskResult, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var r TaskResult
	if err := dec.Decode(&r); err != nil {
		return TaskResult{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return TaskResult{}, errors.New("a result is one document, and this message carries more after it")
	}
	if err := r.check(); err != nil {
		return TaskResult{}, err
	}
	return r, nil
}

// check holds a result to the rules the controller acts on.
//
// Not the whole schema: the shape is JSON Schema's to state and the conformance test's to hold,
// against the vendored document. What is written out here is what a controller would otherwise
// read wrongly or not at all: which dispatch and which runner, whether it is an ending, what a
// container reported where one ran, and every name that is about to become an object key, a row or
// a subject. The runner is held to the wire's lowercase grammar, which is the one the API mints
// its identifier in, and which can be a subject token.
func (r TaskResult) check() error {
	if !isULID(r.TaskID) {
		return fmt.Errorf("task_id %q is not a dispatch identifier: a result carries back the one its task message carried", r.TaskID)
	}
	if err := agk.TaskID(r.IdempotencyKey).Validate(); err != nil {
		return fmt.Errorf("idempotency_key: %w", err)
	}
	if err := validRunner(r.Runner); err != nil {
		return fmt.Errorf("the result of %s: %w", r.IdempotencyKey, err)
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
	if u := r.Usage; u != nil {
		switch {
		case (u.CPUSeconds == nil) != (u.MaxRSSBytes == nil):
			return fmt.Errorf("the result of %s carries one of cpu_seconds and max_rss_bytes without the other, and the two are read off the same samples", r.IdempotencyKey)
		case u.CPUSeconds != nil && (*u.CPUSeconds < 0 || *u.MaxRSSBytes < 0), u.ImagePullMS < 0:
			return fmt.Errorf("the result of %s measures a negative usage", r.IdempotencyKey)
		}
	}
	return nil
}

// TaskProgress is agentiik/schemas wire.schema.json, $defs/taskProgress: what a runner publishes
// when a task it holds moves on without ending, running once its container has started and
// publishing once the container has exited and its outputs are being uploaded.
//
// It goes on the runner's results subject, because that subject is what says who sent it, and a
// runner that could say a task was running on a subject anybody may publish on could show work
// running that nobody runs. Two kinds of message on one subject have to be told apart before either
// is read, so this one says progress where a result says state, and neither carries the other's
// keyword. It carries no instant: the ending carries the container's span as the container reports
// it, and a second clock writing started_at would be a second answer to one question.
type TaskProgress struct {
	TaskID         string        `json:"task_id"`
	IdempotencyKey string        `json:"idempotency_key"`
	Runner         string        `json:"runner"`
	Progress       agk.TaskState `json:"progress"`
}

// encode writes a progress message the way it travels, once it is one a controller would read.
func (p TaskProgress) encode() ([]byte, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// check holds a progress message to the rules the controller acts on: which dispatch, which
// runner, and a state between dispatched and an ending.
func (p TaskProgress) check() error {
	if !isULID(p.TaskID) {
		return fmt.Errorf("task_id %q is not a dispatch identifier: a progress message carries back the one its task message carried", p.TaskID)
	}
	if err := agk.TaskID(p.IdempotencyKey).Validate(); err != nil {
		return fmt.Errorf("idempotency_key: %w", err)
	}
	if err := validRunner(p.Runner); err != nil {
		return fmt.Errorf("the progress of %s: %w", p.IdempotencyKey, err)
	}
	if p.Progress != agk.TaskRunning && p.Progress != agk.TaskPublishing {
		return fmt.Errorf("the progress of %s is %s, and a task only reports running or publishing on its way to an ending, which a result reports", p.IdempotencyKey, p.Progress)
	}
	return nil
}

// readProgress reads one progress message off the bus, closed as readResult reads a result and for
// the same reasons.
func readProgress(body []byte) (TaskProgress, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var p TaskProgress
	if err := dec.Decode(&p); err != nil {
		return TaskProgress{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return TaskProgress{}, errors.New("a progress message is one document, and this message carries more after it")
	}
	if err := p.check(); err != nil {
		return TaskProgress{}, err
	}
	return p, nil
}

// isProgress says whether a document off a results subject is a progress message rather than a
// result, which is whether it carries the keyword progress at its top level. Only the keyword is
// read, and nothing about its value: a document that is neither is refused by whichever reader it
// goes to, and one carrying both keywords by readProgress, which knows no state.
func isProgress(body []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return false
	}
	_, ok := top["progress"]
	return ok
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

// hexOf reads the hexadecimal out of a digest written as the wire writes one, sha256: and
// sixty-four lowercase characters, which is the form an object key is built from once the
// algorithm is off.
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
