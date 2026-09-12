// Package agentiik is the root of the Agentiik core: the graph evaluator, the container
// driver, the controller, the HTTP API, the runner and the agk command line, as one Go
// module. The root package itself holds no code. What the module is made of is the
// packages under it, and this file says what they are for and what they are not.
//
// The specification the code is written against is at https://agentiik.github.io/docs.
// Where that document states a rule, the code states it in the same words; where it is
// silent, the code takes the reading that cannot contradict it and records the reading
// beside the rule, in a doc comment on the function that applies it.
//
// # What this module refuses to be
//
// The evaluator and the driver are libraries, importable with no controller, no task
// bus and no database behind them. That is an architectural constraint rather than a
// preference: it is what lets agk run --local be the same code path a server run takes,
// instead of a second implementation that drifts from the first. A rule hidden inside
// the controller is a rule the workflow file cannot show, so no execution rule lives
// there.
//
// Nothing here redefines a shape that agentiik/schemas owns. The workflow file, the
// brick manifest and the envelope are described by JSON Schema documents released from
// that repository and pinned by version; this module consumes them and pins the fixture
// corpus that pins them. When those shapes disagree with this code, the schemas are
// right.
//
// No language model lives in the engine, and no agent. Agentiik speaks MCP as a server
// and never as a client; whatever model is involved sits on the far side of that
// protocol boundary.
//
// # Layout
//
// At v0.1.0, the group that settles what travels on a port:
//
//	agk       The vocabulary and every rule stated about it: Envelope, Meta, Item, File,
//	          Port, Step, RunID, the agk:// URI, the four size limits and the two
//	          outcomes they produce. Standard library only, held by a test, so that
//	          importing the core types costs nothing and the choice of a JSON Schema
//	          validator or an object store never reaches an importer.
//	artifact  The content-addressed store. A Store is opened for one namespace and
//	          builds every physical key as <namespace>/sha256/<digest>, so two
//	          namespaces cannot share an object and forgetting the namespace is not
//	          expressible.
//	brick     The two edges of the container, as directories: what a step is given
//	          under /agk/in/<port>/, and what is collected from /agk/out/. It knows the
//	          contract and nothing about Docker, so agk brick test can run a brick
//	          against sample envelopes with no driver in reach.
//	schema    JSON Schema 2020-12 for the schemas a user writes, and the boundary rule
//	          that is not JSON Schema's: required and default on a workflow input. The
//	          only package where a third party JSON Schema implementation appears.
//
// Then the group that decides what runs next, which is the one name it claims:
//
//	graph     The evaluator. It reads the entry point, resolves the graph its edges
//	          describe, and hands out the task that should run next and takes back what
//	          happened to it. It decides and never executes: no loop, no goroutine, no
//	          clock, no socket and no Driver call, so the same sequence of results always
//	          produces the same sequence of plans and a run replays exactly.
//
// The run itself is named in agk, where the driver and the controller can see it without
// importing the evaluator: the run states, the step verdict, the task states, the four
// kinds retry.on names and the exit-code table. The identifier Run was held in reserve
// for this group, and this is the group that takes it up; a run no longer travels as
// agk.RunID alone.
//
// Then the group that runs what the evaluator decided, and what a brick is given:
//
//	driver    The container driver. It fills graph.Driver and is the only package in
//	          this module that may reach a Docker daemon. One container per task
//	          through the Engine API: resolve and pull by digest, read and cache
//	          /agk/brick.yaml, prepare the mounts and the environment the contract
//	          promises, apply the settings every container gets, give each task a
//	          network of its own, hold the user namespace floor, enforce the step
//	          timeout, read the exit code off the table, and collect what the
//	          container produced with the secret values masked before anything is
//	          written. It is the thin thing between brick.WriteInputs and
//	          brick.Collect, and it decides nothing about what runs next.
//
// Two decisions inside it are the operator's business rather than an author's. User
// namespace remapping is a floor: a daemon without it is refused, and only
// require_userns_remap = false in /etc/agentiik/runner.toml gets past the refusal, after
// which the driver says once what the machine gives up. Network egress is refused
// outright, because the proxy that would enforce an egress.allow list does not exist yet
// and a workflow must not be able to believe its list is being enforced when nothing is
// enforcing it. network: none and network: internal run.
//
// Dependencies run one way and there is no cycle: artifact and schema import agk, brick
// imports agk and artifact, graph imports agk, brick, schema and internal/expr, driver
// imports agk, artifact, brick, graph and internal/docker, and agk imports nothing
// outside this module but internal/ulid, which is itself the standard library.
// internal/expr is where cel-go is isolated, on the precedent schema set for JSON Schema:
// only the evaluator needs a CEL parser, so only the evaluator pays for one.
// internal/docker is the Engine API over the unix socket, on that same precedent and for
// that same reason: only the driver needs a daemon, it is written against the standard
// library alone so the module takes no dependency for it, and keeping it in a package of
// its own is what lets the driver's own files be about mounts, environments, signals and
// bytes rather than about HTTP. internal/dockertest is a fake daemon on a temporary
// socket, which is how every one of those rules is tested with no Docker in reach.
//
// The arrow between the evaluator and the driver points one way, and it is checked rather
// than asserted. graph states the Driver interface and calls neither of its methods;
// driver implements it and imports graph; graph imports nothing of driver.
// graph/boundary_test.go reads the evaluator's whole import closure and fails it on a
// database, an HTTP client, a socket, a process started outside this one, a task bus
// client, a registry client, a container runtime, or the path segment driver. The
// evaluator reaches no daemon, and that is a test rather than a preference.
//
// Reserved for the group that follows, so that nothing claims the name early: cmd/agk for
// the command line.
package agentiik
