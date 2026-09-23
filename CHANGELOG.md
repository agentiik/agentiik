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
- A result whose state is not an ending, or whose key names no task, is refused with `controller.ErrNotAResult`; the bus takes it off the queue and reports it rather than redelivering it.
- `controller.Answer` names outputs by digest. A success's envelopes are read back from the store, and one not there yet leaves the result for redelivery.
- An envelope is read back no further than one byte past `envelope_max_bytes`, so a digest a runner names never has the controller hold a whole artifact in memory.
- A result naming an object that is not the envelope its digest names (too long, other bytes, or not an envelope) is refused with `controller.ErrNotAResult` rather than delivered again. `artifact.ErrNotAnEnvelope` tells it apart from an object that could not be read.
- A result is taken only from the runner its dispatch was bound to at redemption. Another runner's, or one saying a container ran for a task nobody redeemed, is refused with `controller.ErrNotTheHolder`. The binding is the `task_id`'s and not the key's, so holding a dispatch that was lost gives a runner nothing of its requeue, which answers to whoever redeems it, and that runner is heard, though not as news, for the dispatch it held.
- A task that never reached a container (a refused pull, a grant that would not redeem) is ended by the first runner to report it, which is bound to it as a redemption would bind it, in the transaction that writes the ending. A binding is never left on a task still in flight, where the heartbeat would declare lost a task no container ran for.
- A task that never reached a container is recorded with no exit code, rather than the 0 of success.
- `queued` now waits on something: a concurrency group holds one started run, later ones queue in creation order, and `cancel_in_progress` cancels the running one first.
- `max_concurrent_tasks` holds tasks back rather than failing them.
- Retries wait out their backoff. A task carries the deadline of its step's timeout, and a run past its root timeout is `timed_out` and stops what it holds. Cancelling twice is not an error.
- A task no timeout bounds gets the installation's ceiling, an hour by default.
- A run that ends emits a completion carrying an identifier and a state and nothing else, preceded by a failure when it `failed` or `timed_out`.
- `retain` is resolved by the controller and recorded on the artifact reference.
- A lost task is requeued at once under the same idempotency key and a new `task_id`, where the step is idempotent and `retry.on` names `lost`. A loss does not use up a `retry.max` attempt.
- A loss is heard from the tasks table on the next pass, whether the heartbeat declared it or the runner holding the task reported it. A loss reported twice requeues once. One naming a dispatch bound to another runner, or to none, is refused with `controller.ErrNotTheHolder`, and binds nobody.
- An answer carries the `task_id` of its dispatch. A reported loss moves that dispatch alone, so one delivered late or twice moves nothing, even once the same runner holds the requeue.
- An ending reported for a dispatch its key was requeued past is not news, since the attempt waits on the requeue, and it writes nothing on the requeue's row. An answer naming no dispatch of its key is refused with `controller.ErrNotAResult`.
- An attempt a retry moved past is written as it ended, so it no longer reads as dispatched, counts against `max_concurrent_tasks` or redeems its grant.

### State

- Package `db`: the schema, its migrations, and row level security on every namespaced table. A `Pool` has no `Query`, only three doors: `In` binds a namespace, `Installation` steps past it for a named reason, `Session` pins a connection. A superuser connection is refused.
- Artifact and envelope references with reference counting, log URIs on their tasks, the three purges and the collector.
- `steps.state` has a domain of its own, since the task one cannot hold `skipped`.
- The idempotency key column carries the shard cardinality, as `agk.NewTaskID` does.
- `tasks` keeps one row per dispatch of a key, numbered by `requeue`, and at most one of them that is not `lost`.
- `agk.TriggerKind` has the seven kinds the documentation names, and `cron` is now `schedule`.
- `agk.LogURI` addresses a log by the task that wrote it: `agk://log/<run>/<task>`.

### Bus

