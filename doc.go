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
// Dependencies run one way and there is no cycle: artifact and schema import agk, brick
// imports agk and artifact, graph imports agk, brick, schema and internal/expr, and agk
// imports nothing outside this module but internal/ulid, which is itself the standard
// library. internal/expr is where cel-go is isolated, on the precedent schema set for
// JSON Schema: only the evaluator needs a CEL parser, so only the evaluator pays for one.
//
// Reserved for the groups that follow, so that nothing claims these names early: driver
// for the container driver and cmd/agk for the command line.
package agentiik
