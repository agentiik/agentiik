# Changelog

The releases of `agentiik`. Every repository carries the same version and is tagged at the same moment, so an entry may say that nothing changed; [Versioning](https://agentiik.github.io/docs#versioning) says why. `0.y.z` promises nothing beyond itself.

## Unreleased

### Controller

- One active controller, elected by a PostgreSQL advisory lock. Every write carries a fencing counter, so a partitioned former holder is refused.
- Woken by `NOTIFY` when the API writes a run, and sweeping on an interval anyway.
- Decides through the v0.1.0 evaluator rather than a scheduler of its own. The run stores the evaluator's state, so failover is a resume, and each write is refused if the row moved since it was read.
- Envelopes are lifted out of that state into the object store and replaced by their digests.
- A task is published after its row commits and stamped once published; the sweep resends one whose message never went.
- Results are recorded idempotently: states, holder, log, usage, the digest of every port and the artifacts they reference.
- `queued` now waits on something: a concurrency group holds one started run, later ones queue in creation order, and `cancel_in_progress` cancels the running one first.
- `max_concurrent_tasks` holds tasks back rather than failing them.
- Retries wait out their backoff. A task carries the deadline of its step's timeout, and a run past its root timeout is `timed_out` and stops what it holds. Cancelling twice is not an error.
- A task no timeout bounds gets the installation's ceiling, an hour by default.
- A run that ends emits a completion carrying an identifier and a state and nothing else, preceded by a failure when it `failed` or `timed_out`.
- `retain` is resolved by the controller and recorded on the artifact reference.

### State

- Package `db`: the schema, its migrations, and row level security on every namespaced table. A `Pool` has no `Query`, only three doors: `In` binds a namespace, `Installation` steps past it for a named reason, `Session` pins a connection. A superuser connection is refused.
- Artifact and envelope references with reference counting, log URIs on their tasks, the three purges and the collector.
- `steps.state` has a domain of its own, since the task one cannot hold `skipped`.
- The idempotency key column carries the shard cardinality, as `agk.NewTaskID` does.
- `agk.TriggerKind` has the seven kinds the documentation names, and `cron` is now `schedule`.
- `agk.LogURI` addresses a log by the task that wrote it: `agk://log/<run>/<task>`.

### Bus

- Package `bus` fills `controller.Queue` over NATS JetStream: one WorkQueue stream, one subject per runner pool, results on a stream of their own.
- A task message matches `wire.schema.json`, vendored with its fixtures, and carries names and digests rather than the input envelopes.
- A message nobody can decode is taken off the queue and reported, never dropped silently.
- A stop is published on one subject every runner listens to and acted on by whoever holds the task, rather than put on the queue.
- A runner gets an hour-long bus credential from the API for the pool its runner credential names, never one the request names. It may pull from that pool's consumer, acknowledge, publish results and hear stops, and nothing else. The consumer belongs to the pool and only the control plane creates it.

### API

- Deny by default is structural: a route is registered with the permission it needs and the router checks it.
- A refusal at a namespace or a workflow is the same 404 as an absence. 403 is for the installation scope, and a failure to decide is a 500.
- Until access control arrives in v0.3.0, every route that needs a permission is refused.
- Push a version, start a run, list runs, read one. Starting a run answers 202 and creates no task. A body with an unknown field is refused.
- A version stores the entry point, every file the loader read and every image manifest, so it rebuilds with no tree and no registry. It is built before it is saved, and pushing the same commit again changes nothing.
- A runner pool is a row an administrator creates, holding its labels, accepted namespaces and ceilings.
- A join token names one pool and the exact labels a machine may claim, all of them labels that pool carries, and is spent on use. Every bad token gets the same answer.
- Runner routes have a guard of their own, and every bad runner credential is the same 401. Draining is told in the heartbeat; a revoked credential just stops working.
- A heartbeat keeps alive only the runner's own tasks. A runner silent for three intervals leaves its tasks `lost`, not `failed`.
- Grants, join tokens and runner credentials carry 256 bits, are stored hashed and are shown once.
- A grant carries what its task was dispatched with. Redeeming it returns URLs for the input envelopes and the artifacts they name, the secret values and the upload URLs, and binds the task to that runner.
- A presigned URL names one method, one object, one run and an expiry. A presigned write is hashed as it arrives and refused if the bytes do not match their digest. With the built-in store, the API serves the objects.
- An installation with no secret provider holds nothing, and a task naming a secret fails saying which one.
- A version keeps its tree: each file is stored content-addressed, and the version holds a manifest (`path`, `sha256`, `size`, `mode`) with a counted reference to each object, so the collector never takes a file a version names.
- Redeeming a grant also answers the tree of the task's version, one presigned GET per object, in the `grantRedemption` shape of `wire.schema.json`. The controller names the version in the grant; the runner never speaks git.
- A push is refused with 409 when its commit is already recorded with other files, and with 413 above 4 MiB or 4,096 files: a limit of the interim JSON push, until the installation hosts the repository.

### Secrets

- Package `secret`, the built-in store: a fresh AES-256-GCM data key per value, wrapped by a master key read from a file only the API user can open.
- Master keys rotate through a keyring. Resealing is idempotent and does not change a value's version.
- A test holds that the API is the only component reading a secret value.

### Command line

- `agk push` sends a version and the commit's tree, both read from git's objects rather than the working copy. A dirty tree is refused unless `--allow-dirty`, which pushes the commit and leaves the edits behind. `--commit` takes a hash, a branch or a tag. Symbolic links and submodules are refused, and so is a directory outside a repository. The credential comes from `AGENTIIK_TOKEN`, never a flag.

### Tests

- The PostgreSQL and NATS tests run in CI. `internal/dbtest` gives each test its own database and role.
- `driver` has a boundary test, like `graph`.

## v0.1.2, 2026-09-13

Nothing changed here. The release is documentation: a [Get started](https://agentiik.github.io/docs#get-started) chapter and a recorded session of the command line on the home page.

## v0.1.1, 2026-09-13

This file. `v0.1.0` was tagged before its changelog was written, and a Go module tag cannot be moved once the checksum database has recorded it, so this release adds the description instead. A version's entry is now merged before its tag.

## v0.1.0, 2026-09-12

The first release in which a workflow runs.

- `agk run --local` runs a whole workflow against the local Docker daemon, with no controller, bus or database. `cmd/agk/milestone_test.go` runs one with a fan-out and a merge twice and gets the same envelopes.
- The libraries: `agk` (the vocabulary), `artifact` (the content-addressed store, per namespace), `brick` (the container's two edges), `schema` (JSON Schema 2020-12), `graph` (the evaluator) and `driver` (one task as one container).
- `agk validate`, `agk graph`, `agk run --local` and `agk brick test` work. `login`, `whoami`, `push`, `share`, `grants`, `logs` and `brick init` refuse and say why.
- `/agk/bin/agk`, a static helper for script steps: `agk items`, `agk emit` and `agk attach`.
- `network: egress` is refused until the proxy that enforces `egress.allow` exists.
- Where the host has no tmpfs, as on a Mac, a secret value is written into the task's working directory, removed with the container, and the run says so.
