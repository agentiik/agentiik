# agentiik

The core of the Agentiik workflow engine, as one Go module. The graph evaluator, the
container driver, the controller, the HTTP API, the runner and the `agk` command line all
belong here, and they belong in one module because the evaluator and the driver have to
stay importable libraries, with no server, no task bus and no database behind them. That
is an architectural constraint rather than a preference: it is what makes
`agk run --local` the same code path a server run takes, instead of a second
implementation that drifts from the first.

What is written so far is the group that settles what travels on a port, and the group
that decides what runs next. The rest of the list is named here because that is where it
is going, not because it is there.

What the code is written against is the specification at
<https://agentiik.github.io/docs>, and that is also where the documentation lives; this
README is the only one this repository keeps. The workflow file, the brick manifest and
the envelope are shapes [`agentiik/schemas`](https://github.com/agentiik/schemas) owns,
pinned by version; nothing here redefines them, and where they disagree with this code
the schemas are right.

## What is in the module

v0.1.0 is the group that settles what travels on a port and the evaluator above it. Five
packages, and `doc.go` at the root says what each one is for and what it is not.

- **`agk`** is the vocabulary the documentation uses and every rule it states about it:
  `Envelope`, `Meta`, `Item`, `File`, `Port`, `Step`, `RunID`, the `agk://` URI, the four
  size limits and the two outcomes they produce. An envelope is read and written here,
  validated by hand against the shape the schema fixes, with `meta` carrying `run_id`,
  `step`, `port`, `attempt`, `count` and `produced_at`. A value above `inline_max_bytes`
  left inline rejects the envelope; an envelope over `envelope_max_bytes` fails the
  emitting step. Those are two different outcomes, told apart with `errors.Is` against
  `ErrEnvelopeRejected` and `ErrStepFailed`, and the difference is the whole reason the
  package carries an `Outcome`. `Empty` is what a declared port publishes when nothing
  was written to it, and `Split` and `Concat` carry an item's identifier unchanged
  through a fan-out and the merge that follows. The run itself is named here too, where
  the driver and the controller reach it without importing the evaluator: `Run` with its
  states, `Verdict`, the task states, the four kinds `retry.on` names, and `Band`, which
  is the exit-code table read off the code a container exited with.
- **`artifact`** is the content-addressed store. The logical URI
  `agk://run/<run>/<step>/<port>/<name>` resolves to a physical key `sha256/<digest>`,
  identical bytes are stored once, and deduplication is scoped per namespace: a `Store`
  is opened for one namespace and builds every key as `<namespace>/sha256/<digest>`, so
  two namespaces never share a physical object and forgetting the namespace is not
  expressible. `Objects` is the byte layer underneath, three methods wide, with `Dir` as
  the local directory an `agk run --local` uses.
- **`brick`** is the two edges of the container, as directories. `WriteInputs`
  materialises what a step is given and returns the mounts a driver binds read-only at
  `/agk/in/<port>/`, so a brick opens a path and never a store URL. `Collect` reads back
  one envelope per declared output port from `/agk/out/ports/`, publishing an empty one
  for a port the container never wrote and treating that as success. `Spill` is the other
  half of the inline threshold: it moves a value the engine itself built above
  `inline_max_bytes` into the store and replaces it with a `files[]` entry carrying
  `name`, `uri`, `media_type`, `size` and `sha256`. `ParseManifest` reads
  `/agk/brick.yaml`, which is what a step's declared outputs have to be a subset of.
- **`schema`** is JSON Schema 2020-12 for the schemas a user writes, and the one boundary
  rule that is not JSON Schema's: `required` and `default` on a workflow input. `Bind`
  validates the inputs a trigger supplied against what the workflow declares and refuses
  the run before any step starts. References resolve against the repository tree the run
  pinned and never against the network.
- **`graph`** is the evaluator. `Parse` and `Load` read the entry point and close it,
  resolving includes, `extends` and `defaults` in the order the language fixes, with
  `ParseFragment` for an included file, which is not an entry point and is never read as
  one. `Check` and `Build` resolve the graph the `needs` and `inputs` edges describe,
  refusing an edge onto a port no step declares, rejecting cycles, and holding each
  step's declared outputs to a subset of its brick manifest; `Images` says which
  manifests to fetch, so the caller does the fetching and this package never holds a
  registry client. Then `Start`, `Next` and `Record`: a `Plan` names the tasks that have
  become ready, the tasks in flight that should be stopped, and the moment to ask again,
  and a `Result` goes back in. Inside sit the rules the workflow file states rather than
  the driver: the barrier, where a step runs once every declared input port is satisfied
  and each port publishes exactly one envelope; the four merge strategies `wait_all`,
  `zip`, `join` and `first` with their conflict rules; the four fan-out strategies under
  `max_parallel` and `fail_fast`; `if` and `when`; `retry` and `continue_on_error` taking
  the verdict from the exit-code table; and the run verdict from the step outcomes. A
  refusal is a `Refusal`, naming the step, the port and the rule in the documentation's
  own words.

The evaluator decides and does not execute. It has no loop, no goroutine, no clock, no
identifier generator and no `Driver` call; `Next` and `Record` are functions of the state
and their arguments and of nothing else, so the same sequence of `Result`s always
produces the same sequence of `Plan`s and a run replays by replaying its results. The
`Driver` interface is stated in `graph` and called by nothing in it: the loop that reads
a `Plan`, hands each `Task` to a driver and feeds each `Result` back belongs to the
controller and to `cmd/agk`. `Task.Inputs` is exactly the argument `brick.WriteInputs`
takes and `Task.Outputs` exactly the declared argument `brick.Collect` takes, so the
driver is the thin thing between two calls that already exist. `State` is plain data with
a version and a JSON round trip, because the controller says failover is a state resume
and never a rebuild.

`internal/expr` is the door CEL is behind. It holds the ten roots of the exposed-context
table and the five positions the table's third column distinguishes, builds a compilation
environment from that table and from nothing else, keeps the type of an expression that
fills a whole value and converts an embedded one to text. Item contents stay out of
expressions except under `fan_out: item`, and a secret is a reference and never a value,
exposed to `params` and `secrets` alone. A root the position does not expose is not
declared, so the table is enforced by the compiler rather than by a guard.

Dependencies run one way and there is no cycle: `artifact` and `schema` import `agk`,
`brick` imports `agk` and `artifact`, `graph` imports `agk`, `brick`, `schema` and
`internal/expr`, and `agk` imports nothing outside this module but `internal/ulid`, which
is itself the standard library.

The module takes three direct third party dependencies, each recorded in `go.mod` with
the reason it is taken:

- [`santhosh-tekuri/jsonschema/v6`](https://github.com/santhosh-tekuri/jsonschema), used
  by `schema` and by nothing else. It is the 2020-12 draft itself rather than an older
  one, it takes a custom loader, which is how a `$ref` is resolved against the commit's
  tree and refused when it leaves it, and it returns a structured error whose keyword and
  instance location are what a refusal message names.
- [`goccy/go-yaml`](https://github.com/goccy/go-yaml), used to read the two documents of
  the language: the workflow entry point in `graph` and the brick manifest in `brick`.
  The version of YAML is the reason rather than the API. A YAML 1.1 parser reads the bare
  key `on:` as the boolean true, and `on:` is how the language spells the trigger block,
  so such a parser fails every workflow that carries a trigger.
- [`cel-go`](https://github.com/google/cel-go), used by `internal/expr` alone and reached
  only from `graph`, on the precedent `schema` set for JSON Schema: only the evaluator
  needs a CEL parser, so only the evaluator pays for one. The documentation names the
  language and names the reason, that CEL evaluates in linear time, is mutation free and
  is not Turing-complete, which is what lets a controller serving every namespace
  evaluate a tenant's conditions in its own process.

## What it deliberately is not, yet

There is no container driver, no controller, no HTTP API, no runner and no command line.
The names `driver` and `cmd/agk` are reserved so nothing claims them early. Nothing is
published to `ghcr.io/agentiik/api`, `ghcr.io/agentiik/controller` or
`ghcr.io/agentiik/runner` yet either.

There is no Docker code anywhere. `brick` knows the container contract and nothing about
how a container is started, which is what will let `agk brick test` run a brick against
sample envelopes with no driver in reach, and the evaluator knows it less still: a test
walks the evaluator's whole import closure and fails it on a database, a socket, a task
bus client, a registry client or a container runtime.

Replay from a chosen step and the cache lookup are not in the evaluator's surface.
`CacheKey` is computed, because the evaluator already holds the image digest, the
resolved params and the input envelope digests that make it; it is never looked up,
because a cache hit that resolves to an expired or one-shot artifact is not a hit and
only the store knows that.

`artifact` has no reference counting, no retain and no expiry. The specification puts
expiry on the reference and not on the object: the row binding a run, a step and a port
to a digest is what is dropped, and the object is collected once its reference count
reaches zero. That row is the artifacts table, it arrives with the control plane, and its
shape is not knowable from here.

No language model lives in the engine, and no agent. Agentiik speaks MCP as a server and
never as a client; whatever model is involved sits on the far side of that protocol
boundary. What `graph` holds of MCP is the language rules `mcp.tools` is validated
against.

## Tests

Go 1.27. Tests sit beside the code they cover, and the whole of what CI runs is four
commands:

```
go build ./...
go vet ./...
go test ./... -race
gofmt -l .
```

`gofmt -l .` printing nothing is the passing result.

A test run needs no network and no daemon. The released fixture corpus of
`agentiik/schemas` is vendored and embedded under `internal/fixtures`, pinned at the
version written in `internal/fixtures/testdata/SCHEMAS_VERSION`, so the envelope, the
brick manifest and the workflow file are tested against the documents that pin the shape
to the documentation rather than against table entries this implementation wrote about
itself. The claim that the workflow reader agrees with the released
`workflow.schema.json` is made there and not in the code: every document the corpus
declares valid is read and checked, every document it refuses by its shape is refused,
and the fifteen it marks as refused by a validator are each held to the rule the corpus
names, which is one lookup because a rule is spelled the way the corpus spells it.

## Licence

AGPL-3.0-or-later, see [LICENSE](LICENSE). A brick is not a derivative work of this
engine; [LICENSING.md](https://github.com/agentiik/.github/blob/main/LICENSING.md) says so in as many words, and sets out why the
organisation's repositories are not licensed uniformly.

## Contributing

[CONTRIBUTING.md](https://github.com/agentiik/.github/blob/main/CONTRIBUTING.md). Contributions are accepted under the Developer
Certificate of Origin 1.1, with a `Signed-off-by` line, not under a contributor licence
agreement.
