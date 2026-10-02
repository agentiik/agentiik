// Package trigger starts runs, whatever asked for them, and arms what a workflow declares under on.
//
// "Five ways in, one path. Every trigger kind creates its run the same way": a person at a client,
// a schedule coming round, a webhook arriving, an event published and a workflow calling another
// are each authenticated and authorised where they arrive, and then take Start, which resolves the
// ref to a commit, binds the inputs against that version's declaration, writes the run and tells
// the controller, in one transaction that records the run.trigger entry last. So a trigger kind
// added later is quota-limited, attributed and audited with no code of its own, and a rule about
// starting a run is one rule rather than one per kind.
package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/audit"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	"github.com/agentiik/agentiik/schema"
)

// Versions resolves a workflow's commit to its graph, as version.Store does, and a cache of it.
type Versions interface {
	Graph(ctx context.Context, namespace, workflow, commit string) (*graph.Graph, error)
}

// Starter is the one path a run is created by.
type Starter struct {
	pool     *db.Pool
	versions Versions
	objects  artifact.Objects
	declared declarations
	report   func(error)
	now      func() time.Time
}

// Options are what a Starter reads: the database, the versions, and the object store an input's
// schema naming a file of the tree is read from. Report hears what failed that was the
// installation's rather than the caller's; Now is the clock, time.Now where nil.
type Options struct {
	Pool     *db.Pool
	Versions Versions
	Objects  artifact.Objects
	Report   func(error)
	Now      func() time.Time
}

