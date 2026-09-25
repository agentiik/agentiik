package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/artifact/granted"
	"github.com/agentiik/agentiik/bus"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// Turning a task message and its redemption into what the driver runs.
//
// The controller writes a graph.Task as a task message (bus/control's messageOf), which carries a
// port, a digest and a count where the task carried a whole envelope, and a name and a mount where
// it would have needed a value. The runner is the other end: it redeems the grant, fetches what
// the redemption names and puts the task back together, so that driver.Run is handed the same
// graph.Task on a server that agk run --local hands it on a laptop. What only a server has, the
// task's own store, secret values and tree, travels beside it as driver.Sources.

// ErrNotAsNamed is something fetched for a task that is not what the task message and its
// redemption name: an input envelope that does not hash to its digest or does not hold the items
// the controller published, or a tree file that is not the bytes its digest names. The object
// store holds what it was given under each digest, so fetching again gets the same bytes, and the
// task is refused before any container exists.
var ErrNotAsNamed = errors.New("runner: what was fetched for the task is not what the task names")

// ErrNotRunnable is a task message that does not describe a task any runner could run: one that
// contradicts itself, names its workflow, deadline, timeout or network in a way nothing reads, or
// names a namespace no store can be opened for. Every runner reads it the same way, so nothing
// fetched again or asked again changes it, and it is refused before any container exists.
var ErrNotRunnable = errors.New("runner: the task message describes no task a runner can run")

// notRunnable marks a refusal as ErrNotRunnable and says what the refusal says.
type notRunnable struct{ err error }

func (n notRunnable) Error() string   { return n.err.Error() }
func (n notRunnable) Unwrap() []error { return []error{ErrNotRunnable, n.err} }

// TaskOf is a task message read back as the graph.Task the controller wrote it from, with inputs,
// the envelopes fetched for its input ports.
//
// It is messageOf the other way round. The idempotency key becomes the identity and is held to
// the run, step, attempt and shard the message spells out beside it; workflow@commit is split;
// the RFC 3339 deadline and the timeout are read back; and everything else the wire carries is
// carried. What the wire does not carry is not made up: a call is never dispatched to a runner,
// and whether a result may be cached is the controller's to decide from the cache key. A list or
// a map the wire writes empty is read back as none, which is how the controller holds one.
func TaskOf(m bus.TaskMessage, inputs map[agk.Port]agk.Envelope) (graph.Task, error) {
	id := agk.TaskID(m.IdempotencyKey)
	if err := id.Validate(); err != nil {
		return graph.Task{}, fmt.Errorf("runner: the task message: %w", err)
	}
	run, step, attempt, shard, _ := agk.ParseTaskID(m.IdempotencyKey)
	var said agk.Shard
	if m.Shard != nil {
		said = agk.Shard{Index: m.Shard.Index, Of: m.Shard.Of}
	}
	// The key is what the driver deduplicates on and the fields are what the container is
	// told, so a message where the two disagree would run as one task and be remembered as
	// another.
	if run != agk.RunID(m.RunID) || step != agk.Step(m.Step) || attempt != m.Attempt || shard != said {
		return graph.Task{}, fmt.Errorf("runner: the task message is %s and says run %s, step %s, attempt %d and shard %q, which is another task", id, m.RunID, m.Step, m.Attempt, said)
	}

	// The commit is the part after the last @, since a commit is hexadecimal and never carries
	// one. A message without one names no tree to lay out at /agk/repo.
	at := strings.LastIndex(m.Workflow, "@")
	if at <= 0 || at == len(m.Workflow)-1 {
		return graph.Task{}, fmt.Errorf("runner: task %s names the workflow %q, and a task message names one as name@commit", id, m.Workflow)
	}
	workflow, commit := m.Workflow[:at], m.Workflow[at+1:]

	deadline, err := time.Parse(time.RFC3339Nano, m.Deadline)
	if err != nil {
		return graph.Task{}, fmt.Errorf("runner: task %s carries the deadline %q, which is not an RFC 3339 instant", id, m.Deadline)
	}
	var timeout graph.Duration
	if m.Timeout != "" {
		if timeout, err = graph.ParseDuration(m.Timeout); err != nil {
			return graph.Task{}, fmt.Errorf("runner: task %s: timeout: %w", id, err)
		}
	}
	var network graph.Network
	switch m.Network {
	case "none":
		network = graph.NetworkNone
	case "egress":
		network = graph.NetworkEgress
	case "internal":
		network = graph.NetworkInternal
	default:
		return graph.Task{}, fmt.Errorf("runner: task %s asks for the network %q, and a posture is none, egress or internal", id, m.Network)
	}

	t := graph.Task{
		ID: id,
		// The dispatch, which names the span the container's trace context points at: the
		// controller exports each dispatch's span under its task_id, and a requeue after loss
		// takes a new one.
		Dispatch:  m.TaskID,
		Run:       run,
		Workflow:  workflow,
		Namespace: m.Namespace,
		Commit:    commit,
		Step:      step,
		Attempt:   attempt,
		Shard:     shard,
		Image:     m.Image,

		Script:       orNone(m.Script),
		BeforeScript: orNone(m.BeforeScript),
		AfterScript:  orNone(m.AfterScript),
		Shell:        orNone(m.Shell),

		Resources: graph.Resources{
			CPU:    m.Resources.CPU,
			Memory: m.Resources.Memory,
			PIDs:   m.Resources.PIDs,
		},
		Network:     network,
		EgressAllow: orNone(m.EgressAllow),
		RunsOn:      orNone(m.RunsOn),

		Timeout:    timeout,
		Deadline:   deadline,
		Idempotent: m.Idempotent,
		CacheKey:   m.CacheKey,
	}
	if len(m.Params) > 0 {
		t.Params = m.Params
	}
	for _, f := range m.Files {
		t.Files = append(t.Files, graph.FileSelector{From: f.From, To: f.To, Mode: f.Mode})
	}
	for _, s := range m.Secrets {
		t.Secrets = append(t.Secrets, graph.SecretMount{Name: s.Name, Mount: s.Mount})
	}
	for _, port := range m.Outputs {
		t.Outputs = append(t.Outputs, agk.Port(port))
	}

	// Every port the message names has its envelope, holding the items the controller
	// published on it, and nothing else arrives on a port the message does not name.
	for _, in := range m.Inputs {
		e, ok := inputs[agk.Port(in.Port)]
		if !ok {
			return graph.Task{}, fmt.Errorf("runner: task %s names input port %s, and no envelope was fetched for it", id, in.Port)
		}
		if len(e.Items) != in.Items {
			return graph.Task{}, fmt.Errorf("%w: task %s, input port %s: the controller published %d items and the envelope holds %d", ErrNotAsNamed, id, in.Port, in.Items, len(e.Items))
		}
		if t.Inputs == nil {
			t.Inputs = make(map[agk.Port]agk.Envelope, len(m.Inputs))
		}
		t.Inputs[agk.Port(in.Port)] = e
	}
	if len(inputs) != len(t.Inputs) {
		return graph.Task{}, fmt.Errorf("runner: task %s was given %d input envelopes for the %d ports its message names", id, len(inputs), len(t.Inputs))
	}
	return t, nil
}

