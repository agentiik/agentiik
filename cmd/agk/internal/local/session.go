package local

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/artifact"
	"github.com/agentiik/agentiik/brick"
	"github.com/agentiik/agentiik/driver"
	"github.com/agentiik/agentiik/graph"
)

// Daemon is what this machine offers one local run, which is the half of a runner's
// configuration a laptop has an answer for.
//
// /etc/agentiik/runner.toml is never read from here. A local run is not a runner,
// driver.Policy is a value this side builds, and two sources for one setting is one too
// many.
type Daemon struct {
	// Socket is the daemon to talk to, and empty means wherever one is: DOCKER_HOST,
	// then the per-user path Docker Desktop uses, then the system path, which is the
	// driver's own resolution and not a second one.
	Socket string

	// Helper is the static agk binary on this host, bound read-only at /agk/bin/agk
	// for a script step. Empty is a session with none to offer, which binds nothing
	// and is what driver.Policy.Helper already documents.
	Helper string

	// RequireUsernsRemap holds the user namespace floor. It is false by default here,
	// which is the one place in this module where the floor is lifted without a file
	// saying so: Docker Desktop does not offer the remapping and agk run --local has
	// to work on a laptop, and a lift that needed a flag on every invocation would not
	// make it work. Nothing lifts it silently, because the driver says once what the
	// machine gives up.
	RequireUsernsRemap bool

	// Limits are the four size rules every envelope this session builds is held to.
	// They sit here rather than on a Request because driver.Config names them once per
	// daemon handle, and one handle per session is what keeps one manifest cache; the
	// zero value is agk's own defaults.
	Limits agk.Limits

	// Announce is where the driver's own sentences about this machine go, said once.
	Announce func(string)
}

// tasks is the half of the driver the loop calls, which is exactly graph.Driver, and
// images is the half Resolve calls. Both are named so that the loop and the resolution are
// testable with no daemon in reach, on the precedent the evaluator already set: a test
// fakes graph.Driver in four lines.
type tasks = graph.Driver

type images interface {
	Manifest(ctx context.Context, step agk.Step, image string) (brick.Manifest, error)
}

// Session is one daemon handle, one manifest cache and the announcements said once.
//
// It holds one run at a time. That is not a limitation imposed for its own sake: the
// driver's configuration names a work root, a repository tree, a log sink and a secret
// source once, so a second run inside one session would need a second daemon handle and a
// second manifest cache, which is the thing this type exists to avoid. A local run is a
// person at a terminal, and that person runs one workflow.
type Session struct {
	layout Layout
	limits agk.Limits

	// tasks and images are the driver, named by the two halves that are used, so that
	// neither the loop nor the resolution can reach the rest of it.
	tasks  tasks
	images images
	close  func() error

	// announce is where a sentence about this machine or about a stop the daemon
	// refused goes, which is the terminal for agk run --local and nothing at all for a
	// test.
	announce func(string)

	// now is the clock, so that a test can hold one still, on the precedent
	// driver.Config.Now already set. Nil is the real one.
	now func() time.Time

	// observations is the driver's own queue onto this loop's goroutine. It is
	// buffered and never blocks: Observe is called on the goroutine running a task and
	// must not block, and narration is not the run.
	observations chan driver.Event

	mu     sync.Mutex
	run    *inflight
	stores map[string]*artifact.Store
}

// inflight is the one run a session is holding, which is what the driver asks about
// through Repo, Runs and Secrets. It exists because those three are answered by a control
// plane on a server and by the command line here, and the command line said it once.
type inflight struct {
	run     agk.Run
	tree    string
	secrets map[string][]byte
}

// Open dials the daemon, negotiates the API version, reads the user namespace floor and
// says once what this machine gives up.
//
// The layout is the session's rather than the request's because driver.Config names the
// work root when the handle is made. Everything else the driver asks for is answered
// through this value, so that the configuration a server runner builds from a control
// plane is built here from a directory and a command line, which is the property that
// makes this the same code path and not a second one.
func Open(ctx context.Context, d Daemon, l Layout) (*Session, error) {
	if l.Root == "" {
		return nil, fmt.Errorf("local: there is no working directory: a session is opened over a layout, which agk run --local roots at %s", DefaultDir)
	}
	limits := d.Limits
	if limits == (agk.Limits{}) {
		limits = agk.DefaultLimits()
	}

	s := &Session{
		layout:       l,
		limits:       limits,
		announce:     d.Announce,
		observations: make(chan driver.Event, observationQueue),
		stores:       map[string]*artifact.Store{},
	}

	policy := driver.DefaultPolicy()
	policy.Helper = d.Helper
	if !d.RequireUsernsRemap {
		policy.RequireUsernsRemap = driver.RemapLifted
	}
	// A local run is not a runner, so a daemon applying no seccomp profile is said out
	// loud rather than refused, as the lifted userns floor is: the laptop is somebody's
	// own, and what it gives up is theirs to hear about.
	policy.RequireSeccomp = driver.SeccompLifted
	// Nor is it held to a tmpfs of the runner's own for its secret values: a laptop has
	// none, and the driver says where a value lands instead.
	policy.RequireSecretsTmpfs = driver.SecretsTmpfsLifted

	dk, err := driver.New(driver.Config{
		Socket:   d.Socket,
		Store:    s.store,
		Repo:     s.repo,
		Runs:     s.runOf,
		Secrets:  secrets{s},
		Logs:     logs{s.layout},
		Observer: observer{s.observations},
		Policy:   policy,
		WorkRoot: l.WorkRoot(),
		Limits:   limits,
		Announce: d.Announce,
	})
	if err != nil {
		return nil, err
	}
	s.tasks, s.images, s.close = dk, dk, dk.Close
	return s, nil
}