// New is a Starter over what o names.
func New(o Options) (*Starter, error) {
	if o.Pool == nil || o.Versions == nil {
		return nil, errors.New("trigger: a starter needs the database and the versions")
	}
	s := &Starter{pool: o.Pool, versions: o.Versions, objects: o.Objects, report: o.Report, now: o.Now}
	if s.report == nil {
		s.report = func(error) {}
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Request is one run asked for.
type Request struct {
	Namespace, Workflow string

	// Kind is what asked, and By the principal the run is attributed to: whoever asked, for the
	// kinds somebody asks by, and nobody for a schedule, a webhook or an event, whose runs are the
	// namespace's built-in identity's, which the database writes.
	Kind agk.TriggerKind
	By   string

	// Commit is the version to run, or Ref a branch, a tag or a whole commit resolved to one now,
	// once, or neither, which runs the default branch's head. Both is refused.
	Commit, Ref string

	// Inputs are what was supplied, each number a json.Number as it was written, bound here against
	// the version's declaration with required and default applied. Bound takes their place for a run
	// whose inputs were bound already, a replay's, and is written as it is.
	Inputs map[string]any
	Bound  json.RawMessage

	// ReplayOf and ReplayFrom make the run a replay of another, from a step or from the start.
	ReplayOf   agk.RunID
	ReplayFrom agk.Step

	// Detail is added to what the run.trigger entry records of the run.
	Detail map[string]any

	// Context is what fired the run as expressions read it, frozen on the run: the trigger root,
	// body, headers, query and scheduled_for, and an event trigger's event. A replay is handed the
	// context of the run it replays, so that it sees what fired it and not what is true now.
	Context db.TriggerContext

	// NamespaceVars are the variables the namespace shows the workflow, for a run whose were read
	// already: a replay's, those the run it replays read, so that it reads what that run read and
	// not what is true now; a webhook's or an event's, those its map and its filter read, so that
	// its steps read what they did. Nil, which every other run leaves it, reads them in Create's
	// transaction, the one that writes the run. Either way the run keeps those its file does not
	// write, frozen on it.
	NamespaceVars map[string]any

	// Caller is the step whose call asks for the run, for the kind workflow, and Depth how deep
	// in a chain of calls the run is.
	Caller *db.Caller
	Depth  int
}

// Started is a run created: its identifier and the commit it is pinned to.
type Started struct {
	Run    agk.RunID
	Commit string
}

// ErrCommitAndRef is a request naming a commit and a ref, where a run is of one commit.
var ErrCommitAndRef = errors.New("the request names a commit and a ref, and a run is of one commit: name the commit, or the ref the installation resolves to one")

// Start creates the run r asks for, and answers what it created, or with a refusal the commit its
// ref resolved to where it resolved: Prepare, then Create in a transaction of its own. What it
// refuses is one of the errors this package or db names, the caller's to answer:
// db.ErrNoWorkflow and db.ErrNoVersion for nothing to run, *db.RefUnresolved for a ref naming
// nothing or two things, version.ErrLibrary for a library, *schema.InputRefusal and
// *InputsTooLarge for inputs, *db.RunsPerHourReached for the quota, db.ErrWorkflowMoving for a
// workflow on its way to another namespace, and ErrNoObjectStore for a schema naming a file where
// no store is attached.
func (s *Starter) Start(ctx context.Context, r Request) (Started, error) {
	p, err := s.Prepare(ctx, r)
	if err != nil {
		return Started{Commit: p.commit}, err
	}
	var run agk.RunID
	var reached *db.RunsPerHourReached
	err = s.pool.In(ctx, r.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		run, err = p.Create(ctx, ns)
		if errors.As(err, &reached) {
			// Committed rather than rolled back: the refusal is counted where it was decided, in
			// this transaction, for the chart of the namespace against its quotas, and nothing
			// else was written in it.
			return nil
		}
		return err
	})
	if err == nil && reached != nil {
		err = reached
	}
	if err != nil {
		return Started{Commit: p.commit}, err
	}
	return Started{Run: run, Commit: p.commit}, nil
}

// Prepared is a run asked for, with everything read that it is made of: the commit its ref
// resolved to, its graph and its inputs bound. Nothing is written until Create.
type Prepared struct {
	r      Request
	commit string
	g      *graph.Graph
	inputs json.RawMessage
}

// Commit is the commit the run will be pinned to.
func (p Prepared) Commit() string { return p.commit }

// Prepare reads what the run r asks for is made of, and refuses what Start refuses before anything
// is written. On a refusal the Prepared still names the commit the ref resolved to, where it did.
func (s *Starter) Prepare(ctx context.Context, r Request) (Prepared, error) {
	if r.Commit != "" && r.Ref != "" {
		return Prepared{}, ErrCommitAndRef
	}
	commit, err := s.resolve(ctx, r)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{r: r, commit: commit}
	g, err := s.versions.Graph(ctx, r.Namespace, r.Workflow, p.commit)
	if err != nil {
		return p, err
	}
	p.g, p.inputs = g, r.Bound
	if p.inputs == nil {
		if p.inputs, err = s.bind(ctx, r.Namespace, r.Workflow, p.commit, g, r.Inputs); err != nil {
			return p, err
		}
	}
	return p, nil
}

// resolve answers the commit a run r asks for is of: the commit it names, or the one its ref
// resolves to now, or the default branch's head where it names neither.
func (s *Starter) resolve(ctx context.Context, r Request) (string, error) {
	if r.Commit != "" {
		return r.Commit, nil
	}
	var commit string
	err := s.pool.In(ctx, r.Namespace, func(ctx context.Context, ns *db.NS) error {
		var err error
		if r.Ref != "" {
			// A ref other than the default branch, resolved here, once, and the run pinned to
			// the commit it names now, whatever the ref does next.
			commit, err = ns.ResolveRef(ctx, r.Workflow, r.Ref)
		} else {
			// "A run naming no ref runs the default branch's head."
			commit, err = ns.DefaultCommit(ctx, r.Workflow)
		}
		return err
	})
	return commit, err
}

// Declaration is what a manual run of one version takes: the commit a ref resolved to, the inputs
// that version declares as its file writes them, and each file of its tree their schemas reach by
// $ref, by its path, as parsed JSON.
type Declaration struct {
	Commit string
	Inputs map[string]graph.Input
	Files  map[string]any
}

// Declared answers what the run r asks for would take, without asking for it: the ref resolved
// and the graph read as Prepare reads them, and the declaration compiled against the version's
// tree as a run binds against it, so that a declaration a run would refuse is refused here too,
// with the same errors. Nothing is written, and r's inputs are not read.
//
// It is what a form asks before a run is asked for, by whoever may ask for one: the inputs are the
// workflow's boundary, and nothing else of the file is answered.
func (s *Starter) Declared(ctx context.Context, r Request) (Declaration, error) {
	if r.Commit != "" && r.Ref != "" {
		return Declaration{}, ErrCommitAndRef
	}
	commit, err := s.resolve(ctx, r)
	if err != nil {
		return Declaration{}, err
	}
	g, err := s.versions.Graph(ctx, r.Namespace, r.Workflow, commit)
	if err != nil {
		return Declaration{Commit: commit}, err
	}
	wf := g.Workflow()
	tree := &versionTree{ctx: ctx, pool: s.pool, objects: s.objects, namespace: r.Namespace, workflow: r.Workflow, commit: commit}
	files, err := wf.InputFiles(tree)
	switch {
	case errors.Is(tree.trouble, ErrNoObjectStore):
		return Declaration{Commit: commit}, ErrNoObjectStore
	case tree.trouble != nil:
		return Declaration{Commit: commit}, fmt.Errorf("the tree of %s/%s@%s could not be read to answer its inputs: %w", r.Namespace, r.Workflow, commit, tree.trouble)
	case err != nil:
		return Declaration{Commit: commit}, fmt.Errorf("%w: %v", ErrDeclarationRefused, err)
	}
	return Declaration{Commit: commit, Inputs: wf.Inputs, Files: files}, nil
}

// Create writes the run, tells the controller and records run.trigger, in the transaction ns
// carries, and answers the run: the one place a run is written, so that a trigger added later is
// quota-limited, attributed and audited with no code of its own. A caller firing a schedule does it
// in the fenced transaction that moves the schedule on, so that a controller that lost the lead
// neither starts a run nor records one fired.
func (p Prepared) Create(ctx context.Context, ns *db.NS) (agk.RunID, error) {
	r := p.r
	run := agk.NewRunID()
	actor := r.By
	if r.Kind.Unattended() {
		actor = r.Namespace + "/" + db.BuiltIn
	}
	detail := map[string]any{"workflow": r.Workflow, "commit": p.commit}
	if r.Ref != "" {
		detail["ref"] = r.Ref
	}
	// The kind, for every kind but the one every run was before v0.5.0, so that an entry of a
	// person's run reads as it always has and one a trigger made says which.
	if r.Kind != agk.TriggerManual {
		detail["trigger_kind"] = r.Kind.String()
	}
	if r.Caller != nil {
		detail["from"] = map[string]any{"run": string(r.Caller.Run), "step": string(r.Caller.Step)}
		detail["depth"] = r.Depth
	}
	for k, v := range r.Detail {
		detail[k] = v
	}
	vars, err := p.namespaceVars(ctx, ns)
	if err != nil {
		return "", err
	}
	if err := ns.CreateRun(ctx, db.NewRun{
		ID: run, Workflow: r.Workflow, Commit: p.commit,
		Trigger: r.Kind, TriggeredBy: r.By,
		Inputs: p.inputs, Steps: p.g.Steps(),
		ReplayOf: r.ReplayOf, ReplayFrom: r.ReplayFrom,
		Context:       r.Context,
		NamespaceVars: vars,
		Caller:        r.Caller, Depth: r.Depth,
	}); err != nil {
		return "", err
	}
	// In the same transaction, because PostgreSQL delivers the notification only when it commits:
	// the row and the wake-up are one fact rather than two.
	if err := ns.NotifyRun(ctx, run); err != nil {
		return "", err
	}
	// Recorded last, so that a run never starts unrecorded and the chain's lock is held for no
	// longer than the commit.
	if err := ns.Audit(ctx, audit.Record{
		Actor: actor, Action: audit.RunTrigger, Target: string(run), Result: audit.Done,
		Detail: detail,
	}); err != nil {
		return "", err
	}
	return run, nil
}

// namespaceVars are the namespace's variables the run keeps: those its namespace shows its workflow,
// read here, in the transaction that writes the run, unless the request carries them, less those
// its file writes, whose value the file's is. Read once, when the run is created, and kept, so that
// a variable written in the middle of a run never gives its first step one value and its last
// another, and a controller taking it over reads what the one before it did.
func (p Prepared) namespaceVars(ctx context.Context, ns *db.NS) (map[string]any, error) {
	shown := p.r.NamespaceVars
	if shown == nil {
		var err error
		if shown, err = ns.VariablesFor(ctx, p.r.Workflow); err != nil {
			return nil, err
		}
	}
	own := p.g.Workflow().Vars
	kept := make(map[string]any, len(shown))
	for name, value := range shown {
		if _, written := own[name]; !written {
			kept[name] = value
		}
	}
	return kept, nil
}

// InputsTooLarge is inputs past what a run's inputs may hold once the defaults the workflow declares
// are in: as many values as one envelope may carry items, and one envelope's weight.
type InputsTooLarge struct{ Why string }

func (e *InputsTooLarge) Error() string { return e.Why }

// ErrNoObjectStore is an input's schema naming a file of the version's tree where no object store
// is attached to read it from.
var ErrNoObjectStore = errors.New("this installation has no object store attached, and an input's schema names a file of the version's tree, which is kept there")

// ErrDeclarationRefused is a version whose own declaration of its inputs does not compile, which a
// push refuses, so that only a version pushed before then holds one. Nothing a request changes
// would start it.
var ErrDeclarationRefused = errors.New("the version's declaration of its inputs does not compile")

// InputsMaxValues is how many values the inputs of a run may hold, counting every object, array,
// string, number, boolean and null at any depth, and it is max_items at its default.
//
// Counted as well as weighed, because whoever decodes the inputs pays for their values rather than
// their bytes, a map for three bytes of {}, and the controller decodes them at every decision it
// takes on the run. An envelope is what the documentation lets the controller read, and max_items is
// how many items one carries: inputs held to that many values cost the controller no more than an
// envelope it may already read.
const InputsMaxValues = agk.DefaultMaxItems

// InputsMaxBytes is what a run's inputs may weigh: one envelope, envelope_max_bytes at its default,
// since an input reaches a step as the envelope of a port it feeds.
const InputsMaxBytes = agk.DefaultEnvelopeMaxBytes

// bind binds what was supplied against the declaration of one version, and answers the inputs as
// the run records them.
//
// "Already held to their declared schemas with required and default applied" is what a run's
// inputs are when they are written, whatever started it: bound here and nowhere else, by the same
// graph.Workflow.DeclaredInputs and schema.Bind agk run --local binds them with, so that a run reads
// the same inputs whoever started it.
func (s *Starter) bind(ctx context.Context, namespace, workflow, commit string, g *graph.Graph, supplied map[string]any) (json.RawMessage, error) {
	key := namespace + "/" + workflow + "@" + commit
	declared, held, done := s.declared.claim(ctx, key)
	if !held && done == nil {
		return nil, fmt.Errorf("the version's declaration was being compiled when the request ended: %w", ctx.Err())
	}
	if !held {
		compiled := false
		defer func() { done(declared, compiled) }()
		tree := &versionTree{ctx: ctx, pool: s.pool, objects: s.objects, namespace: namespace, workflow: workflow, commit: commit}
		var err error
		declared, err = g.Workflow().DeclaredInputs(tree)
		switch {
		case errors.Is(tree.trouble, ErrNoObjectStore):
			return nil, ErrNoObjectStore
		case tree.trouble != nil:
			return nil, fmt.Errorf("the tree of %s/%s@%s could not be read to bind a run's inputs: %w", namespace, workflow, commit, tree.trouble)
		case err != nil:
			return nil, fmt.Errorf("%w: %v", ErrDeclarationRefused, err)
		}
		compiled = true
	}

	bound, err := schema.Bind(declared, supplied)
	if err != nil {
		return nil, err
	}
	// Held to the bounds the inputs sent were held to, since the defaults the workflow declares are
	// read by the controller at every decision it takes on the run exactly as they are.
	if n := Values(bound); n > InputsMaxValues {
		return nil, &InputsTooLarge{Why: fmt.Sprintf("the inputs, with the defaults the workflow declares, hold %d values, and a run's inputs hold at most %d, as many as one envelope may carry items: a run's data belongs in an artifact", n, InputsMaxValues)}
	}
	// Without HTML escaping, which would write a < sent as one byte in six.
	var encoded bytes.Buffer
	e := json.NewEncoder(&encoded)
	e.SetEscapeHTML(false)
	if err := e.Encode(bound); err != nil {
		return nil, err
	}
	if int64(encoded.Len()) > InputsMaxBytes {
		return nil, &InputsTooLarge{Why: fmt.Sprintf("the inputs, with the defaults the workflow declares, are written in %d bytes, and a run's inputs weigh at most %d, one envelope: a run's data belongs in an artifact", encoded.Len(), InputsMaxBytes)}
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), nil
}

// Values counts a decoded document as the API's body reader counts one: every object, array,
// string, number, boolean and null, at any depth, and the document itself.
func Values(v any) int {
	n := 1
	switch v := v.(type) {
	case map[string]any:
		for _, e := range v {
			n += Values(e)
		}
	case []any:
		for _, e := range v {
			n += Values(e)
		}
	}
	return n
}