// orNone reads a list the wire wrote empty as none.
func orNone(l []string) []string {
	if len(l) == 0 {
		return nil
	}
	return l
}

// Assembly is what assembling a task needs beside the message and its redemption.
type Assembly struct {
	// WorkRoot is the runner's work root, AGK_RUNNER_WORKDIR, under which the task's tree is
	// laid out in TreesDir.
	WorkRoot string

	// Limits are the size rules the task's envelopes and objects are held to. Its zero value
	// is agk's own, which is what the controller holds a task's envelopes to.
	Limits agk.Limits

	// Client is what every request to the object store goes through. Nil is one that follows
	// no redirect, since a presigned URL is the whole of its own authorisation.
	Client *http.Client
}

// Assembled is one task ready for the driver: the task, and the sources the driver answers it
// from in place of its own Config.
type Assembled struct {
	Task    graph.Task
	Sources driver.Sources
}

// Context is the context to hand driver.Run for this task, carrying its sources.
func (a *Assembled) Context(ctx context.Context) context.Context {
	return driver.WithSources(ctx, a.Sources)
}

// Remove takes away every tree laid out for the task, once its container is gone.
//
// Every tree, and not only this assembly's: an agent that restarted under the task's running
// container assembled it again, and the tree that container was given was laid out by the
// assembly before the restart, which nothing else holds any more. A host runs one container of a
// key at a time, since the driver refuses a key in flight or ended, so once this one is gone none
// of the key's trees is bound into anything. A tree already gone is the outcome asked for and no
// error.
func (a *Assembled) Remove() error {
	if a == nil || a.Sources.Repo == "" {
		return nil
	}
	trees, name := filepath.Split(a.Sources.Repo)
	at := strings.LastIndex(name, ".")
	if at < 0 {
		// Not a directory newTreeDir named, so nothing tells which trees are the key's.
		return os.RemoveAll(a.Sources.Repo)
	}
	of := name[:at]
	entries, err := os.ReadDir(trees)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runner: task %s: the trees under %s could not be listed: %w", a.Task.ID, trees, err)
	}
	var left []error
	for _, e := range entries {
		if at := strings.LastIndex(e.Name(), "."); at < 0 || e.Name()[:at] != of {
			continue
		}
		if err := os.RemoveAll(filepath.Join(trees, e.Name())); err != nil {
			left = append(left, err)
		}
	}
	if err := errors.Join(left...); err != nil {
		return fmt.Errorf("runner: task %s: a tree could not be removed: %w", a.Task.ID, err)
	}
	return nil
}

