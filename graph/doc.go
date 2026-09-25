// Package graph is the workflow evaluator: it reads an entry point, resolves the graph
// its edges describe, and says which task should run next and what that task carries.
// It decides; it does not execute.
//
// The name is the one the root doc.go reserved for this work, and it is the only name
// this group claims. The three layers below are one package because the rules do not
// cut apart: the expression roots a step may read depend on its fan_out, the outputs it
// may declare depend on its brick manifest, and the barrier depends on both. A second
// package beside this one would have to export enough internal structure across the
// seam to make the seam pointless.
//
//	Parse, ParseFragment, Load   the entry point, closed, with includes, extends and
//	                             defaults resolved in the order the language fixes
//	Check, Images, Build         the resolved graph, its edges, its cycles and the
//	                             manifest subset rules
//	Start, New, Evaluator        the run, as a state a Plan comes out of and a Result
//	                             goes back into
//
// # What this package refuses to be
//
// It has no loop, no goroutine, no clock, no identifier generator and no Driver call.
// Nothing here opens a socket, pulls an image, speaks to a registry, reads or writes the
// artifact store, or knows that a database exists. Next and Record are functions of the
// state and their arguments and of nothing else, so the same sequence of Results always
// produces the same sequence of Plans, a run replays by replaying its Results, and every
// rule the documentation states is testable with nothing behind it but a map in a test
// file.
//
// Everything from outside arrives as an argument, and each argument is there because a
// documented rule needs it:
//
//	fs.FS                        the repository tree, already pinned to the commit, so
//	                             an include and a schema $ref resolve inside it and can
//	                             never leave it
//	map[WorkflowRef]Fragment     the cross-repository includes, already fetched, because
//	                             resolving one is reaching another repository
//	map[string]brick.Manifest    the manifests, already fetched, because reading
//	                             /agk/brick.yaml means pulling an image, and pulling an
//	                             image is executing
//	now time.Time                handed to Next, so that evaluation is pure and a replay
//	                             is exact rather than nearly exact
//
// Images says which manifests to fetch, so the caller can do the fetching between Check
// and Build without this package ever holding a registry client.
//
// # Where the evaluator stops and the driver begins
//
// The evaluator stops at a decision and the driver begins at a container. A Plan is the
// decision: the tasks that have become ready, the tasks that should be stopped, and the
// moment to ask again. Nothing in a Plan has happened yet, and calling
// Next twice with the same now returns the same Plan.
//
// With one exception, which is a rule of the language and so is decided here rather than by
// whoever reads the Plan. A task stopped as superseded or sibling_failed while the run goes
// on ends cancelled in the pass that names its stop, as the table of stops says it ends, and
// its step is judged then; the second call finds it over and does not name it again. What
// its driver reports afterwards adds how the container exited and nothing else. agk run
// --local and the controller both read that from this package, which is what makes them
// give one shard state for one history rather than one each.
//
// A Task is one shard of one attempt of one step, carrying everything a driver needs and
// nothing it has to look up. Task.Inputs is exactly the argument brick.WriteInputs
// takes and Task.Outputs is exactly the declared argument brick.Collect takes, so the
// driver is the thin thing between two calls that already exist. That is why the merge
// strategies are resolved here and not there: wait_all, zip, join and first are rules
// the workflow file states, and a rule hidden inside the driver is a rule the workflow
// file cannot show.
//
// The interface the driver implements is stated here and called by nothing here:
//
//	type Driver interface {
//	    Run(ctx context.Context, t Task) (Result, error)
//	    Stop(ctx context.Context, s Stop) error
//	}
//
// Stop is a method rather than a cancelled context because a task in flight may be held
// by a runner in another process, where a context does not reach. The loop that reads a
// Plan, hands each Task to a Driver and feeds each Result back belongs to cmd/agk and to
// the controller; putting it here would be the evaluator executing.
//
// The task message of the documentation carries digests rather than envelopes. That is
// one serialisation of a Task, made by a server driver that spills the envelope to the
// store and puts the digest on the bus, and it is not a second decision.
//
// # The run, as a value
//
// State is plain data with a Version field and a JSON round trip, holding an agk.Run as
// its head and restating nothing that sits there. The controller says failover is a
// state resume and never a rebuild, so the state has to be something a database can hold
// and a second process can pick up: load the State, call Next, get the Plan the dead
// instance would have got. Evaluator is a handle over a Graph and a State and holds no
// progress of its own.
//
// # Reading a workflow file
//
// The entry point is read by hand, with closed decoding, rather than through a JSON
// Schema evaluator, on the precedent agk already set for the envelope and for the same
// reason: a refusal has to name the step, the port and the rule in the documentation's
// own words, and a JSON Schema error names a keyword and a JSON Pointer. The entry point
// and the brick manifest are the engine's own shapes; package schema stays what its own
// doc says it is, JSON Schema for the schemas a user writes, and is used here for
// exactly that, workflow inputs and brick params and port schemas.
//
// The claim that this reader agrees with the released workflow.schema.json is not made
// in the code. It is made by corpus_test.go, which runs the vendored fixture corpus and
// holds each invalid document to the rule its index entry names. The index marks every
// invalid workflow fixture refused_by "schema" or "validator"; everything marked schema
// is shape, which closed decoding gives for nothing, and the fifteen marked validator
// are, item for item, the rules this package owns. Rule values are spelled as the corpus
// spells them, so that holding a fixture to its rule is one lookup and not a translation
// table somebody has to keep in step.
//
// The file is read with a YAML 1.2 parser. That is load bearing rather than a
// preference: a 1.1 parser reads the bare key on: as the boolean true and fails every
// document carrying a trigger.
//
// # Where the documentation is silent
//
// Fifteen readings are taken here, each recorded beside the rule that applies it rather
// than only in this list, because otherwise whoever writes the driver, or the next reader
// of a workflow file, settles them again and differently.
//
// Exit 125 and above is an infrastructure failure, charged to the runner and not to the
// brick, and retry.on has no name for it: its four names are transient, failed, lost and
// timeout. So it is its own band and cannot be retried by name. Folding it into
// transient would retry what the documentation charges to the runner.
//
// A join publishes the unmatched envelope on the step's own unmatched output port,
// without the container ever touching it. The engine writes a port the brick never
// writes, which is why a step doing a join declares it in outputs.
//
// A step whose if is false publishes empty envelopes with count 0 and no artifact, so
// materialising an empty port asks nothing of the store.
//
// A failure without continue_on_error fixes the run verdict at failed, and evaluation
// continues. Otherwise when: [always] on a cleanup step never runs and the keyword means
// nothing, and the run states list three causes of cancelled, of which a step failure is
// not one.
//
// Precedence among timed_out, cancelled and failed when more than one is true.
//
// Whether max_parallel counts a retry attempt, and whether it is read per step or per
// step per attempt.
//
// Whether a requeue after a loss waits out the backoff. It does not: a backoff spaces
// attempts so that a dependency which is briefly unwell is not hammered, and a requeue is
// the same attempt handed out again because a host went quiet.
//
// Whether the ending of a dispatch the shard was requeued past ends the attempt. It does
// not: that dispatch was judged lost and handed out again, and the requeue, still running
// and owed an ending of its own, is what the attempt waits on.
//
// Which position a step's inputs keyword is read in. It feeds a port, and a port is fed
// before the step is divided into shards, so it reads what a step reads and not what a
// shard does: no item, no matrix, and no secret, since the table exposes secrets to
// params and to secrets alone.
//
// What a fan-out that has not arrived yet does to the expressions beside it. A step whose
// strategy is written in a block an included file carries has no fan_out in hand between
// Parse and Load, so its params are compiled in the widest position and the rule that
// item is read only under fan_out: item is left to Load, which has every block.
//
// What a port the inputs keyword feeds carries. A list is a batch of items and an object
// is a batch of one, which is what a workflow input declared with an array schema and one
// declared with an object schema each mean on a port; a scalar is refused, because
// wrapping one would invent the key it sits under.
//
// The identity of an item on such a port. It is derived from the step, the port and the
// rank rather than minted, because the evaluator has no identifier generator and because
// a minted one would move the cache key of every step below it on every evaluation.
//
// run.attempt is 1. A replay is a new run and nothing moves out of a terminal run state,
// so a run has one attempt; the attempt a container is on is the task's, which
// AGK_ATTEMPT carries.
//
// What fail_fast does to the shards nobody has handed out yet. They never start: they end
// cancelled with the ones in flight, a retry waiting out its backoff included, which is the
// staged rollout the documentation describes, where fail_fast "stops it at the first broken
// region". Its paragraph on fan-out names only "the shards still running". A step a merge:
// first cancelled ends its own the same way, since a cancelled step hands nothing out and a
// shard left pending in it would read as work still to come.
//
// What becomes of a step that broke a rule of the language after the run had started: a
// zip on envelopes of differing lengths, a batch too large to travel, a condition that
// does not evaluate, a parameter the manifest refuses once it is resolved. The step
// failed, which the run states already have an answer for, rather than the evaluation
// stopping: a run whose Next returned an error would stay at running with nothing to
// record, and the states have no word for that.
//
// # What is deliberately not claimed here
//
// Replay from a chosen step and the cache lookup are not in this surface. CacheKey is
// computed, because the evaluator already holds the image digest, the resolved params
// and the input envelope digests that make it; it is never looked up, because a cache
// hit that resolves to an expired or one-shot artifact is not a hit and only the store
// knows that. Expiry is the store's knowledge and the replay rule rests on it, so
// ReplayFrom belongs to the group that has a store in reach.
package graph
