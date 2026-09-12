# agentiik

The core of the Agentiik workflow engine, as one Go module. The graph evaluator, the
container driver, the controller, the HTTP API, the runner and the `agk` command line all
belong here, and they belong in one module because the evaluator and the driver have to
stay importable libraries, with no server, no task bus and no database behind them. That
is an architectural constraint rather than a preference: it is what makes
`agk run --local` the same code path a server run takes, instead of a second
implementation that drifts from the first.

What is written so far is the group that settles what travels on a port, the group that
decides what runs next, the group that runs it, and the command line that makes the three of
them usable. The rest of the list is named here because that is where it is going, not
because it is there.

v0.1.0 is closed by one sentence, and it is a test rather than a claim: a multi-step
workflow with a fan-out and a merge runs end to end on a laptop, and running it again on the
same inputs produces the same envelopes. That test is `cmd/agk/milestone_test.go`, it runs
real containers against the real daemon of the machine it is on, twice, and it compares the
envelopes.

What the code is written against is the specification at
<https://agentiik.github.io/docs>, and that is also where the documentation lives; this
README is the only one this repository keeps. The workflow file, the brick manifest and
the envelope are shapes [`agentiik/schemas`](https://github.com/agentiik/schemas) owns,
pinned by version; nothing here redefines them, and where they disagree with this code
the schemas are right.

## What is in the module

v0.1.0 is the group that settles what travels on a port, the evaluator above it, the
container driver beside the evaluator, and the command line that wires the six of them
together. Six library packages and two commands, and `doc.go` at the root says what each one
is for and what it is not.

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
- **`driver`** runs the container. It fills `graph.Driver` and is the only package in the
  module that may reach a Docker daemon. One task is one container and one conversation
  with the Engine API: resolve the image and pull it where the daemon does not hold it,
  read `/agk/brick.yaml` on the first pull and cache it by image digest, refuse a manifest
  declaring a root user before anything is created, prepare what the contract promises
  (the envelope on standard input and at `/agk/in/<port>/envelope.json`, `/agk/repo` and
  `/agk/run.json` and `/agk/params.json` read-only, a secret as a file at
  `/agk/secrets/<name>`, `/agk/out` writable and `/tmp` a sized tmpfs), set the `AGK_*`
  table, apply the settings every container gets (`ReadonlyRootfs`, `CapDrop: ALL`,
  `no-new-privileges`, the pid and memory ceilings, `AutoRemove: false`), give the task a
  network of its own, open the wait before the start so an exit cannot fall between the
  two calls, enforce the step timeout as `SIGTERM` then `SIGKILL` after the grace, read
  the exit code off the table, and collect one envelope per declared port with the files
  uploaded and the oversized values spilled. `before_script`, `script` and `after_script`
  are one shell invocation in one container, the shell defaulting to
  `["/bin/sh", "-e", "-c"]`, and a script that writes nothing and exits 0 publishes one
  item on `out` carrying its captured standard output. Standard error is collected as a
  log that is timestamped, indexed and capped, with the secret values the task was given
  replaced by literal match before anything is written, the item a script publishes
  included. A `Result` means a container ran; an error means none did, and it names the
  step, the port and the rule. No exit code is invented for a failure that produced none,
  because a driver reporting its own trouble as a brick failure fails somebody else's
  step.
- **`cmd/agk`** is the command line, and it is the table `#command-line` states rather than
  a surface of its own. `agk validate` resolves the includes and the inheritance, detects
  the cycles and checks every step's ports against the manifests of the images it names;
  `agk graph` writes the resolved graph as DOT or Mermaid for review inside a merge request;
  `agk run --local` runs the whole graph against the daemon of this machine with no
  controller, no bus and no database, mounting the working tree at `/agk/repo`, giving the
  run a ULID, labelling it `local` so a history never mistakes it for a server run, and
  taking secrets from `--secret` and `--secret-file` to mount them exactly as a server run
  does; `agk brick test` runs a brick against sample envelopes and compares against expected
  outputs. The seven verbs that reach an installation, `login`, `whoami`, `push`, `share`,
  `grants`, `logs` and `brick init`, are each in the table and each refuses naming what is
  missing, because a verb the documentation lists and the binary does not know is a binary
  that looks broken. No scheduling decision, no collection and no daemon call of its own
  lives here: the loop that reads a `Plan`, hands each `Task` to the driver and feeds each
  `Result` back is `cmd/agk/internal/local`, and that loop is the whole of what this group
  adds to the execution path. The flags are the standard library's `flag` package, so the
  module takes no dependency for them. Exit codes are five and each is a different thing to
  do next: 0 did what it says, 1 refused with nothing run, 2 the command line was wrong, 3
  the run reached a terminal state other than `succeeded`, and 4 no outcome could be
  determined.
- **`cmd/agk-helper`** is the static helper bound read-only at `/agk/bin/agk` for a script
  step, with `agk items`, `agk emit` and `agk attach`. It imports `agk` and nothing else,
  which is what lets it be built `CGO_ENABLED=0` and mounted into an image this project does
  not control, and what is about to be bound is checked first: a regular file, a linux ELF,
  the daemon's own machine, and no `PT_INTERP`, which is what "static" means spelled as
  something a machine can check. It is a convenience and never a requirement, so a build
  that carries no binary for the daemon's platform binds nothing and says so in one
  sentence.

Two settings in the driver belong to the operator rather than to a workflow author, and
both are decisions the project took before the code:

- **User namespace remapping is a floor that can be lifted.** The driver refuses a daemon
  that does not remap, so that root inside a container is an unprivileged high-numbered
  account on the host. An operator lifts the refusal with `require_userns_remap = false`
  in `/etc/agentiik/runner.toml`, which is a line in a file rather than a flag so that
  lifting the floor is a thing somebody did on purpose and can be read back. When it is
  lifted the driver says so once, in one plain sentence naming what is given up: a task's
  files are then owned by a real uid on the host. The setting exists because Docker
  Desktop does not offer the remapping and `agk run --local` has to work on a laptop.
  Where the daemon does remap, the driver prepares each task's working directory with
  ownership inside the remapped range before it creates the container, which is the cost
  the documentation warns bind mounts carry.
- **Network egress is refused for now.** `network: none` and `network: internal` run.
  `none` is the absence of a network, which is the default and the stronger of the two,
  and `internal` is a bridge of the task's own with no route out, named after the task and
  removed with it, so that two containers on one host never see each other.
  `network: egress` is refused, before anything is created, with an error saying the
  egress proxy does not exist yet: the posture promises that a proxy on the runner enforces
  the `egress.allow` list, and a workflow must not be able to believe its list is being
  enforced when nothing is enforcing it. Nothing opens the network and calls it filtered.
  The proxy is a task in the v0.2.0 runner group.

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

The arrow between the two points one way, and it is checked rather than asserted.
`graph/boundary_test.go` reads the evaluator's whole import closure and fails it on a
database, an HTTP client, a socket, a process started outside this one, a task bus client,
a registry client, a container runtime, or the path segment `driver`. The evaluator
reaches no daemon, and that is a test rather than a preference.

`internal/expr` is the door CEL is behind. It holds the ten roots of the exposed-context
table and the five positions the table's third column distinguishes, builds a compilation
environment from that table and from nothing else, keeps the type of an expression that
fills a whole value and converts an embedded one to text. Item contents stay out of
expressions except under `fan_out: item`, and a secret is a reference and never a value,
exposed to `params` and `secrets` alone. A root the position does not expose is not
declared, so the table is enforced by the compiler rather than by a guard.

`internal/docker` is the door the Docker daemon is behind, on that same precedent: only
the driver needs a daemon, so only the driver reaches one, and the socket is a package of
its own so that the driver's own files are about mounts, environments, signals and bytes
rather than about HTTP. It speaks the Engine API over the unix socket with the standard
library alone, which is why the module takes no dependency for it: the official client is
large, it carries a dependency tree of its own, and what is needed here is nineteen
endpoints, the frame header that keeps standard output and standard error apart, and the
hijacked connection an attach becomes. The version is negotiated rather than compiled in, as
`min(1.56, what the daemon offers)`, with 1.41 as the oldest daemon supported, because a
hard pin refuses to run where `agk run --local` has to run. `internal/dockertest` is the
other half: a fake daemon on a temporary socket whose container is a Go function a test
supplies, plus the one function that answers whether a real daemon is present.

Dependencies run one way and there is no cycle: `artifact` and `schema` import `agk`,
`brick` imports `agk` and `artifact`, `graph` imports `agk`, `brick`, `schema` and
`internal/expr`, `driver` imports `agk`, `artifact`, `brick`, `graph` and
`internal/docker`, and `agk` imports nothing outside this module but `internal/ulid`,
which is itself the standard library.

The module takes three direct third party dependencies, each recorded in `go.mod` with
the reason it is taken. The driver adds none, and the reason is written above: the Engine
API is nineteen endpoints of JSON over a unix socket, and the standard library speaks it.

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

There is no controller, no HTTP API and no runner. Nothing is published to
`ghcr.io/agentiik/api`, `ghcr.io/agentiik/controller` or `ghcr.io/agentiik/runner` yet
either. The loop that reads a `Plan`, hands each `Task` to the driver and feeds each
`Result` back is here now, once, under `cmd/agk/internal/local`, which is what a local run
is; the controller's own will be a second caller of the same two evaluator methods and not a
second set of rules, which is the property `graph/boundary_test.go` exists to keep.

The command line is here and it is not the whole table. Seven of the documented verbs reach
an installation that does not exist at v0.1.0, and `login`, `whoami`, `push`, `share`,
`grants`, `logs` and `brick init` each refuse naming what is missing rather than being left
out of the binary. `agk run` without `--local` refuses for the same reason, since there is
nothing to reach until the API arrives.

The embedded static helper is a release artifact and not a committed one.
`cmd/agk/internal/helper/bin/` carries a committed `README.md` and no binaries, because
`//go:embed` refuses a directory it matches nothing in and this module has to compile on a
machine that has never cross-compiled the helper. A plain `go build ./...` therefore produces
an `agk` that carries none, and `agk run --local` says so in one sentence and binds nothing;
`--helper <path>` and `$AGK_HELPER` are how a script step gets one in the meantime. Nothing
in this repository populates that directory yet, so the release stage that does is still to
be written.

The Docker code is in `driver` and `internal/docker` and nowhere else. `brick` still knows
the container contract and nothing about how a container is started, which is what lets
`agk brick test` run a brick against sample envelopes with no driver in reach.

`network: egress` is refused rather than approximated, and the proxy that would make it
real is a task in the v0.2.0 runner group. Signature verification before a pull, the
registry credentials a private image needs, the startup sweep that would remove what a
previous process left on the host, and the runner-side `pre_task` and `post_task` hooks
are all the runner's rather than the driver's, and arrive with it. The `cpu_seconds` and
`max_rss_bytes` of the usage block are not measured yet; `image_pull_ms` is.

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

A test run needs no network and no daemon. The two things in the driver that resist a
test are the socket and the container, and each has a package of its own:
`internal/docker` holds the Engine API and nothing else, and `internal/dockertest` is a
fake daemon on a temporary unix socket whose container is a Go function the test supplies,
given the working directory as the driver prepared it and the environment as the driver
computed it. It is a fake daemon on a real socket rather than an in-process interface on
purpose: an in-process fake would let a wire bug pass every rule test, because the rules
would never be marshalled. It can also be asked for the things that actually go wrong, a
pull that fails half way through a 200, a container that exits during the attach, a daemon
that closes every connection, an event stream that drops, an `/info` with and without
`name=userns`, and an out-of-memory kill the wait never sees, so those failure modes are
covered where no daemon exists.

What genuinely needs a real daemon skips itself when there is none, and the decision is
one function rather than a skip condition copied into every file that needs one:
`dockertest.Socket()` consults `DOCKER_HOST`, then the per-user path Docker Desktop puts
its socket at, then `/var/run/docker.sock`. The suite is therefore green in CI and runs
against the real thing on a machine that has one, where it starts real containers off a
small public image and asserts that `/agk/repo` refuses a write, that a secret reaches the
container and never the log, that the settings table is what the container actually lives
under, that a manifest declaring root is refused before a container exists, and that a
redelivered task adopts the container it already started.

The sentence that closes v0.1.0 is one of those tests. `cmd/agk/milestone_test.go` runs the
fixture under `cmd/agk/testdata/milestone` through `run(ctx, Env, args)` itself, the way a
person types it: four steps, three of them bricks built there from plain Dockerfiles and one a
script step using the static helper, a fan-out of three containers, a merge of two edges into
one port under `wait_all`, one committed inputs file and one secret supplied on the command
line. It runs twice, six containers each time, and compares the four declared output
envelopes through `cmd/agk/internal/diff`. What is held aside is `diff.Default` and nothing
more, which is `meta.run_id`, `meta.produced_at` and the run segment of every artifact URI:
the three facts about which run this was. Item identities, item data, counts, port names,
file names, media types, sizes and every `sha256` are compared, and the test fails if the
default is ever widened to hold identities aside, because a proof that gave them up would be
true of a brick that mints a fresh identifier on every pass. It also asserts that two runs
over the same inputs added no second copy to the content-addressed store. Like every other
test that needs a daemon it skips where there is none, so CI stays green and a laptop proves
the sentence.

The released fixture corpus of
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