// Close releases the daemon handle. It stops nothing: a container outlives the process
// that started it, which is the property adoption depends on, and a run that was
// interrupted has already had its containers stopped through the plan.
//
// It does empty the work root, which is the gap between the task directory the driver removes
// and the run and step directories above it that this package named. Layout.pruneWork is the
// rule, and the closing of the session is the moment because that is the end of a local run:
// every container has been accounted for by then, so nothing under the work root is still
// somebody's. The work root itself stays, for the reason pruneWork gives.
func (s *Session) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	err := s.close()
	s.layout.pruneWork()
	return err
}

// Resolve reads the manifest of every image the workflow names and builds the graph.
//
// graph.Images says which manifests to fetch, the driver is the only thing in this module
// that may reach a daemon, and graph.Build holds every step to the manifest of the brick it
// runs: outputs a subset of the manifest's ports, params against the manifest's schemas.
// The manifests stay in the driver's cache keyed by image digest, so a run that follows
// reads no manifest twice and the manifest that warmed the cache is the one agk validate
// read.
func (s *Session) Resolve(ctx context.Context, wf *graph.Workflow) (*graph.Graph, error) {
	if wf == nil {
		return nil, fmt.Errorf("local: there is no workflow to resolve")
	}
	if err := graph.Check(wf); err != nil {
		return nil, err
	}
	refs := graph.Images(wf)
	manifests := make(map[string]brick.Manifest, len(refs))
	for _, image := range refs {
		// The step is an argument because a driver refusal always names one, and
		// the step named is the first that runs the image, which is the step whose
		// line in the file the person has to go and read.
		m, err := s.images.Manifest(ctx, stepOf(wf, image), image)
		if err != nil {
			return nil, err
		}
		manifests[image] = m
	}
	return graph.Build(wf, manifests)
}

// stepOf names a step that runs one image. The steps are taken in sorted order so that two
// runs over one file refuse the same image in the same words.
func stepOf(wf *graph.Workflow, image string) agk.Step {
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		st := wf.Steps[name]
		if st.Image == image && len(st.Script) == 0 {
			return name
		}
	}
	return ""
}

// hold takes the run this session is about to evaluate, and answers with what to call when
// it is over. A second run inside one session is refused rather than queued, because the
// driver was configured for the first.
func (s *Session) hold(f *inflight) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != nil {
		return nil, fmt.Errorf("local: run %s is still in flight: a session holds one run at a time, because the driver's work root, repository tree and log sink are named once when the daemon handle is made", s.run.run.ID)
	}
	s.run = f
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.run = nil
	}, nil
}

// current is the run this session is holding, or nothing.
func (s *Session) current() *inflight {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

// repo answers with the working tree, whole, which is what gets bound read-only at
// /agk/repo.
//
// Narrowing it by the files a step declares is an optimisation and never a requirement,
// and the long form's to relocation still applies inside the driver. A server runner lays the
// commit's tree out from what its task's grant redeems for; this has the tree the person is
// standing in.
func (s *Session) repo(ctx context.Context, namespace, workflow, commit string) (string, error) {
	f := s.current()
	if f == nil {
		return "", fmt.Errorf("local: the repository tree of %s was asked for and no run is in flight", workflow)
	}
	return f.tree, nil
}

// runOf answers with the one run this session holds, which is what /agk/run.json carries
// into every container.
func (s *Session) runOf(ctx context.Context, id agk.RunID) (agk.Run, error) {
	f := s.current()
	if f == nil {
		return agk.Run{}, fmt.Errorf("local: run %s was asked for and no run is in flight", id)
	}
	if f.run.ID != id {
		return agk.Run{}, fmt.Errorf("local: run %s was asked for and this session is holding run %s", id, f.run.ID)
	}
	return f.run, nil
}
