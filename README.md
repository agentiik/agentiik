# agentiik

The core of the Agentiik workflow engine, as one Go module. The graph evaluator, the
container driver, the controller, the HTTP API, the runner and the `agk` command line all
belong here, and they belong in one module because the evaluator and the driver have to
stay importable libraries, with no server, no task bus and no database behind them. That
is an architectural constraint rather than a preference: it is what makes
`agk run --local` the same code path a server run takes, instead of a second
implementation that drifts from the first.

What is written so far is the group below the evaluator, the one that settles what
travels on a port. The rest of the list is named here because that is where it is going,
not because it is there.

What the code is written against is the specification at
<https://agentiik.github.io/docs>, and that is also where the documentation lives; this
README is the only one this repository keeps. The workflow file, the brick manifest and
the envelope are shapes [`agentiik/schemas`](https://github.com/agentiik/schemas) owns,
pinned by version; nothing here redefines them, and where they disagree with this code
the schemas are right.

## What is in the module

v0.1.0 is the group that settles what travels on a port. Four packages, and `doc.go` at
the root says what each one is for and what it is not.

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
  through a fan-out and the merge that follows.
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
  `name`, `uri`, `media_type`, `size` and `sha256`.
- **`schema`** is JSON Schema 2020-12 for the schemas a user writes, and the one boundary
  rule that is not JSON Schema's: `required` and `default` on a workflow input. `Bind`
  validates the inputs a trigger supplied against what the workflow declares and refuses
  the run before any step starts. References resolve against the repository tree the run
  pinned and never against the network.

Dependencies run one way and there is no cycle: `artifact` and `schema` import `agk`,
`brick` imports `agk` and `artifact`, and `agk` imports the standard library alone, held
there by a test so that importing the core types never pulls in a schema compiler or an
object store.

The module takes one direct third party dependency,
[`santhosh-tekuri/jsonschema/v6`](https://github.com/santhosh-tekuri/jsonschema), used by
`schema` and by nothing else. It is the 2020-12 draft itself rather than an older one, it
takes a custom loader, which is how a `$ref` is resolved against the commit's tree and
refused when it leaves it, and it returns a structured error whose keyword and instance
location are what a refusal message names. It brings `golang.org/x/text` with it, for the
localised message its errors are printed through, and nothing else is linked in.

## What it deliberately is not, yet

There is no graph evaluator, no container driver, no controller, no HTTP API, no runner
and no command line. The names `graph`, `driver` and `cmd/agk` are reserved so nothing
claims them early, as is the identifier `Run` in package `agk` for the run itself, with
its states and its verdict; until then a run travels as `agk.RunID`, which is the name
the envelope schema gives the field. Nothing is published to `ghcr.io/agentiik/api`,
`ghcr.io/agentiik/controller` or `ghcr.io/agentiik/runner` yet either.

There is no Docker code anywhere. `brick` knows the container contract and nothing about
how a container is started, which is what will let `agk brick test` run a brick against
sample envelopes with no driver in reach.

`artifact` has no reference counting, no retain and no expiry. The specification puts
expiry on the reference and not on the object: the row binding a run, a step and a port
to a digest is what is dropped, and the object is collected once its reference count
reaches zero. That row is the artifacts table, it arrives with the control plane, and its
shape is not knowable from here.

No language model lives in the engine, and no agent. Agentiik speaks MCP as a server and
never as a client; whatever model is involved sits on the far side of that protocol
boundary.

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
version written in `internal/fixtures/testdata/SCHEMAS_VERSION`, so the envelope is
tested against the documents that pin the shape to the documentation rather than against
table entries this implementation wrote about itself.

## Licence

AGPL-3.0-or-later, see [LICENSE](LICENSE). A brick is not a derivative work of this
engine; [LICENSING.md](https://github.com/agentiik/.github/blob/main/LICENSING.md) says so in as many words, and sets out why the
organisation's repositories are not licensed uniformly.

## Contributing

[CONTRIBUTING.md](https://github.com/agentiik/.github/blob/main/CONTRIBUTING.md). Contributions are accepted under the Developer
Certificate of Origin 1.1, with a `Signed-off-by` line, not under a contributor licence
agreement.