// Assemble turns a task message and the redemption of its grant into the graph.Task driver.Run
// takes, and the driver.Sources it takes it with.
//
// Everything the redemption names is fetched and checked here, before any container exists: each input envelope against the digest and the count the message
// gives it, and each file of the tree against its digest. The secret values are decoded from the
// encoding they travelled in, and are the task's own driver.Secrets, so that two tasks this runner
// holds at once, from two namespaces that both name billing, are each given their own value and
// each masked against it. The store reads through the redemption's URLs and writes under its one
// upload policy, and nothing else.
//
// A refusal wraps ErrAnswerUnusable where the redemption does not answer the message,
// ErrNotAsNamed where what was fetched is not what was named, and ErrNotRunnable where the message
// itself names no task a runner can run. Anything else is a fetch that may pass, and assembling
// again with the same redemption, whose URLs hold until the deadline, may get past it without
// reading the secrets a second time.
func Assemble(ctx context.Context, m bus.TaskMessage, r Redemption, o Assembly) (*Assembled, error) {
	if err := r.answers(m); err != nil {
		return nil, fmt.Errorf("%w: task %s: %w", ErrAnswerUnusable, m.IdempotencyKey, err)
	}
	// Nothing fetched for the task is any use past its deadline, when the grant and every
	// URL it answered expire, so a store that stops answering does not hold the assembly
	// longer than that.
	if deadline, err := time.Parse(time.RFC3339Nano, m.Deadline); err == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	limits := o.Limits
	if limits == (agk.Limits{}) {
		limits = agk.DefaultLimits()
	}
	secrets, err := decodeSecrets(r)
	if err != nil {
		return nil, err
	}

	objects, err := objectsOf(m.Namespace, r, o.Client)
	if err != nil {
		return nil, err
	}
	store, err := artifact.New(objects, m.Namespace, limits)
	if err != nil {
		return nil, notRunnable{fmt.Errorf("runner: task %s: %w", m.IdempotencyKey, err)}
	}

	inputs, err := fetchInputs(ctx, objects, m, r, limits)
	if err != nil {
		return nil, err
	}
	t, err := TaskOf(m, inputs)
	if err != nil {
		if errors.Is(err, ErrNotAsNamed) {
			return nil, err
		}
		return nil, notRunnable{err}
	}

	dir, err := newTreeDir(o.WorkRoot, t.ID)
	if err != nil {
		return nil, err
	}
	if err := layOutTree(ctx, objects, m.Namespace, dir, r.Tree, limits); err != nil {
		return nil, fmt.Errorf("runner: task %s: %w", t.ID, err)
	}
	return &Assembled{Task: t, Sources: driver.Sources{Store: store, Secrets: secrets, Repo: dir}}, nil
}

// objectsOf is the store as one task's redemption lets it be reached: the presigned GET of every
// object it names, under the key the namespace writes that object at, and the upload policy.
//
// Two entries naming one digest are one object, since the key is the digest, and either URL
// reads it.
func objectsOf(namespace string, r Redemption, client *http.Client) (*granted.Objects, error) {
	get := map[string]string{}
	name := func(digest, url string) {
		key := artifact.Key(namespace, digest)
		if _, held := get[key]; !held {
			get[key] = url
		}
	}
	for _, in := range r.Inputs {
		if hex, ok := hexOf(in.Envelope.Digest); ok {
			name(hex, in.Envelope.URL)
		}
		for _, a := range in.Artifacts {
			name(a.SHA256, a.URL)
		}
	}
	for _, f := range r.Tree {
		name(f.SHA256, f.URL)
	}
	o, err := granted.New(granted.Options{
		Get:     get,
		Uploads: artifact.Policy{URL: r.Uploads.URL, Fields: r.Uploads.Fields, KeyPrefix: r.Uploads.KeyPrefix},
		Client:  client,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAnswerUnusable, err)
	}
	return o, nil
}
