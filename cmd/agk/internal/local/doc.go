// Package local is one local run: the six merged packages wired together, plus the loop
// the evaluator deliberately does not have.
//
// graph/doc.go places that loop in as many words: "The loop that reads a Plan, hands each
// Task to a Driver and feeds each Result back belongs to cmd/agk and to the controller;
// putting it here would be the evaluator executing." It lives under cmd/agk/internal
// because the root doc.go reserved cmd/agk for this group and nothing else, and because
// writing an exported package for a second consumer two milestones away, whose shape
// nobody can see, is speculation. When the controller wants it in v0.3.0 it moves up,
// which is an addition rather than a change.
//
// # What this package refuses to be
//
// It decides nothing about what runs next and collects nothing. Which task runs, how many
// shards there are, what a port carries, when a step is ready, what a merge means, which
// exit code means what and what gets collected are all read out of a Plan or out of a
// Result, and every one of them has an owner already. The test for that is the import
// list: graph for what runs next, driver for containers, artifact for where bytes go,
// schema for nothing at all, and brick for one type and no function, because driver is
// between brick.WriteInputs and brick.Collect and this is above driver. The one type is
// brick.Manifest, which travels from the driver that read it out of an image to the
// graph.Build that holds each step to it, and this is what carries it between the two:
// naming it is unavoidable, and boundary_test.go holds the allowance to that one name. If
// this package ever grows a decision about readiness, shards, merges or collection, the
// decision has been written in the wrong package.
//
// It also holds no terminal. Everything it has to say leaves through Request.Events as a
// value, delivered from the one goroutine that calls Next and Record, so the screen shows
// the run in the order the evaluator saw it and the narrator needs no lock. What the
// screen does with it is cmd/agk's.
//
// # The five rules of the loop
//
// One: ask. plan, err := ev.Next(now()). An error from Next is a broken state and not a
// failed run, so it ends the process rather than the run: the run states have no word for
// a run whose Next returns an error, which graph/doc.go already says of a step that broke
// a rule of the language.
//
// Two: record the dispatch before starting anything. For each Task taken off plan.Start,
// Record a Result carrying agk.TaskDispatched, and only then start the goroutine that
// calls driver.Run. This is the one rule the loop must not get wrong. A planned shard
// stays agk.TaskPending until a Result moves it, and the evaluator plans every pending
// shard on every pass, so a loop that started a container without recording the dispatch
// would start the same container again on the next Next. graph.Result's own comment is the
// authority rather than a discovery: recording the dispatch is what fixes the task's
// deadline, because the deadline runs from the moment the work became somebody's.
//
// Three: the loop starts exactly what the Plan says. There is no ceiling on this side.
// max_parallel is the author's lever, stated in the file, and a second scheduler here
// would make a local run answer differently from a server run on the same file.
//
// Four: stops and the clock. Every graph.Stop a Plan names goes to driver.Stop in the
// order the Plan named it, every time it appears, because Stop is idempotent by design and
// a stop for a task that already finished is the ordinary consequence of at-least-once
// delivery. A stop sent for a task this loop started is sent again on every pass until the
// task comes back, because the evaluator names a stop sent while the run goes on, as
// superseded or sibling_failed, once: it ends that task cancelled in the pass that names the
// stop, exactly as it does for a server, and what the driver reports of it afterwards adds
// its exit code and nothing else. Sent once, it could miss a container the driver has not
// reached yet, which it answers with nil. Then the loop blocks on one of three things: a Result
// coming back, the timer at plan.Wake, or the context being cancelled. Wake is a timer and
// not a poll.
//
// Five: ending. The loop ends when the run state is terminal and nothing is outstanding. A
// terminal run still has containers to call off, so terminal alone is not the exit: the
// plans after it are the stops, and the Runs still blocked are the ones that come back
// agk.TaskCancelled or agk.TaskTimedOut and report.
//
// An interrupt calls Evaluator.Cancel, after which Next returns the stop-everything plan,
// driver.Stop sends the daemon's own stop with the policy grace, and the run reports
// cancelled. The Results still owed are drained before anything is reported, because a
// report written while a container is still exiting is a report that disagrees with the
// state beside it.
//
// The loop wants nothing from the evaluator that the evaluator does not expose: Next,
// Record, Cancel, State and Outputs are the whole of it.
//
// # The reading the loop has to take
//
// What to do with a driver that returns an error rather than a Result. The driver
// deliberately invents no exit code for a failure that produced no container, because "a
// driver reporting its own trouble as a brick failure fails somebody else's step". But the
// evaluator accepts only a Result, and a shard left dispatched hangs the run, so somebody
// has to answer and it is this package.
//
// driver.Charged(err) decides. A platform charge is recorded as agk.TaskFailed with exit
// 125, which the exit-code table charges to the runner and which agk.Band already makes
// unretryable and nameless to retry.on. A brick charge with no container is recorded as
// exit 120, invalid input, which is the code the evaluator itself already uses for a task
// it could not build and which is a permanent failure never retried whatever retry says.
// The state is agk.TaskFailed and never agk.TaskLost, because lost is the heartbeat's word
// for a runner that stopped reporting and means the work may well have finished, which is
// false for a pull that died before a container existed.
//
// One of these errors has a code of its own: a container that exited 0 and left outputs the
// driver refused, which driver.ErrOutputsRefused marks. The exit code table reserves 121,
// driver.ExitContractBroken, for it, and that is the code recorded. Outputs that passed
// and could not be written are the platform's, like any other platform charge.
//
// Otherwise the code does not reach the report. The Failure this package hands back
// carries HasExit false and the driver's own Fault sentence, so the report reads "no exit
// code" and names what was refused, while the evaluator still gets a band it can act on.
// The code is for the evaluator and the sentence is for the person. A refused output is
// reported with its 121 beside the sentence, since a container did exit.
//
// # The working directory
//
// This is the only package that knows the layout, and it is per run under one shared
// object store so that two runs do not collide and content addressing shows.
//
//	.agk/objects/                    the artifact.Dir root, shared by every run
//	.agk/bin/agk-linux-<arch>        the static helper, 0555, laid down once
//	.agk/runs/<run>/run.json         the local run record, carrying local: true
//	.agk/runs/<run>/state.json       graph.State, written after every Record
//	.agk/runs/<run>/logs/<step>/<attempt>[-<shard>].log
//	.agk/runs/<run>/outputs/<name>.json
//
// One directory is deliberately not in that list, and Layout.WorkRoot says why at length:
// the driver's work root, one directory per task, created fresh and removed with its
// container, sits outside the tree. Everything under .agk is readable by every container of
// the run, because .agk defaults to sitting inside the tree that is bound read-only at
// /agk/repo, and a task's working directory is where the driver writes that task's secret
// values on a platform with no tmpfs. Under .agk a step that declares no secret could read
// one another step was given.
//
// The work root is not per run, and that is the driver's own doing rather than a choice
// taken here: the driver names a task's directory run/step/attempt beneath its work root,
// so the run is already the first segment of every path below it and a work root per run
// would spell the run twice. Layout.Work(run) is that directory.
//
// state.json is written after every Record and not only at the end, because the state is a
// value and writing it costs nothing, so a run that dies at three in the morning leaves
// the thing a second process would resume from. Resuming is not claimed at v0.1.0.
//
// One wart, said out loud rather than hidden: .agk defaults to sitting inside the tree
// that is bound read-only at /agk/repo, so a container can see previous runs' artifacts.
// On a server the tree is a commit and has no such directory. The alternative is hiding a
// run's output in a cache directory somewhere, and at three in the morning
// discoverability is worth more than tidiness. A --dir flag moves it for anyone who minds.
// What the wart covers is records: envelopes, state and logs, of one namespace and the
// namespace's own run. It does not cover a secret value, which is why the work root is the
// one directory that sits outside the tree.
//
// # What the driver is given
//
// driver.Config is built here exactly as a server runner would build it, from the layout
// rather than from a control plane, which is the property that makes this the same code
// path and not a second one.
//
//	Store     artifact.New(artifact.Dir(.agk/objects), namespace, limits), so physical
//	          keys are <namespace>/sha256/<digest> and two runs share identical bytes
//	Repo      the working tree, whole, bound read-only at /agk/repo. Narrowing it by
//	          files is an optimisation and never a requirement, and the long form's to
//	          relocation still applies inside the driver
//	Runs      the one run this session holds
//	Secrets   the command line, so the driver writes each value to a file and binds it
//	          read-only at /agk/secrets/<name>, masks it out of the log and out of the
//	          payload, and nothing reaches an environment variable
//	Logs      one file per task
//	Observer  the dispatched, running and publishing transitions, onto the loop's queue
//	Policy    a value built here. driver.DefaultPolicy with the userns and seccomp floors
//	          lifted and Helper set, and /etc/agentiik/runner.toml is never read
//	WorkRoot  Layout.WorkRoot, which is outside the tree bound at /agk/repo and is the
//	          one path of the layout that is
//
// All of it is built once, when the daemon handle is made, because that is when driver.New
// takes it. Two of those are therefore the session's and not one request's: the layout, which
// the work root is named out of, and the size limits. That is why Open takes the layout and
// Daemon carries the limits, and it is also why a Session holds one run at a time: a second
// run with a layout of its own would need a second handle, and a second handle is a second
// manifest cache, which is the thing this type exists to avoid.
//
// The floor is lifted by default and the driver's own Announce sentences are printed once,
// naming what this machine gives up: the floor as the daemon is opened, and where a secret
// lands at the first step that was given one. That is the case the lift exists for:
// driver/doc.go says so, because Docker Desktop does not offer the remapping and agk run
// --local has to work on a laptop, and a lift that needed a flag on every invocation would
// not make it work. Nothing lifts the floor and says nothing, which is the same shape as
// nothing opens the network and calls it filtered. A flag holds the floor for anyone
// running against a Linux daemon that has the remapping.
//
// # The order of one local run
//
// Every step is a call into a merged package and this package adds nothing to any of them.
//
//	1  graph.Load(os.DirFS(tree), entry, nil), then graph.Check. The nil remote map
//	   refuses a workflow: include naming the repository and the ref it wanted, because
//	   resolving one is reaching another repository and there is no server here.
//	2  graph.Workflow.DeclaredInputs(os.DirFS(tree)), then schema.Bind, as the API binds a
//	   server run's. A $ref resolves inside the tree and is refused when it leaves it, and
//	   required and default are applied before a run exists, which is what
//	   graph.Options.Inputs expects.
//	3  the secrets check, before a daemon is touched.
//	4  Open: dial, negotiate, read the floor, say what is given up.
//	5  Resolve: graph.Images, one driver.Manifest per image, graph.Build. Build holds
//	   every step to its manifest, and the manifests stay in the driver's cache keyed by
//	   image digest, so the manifest that warmed the run is the one agk validate read.
//	6  agk.NewRunID, then agk.Run with Commit empty, Trigger agk.TriggerManual and
//	   TriggeredBy local. The empty commit is load bearing twice: the driver's environment
//	   table drops a variable with nothing to carry, so AGK_COMMIT is absent rather than
//	   empty, and a run with no commit is a run nobody can mistake for a version of a
//	   repository. TriggeredBy is the label, the same word as the flag, and it travels
//	   into /agk/run.json and into run.json on disk.
//	7  driver.New, then graph.Start, then the loop.
//	8  Evaluator.Outputs on a terminal run, one envelope per name the outputs block
//	   declares. A failed run is not asked for outputs: an output is a view of a step
//	   port and that step did not end, and the envelopes that were published are in
//	   state.json.
//
// Everything below the loop is already merged and this package calls none of it directly.
// Inside driver.Run: resolveImage, brick.ParseManifest cached by digest, brick.WriteInputs,
// the mounts and the AGK_ environment, create, the wait opened before the start, attach,
// start, brick.Collect, Store.Put per file, brick.Spill, agk.Band, graph.Result. If any of
// that appeared here it would be in the wrong place.
//
// # Layout
//
//	session.go   Daemon, Open, Resolve, Close: one daemon handle, one manifest cache,
//	             the announcements said once
//	run.go       Request, Outcome, and the loop
//	layout.go    Layout and the paths above
//	store.go     artifact.Dir and artifact.New for the workflow's namespace
//	secrets.go   driver.Secrets over what the command line supplied
//	logs.go      driver.Logs as a file per task
//	observe.go   driver.Observer onto the loop's queue
//	event.go     Event, what a narrator needs and nothing more
//	narrate.go   the Events, said by diffing the state rather than by guessing at it, so
//	             that a verdict Next settled and a transition Record took read alike
//	failure.go   Failure, read out of the written state so that the report of a run and
//	             the state beside it cannot disagree
package local
