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
// Dependencies run one way and there is no cycle: artifact and schema import agk, brick
// imports agk and artifact, and agk imports nothing outside this module but
// internal/ulid, which is itself the standard library.
//
// Reserved for the groups that follow, so that nothing claims these names early: graph
// for the evaluator, driver for the container driver, cmd/agk for the command line, and
// the identifier Run in package agk for the run itself, with its states and its verdict.
// Until then a run travels as agk.RunID, which is the name the envelope schema gives
// the field.
package agentiik