- Package `bus` fills `controller.Queue` over NATS JetStream: one WorkQueue stream, one subject per runner pool, results on a stream of their own.
- A task message matches `wire.schema.json`, vendored with its fixtures, and carries names and digests rather than the input envelopes.
- A message nobody can decode is taken off the queue and reported, never dropped silently.
- A result travels as `wire.schema.json` `$defs/taskResult`, with its outputs as digests. `bus.Report` takes a `bus.TaskResult`, and a result the reader refuses (an unknown field, not an ending, what no container could report, a malformed name or digest, a log outside its run) is taken off the queue and reported. The reader takes a runner by the ULID the API mints, which the wire's lowercase pattern for `runner` refuses.
- A runner publishes its results on `agentiik.results.<runner>`, the one results subject its bus credential allows, and a result naming another runner is taken off the queue and reported.
- A result the controller could not record comes back after a pause, from a second doubling to a minute, rather than at once.
- A stop is published on one subject every runner listens to and acted on by whoever holds the task, rather than put on the queue.
- A task is deduplicated on its `task_id` rather than its key, so a requeue published within two minutes of the dispatch it replaces still goes out.
- A result is deduplicated on its runner, its `task_id` and its ending rather than its key, so the requeue's ending still reaches the controller after a late one of the dispatch it replaced, and a holder's ending still reaches it after another runner's refused report of the same dispatch. A result naming no dispatch is not published.
- A runner gets an hour-long bus credential from the API for the pool its runner credential names, never one the request names. It may pull from that pool's consumer, acknowledge, publish results and hear stops, and nothing else. The consumer belongs to the pool and only the control plane creates it.
- A runner's replies come back under an inbox of its own, `bus.Inbox`, the only one its credential may listen on, and it acknowledges on its own pool's consumer alone. Before, any runner could hear every task handed to another, grant included.
- A runner takes work from the consumer the control plane created for its pool, and creates none. `bus.OpenRunner` connects with the runner's credential.
- A runner acknowledges a task on take, once it is written down on the host, and a host that dies mid-task is left to the heartbeat. `Taken.Done` is now `Taken.Held`, and `Taken.Working` is gone.
- `Taken.Held` takes a context and answers once the server confirms the acknowledgement, not once the client has buffered it. A runner starts nothing for a task whose `Held` failed.

### Driver

- A redelivered task never starts its container a second time: a running one is waited on, an exited one is collected as it stands, and a delivery of a task already in flight on the host is refused.
- A redelivery reads a container its deadline stopped as `timed_out`, and gives one that was created and never started its envelope on standard input.
- A key that has completed on a host is never started there again, even once its container is gone: every ending is written under `.keys` in the work root before the container is removed and kept seven days, and a later delivery is refused with `driver.ErrCompleted` before anything is created.
- `Docker.Hold` writes a key down when a runner takes it, before the message is acknowledged, and refuses one that has completed.
- A container that ran to its end ends its key even when what it left cannot be collected or uploaded: Run still answers the error, and the key is written down `failed`.

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
- Only a task a runner has redeemed can be `lost`, counted from its last heartbeat or its redemption, so a task waiting on the queue of a full pool is neither failed nor requeued for the wait.
- Grants, join tokens and runner credentials carry 256 bits, are stored hashed and are shown once.
- A grant carries what its task was dispatched with. Redeeming it returns URLs for the input envelopes and the artifacts they name, the secret values and the upload URLs, and binds the task to that runner.
- The grant of a dispatch that was lost is refused, and so is every grant of a key that has completed. A requeue redeems a grant of its own.
- A grant is never replaced. A task published again, because the pass that published it could not record the dispatch, gets another grant beside the first, and the message the bus kept still redeems; the first redemption binds the task for both. Before, the message on the queue carried a grant that opened nothing. Migration `0014_grants_kept.sql`.
- A presigned URL names one method, one object, one run and an expiry. A presigned write is hashed as it arrives and refused if the bytes do not match their digest. With the built-in store, the API serves the objects, at `/objects/{key...}` beside `/api/v1` so the two route sets can share one router.
- An installation with no secret provider holds nothing, and a task naming a secret fails saying which one.
- A version keeps its tree: each file is stored content-addressed, and the version holds a manifest (`path`, `sha256`, `size`, `mode`) with a counted reference to each object, so the collector never takes a file a version names.
- Redeeming a grant also answers the tree of the task's version, one presigned GET per object, in the `grantRedemption` shape of `wire.schema.json`. The controller names the version in the grant; the runner never speaks git.
- A redemption is asked with `task_id` and `idempotency_key`, both required, and answered in the `grantRedemption` shape except for uploads: artifacts under the port whose envelope names them, and each secret with its `mount` and an `encoding`, base64 when the value is not text. A `mount` the manifest allows and the response pattern does not, such as `/agk/secrets/api.key`, is answered as written.
- A push names its commit by the whole 40-character hash, and is refused with 409 when that commit is already recorded with other files. A tree is at most 4 MiB counted with its paths, 4,096 files, 255 bytes a name and 2,048 a path: limits of the interim JSON push, until the installation hosts the repository. A path a runner could lay out as `.git`, or outside the tree, is refused.

### Secrets

- Package `secret`, the built-in store: a fresh AES-256-GCM data key per value, wrapped by a master key read from a file only the API user can open.
- Master keys rotate through a keyring. Resealing is idempotent and does not change a value's version.
- A test holds that the API is the only component reading a secret value. It follows imports transitively from every package, wherever they lead, and exempts `api` itself but not what imports it.

### Command line

- `agk push` sends a version and the commit's tree, both read from git's objects rather than the working copy. A dirty tree is refused unless `--allow-dirty`, which pushes the commit and leaves the edits behind. `--commit` takes a hash, a branch or a tag. Symbolic links, submodules, SHA-256 repositories and a directory outside a repository are refused before any file is read. The credential comes from `AGENTIIK_TOKEN`, never a flag.

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
