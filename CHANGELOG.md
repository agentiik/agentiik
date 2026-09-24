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
- A key is handed out again after a loss at most `max_requeues` times: three where `controller.Options.MaxRequeues` is unset, none where it is zero, and a negative number is refused. Past it the loss stands: the task stays `lost`, spends no `retry.max` attempt, and its step fails on the infrastructure's account, saying why. `graph.New` takes the bound beside the size rules.
- A step a `merge: first` cancelled keeps the reason it was cancelled for when its task in flight is then lost past `max_requeues`.
- Only a loss counts against `max_requeues`, which is a runner the control plane stopped hearing from. A message the bus hands on because its runner died before redeeming it is the same dispatch delivered again, and a task waiting on a full pool's queue is never lost, so neither spends a requeue.
- A loss is heard from the tasks table on the next pass, whether the heartbeat declared it or the runner holding the task reported it. A loss reported twice requeues once. One naming a dispatch bound to another runner, or to none, is refused with `controller.ErrNotTheHolder`, and binds nobody.
- An answer carries the `task_id` of its dispatch. A reported loss moves that dispatch alone, so one delivered late or twice moves nothing, even once the same runner holds the requeue.
- An ending reported for a dispatch its key was requeued past is not news, since the attempt waits on the requeue, and it writes nothing on the requeue's row. An answer naming no dispatch of its key is refused with `controller.ErrNotAResult`.
- An attempt a retry moved past is written as it ended, so it no longer reads as dispatched, counts against `max_concurrent_tasks` or redeems its grant.
- A run somebody asked to cancel is cancelled on the next pass, before admission, so a queued run is never let in to be called off, nor cancels the run holding its group to make way. The sweep finds the request whatever the run's clock says.
- A run cancelled from `queued` ends with no `started_at`, where it read as started at the moment it was called off.
- Cancelling a run writes every task of it not yet over as `cancelled`, in the pass that ends the run. A message still on the queue then redeems nothing and starts no container, and the run gives back its share of `max_concurrent_tasks` at once. A lost dispatch keeps its loss.
- Cancelling a run also stops every task whose row a runner has redeemed. A task published by a pass that died before recording the dispatch reads pending in the document, and was left running to its deadline.
- A requeue that comes back to the host which already ended its key is answered with that ending, reported under the requeue's `task_id`. Where nobody has redeemed the requeue, the controller takes it from a runner that redeemed an earlier dispatch of the key and binds that runner as the ending is written, so the run no longer waits for its timeout.
- The sweep declares lost every task whose runner has said nothing of it for three heartbeat intervals, then decides the runs it woke on the same pass. Nothing ran that check before, so a silent runner's tasks were never lost.
- The sweep locks a run before its tasks, as a decision does, and passes over a run or a task somebody else holds rather than wait on it, so it never deadlocks with a decision.
- A loss a runner reports locks its run before its task, as a decision does, and waits for a decision on the run rather than deadlocking with it.
- A sweep that cannot look for lost tasks reports why, through `Controller.Trouble` with no run, and still decides the runs that are due.

### State

- Package `db`: the schema, its migrations, and row level security on every namespaced table. A `Pool` has no `Query`, only three doors: `In` binds a namespace, `Installation` steps past it for a named reason, `Session` pins a connection. A superuser connection is refused.
- Artifact and envelope references with reference counting, log URIs on their tasks, the three purges and the collector.
- `steps.state` has a domain of its own, since the task one cannot hold `skipped`.
- The idempotency key column carries the shard cardinality, as `agk.NewTaskID` does.
- `tasks` keeps one row per dispatch of a key, numbered by `requeue`, and at most one of them that is not `lost`.
- `Wide.RedeemedBefore` says whether a runner redeemed an earlier dispatch of a key, and `Wide.BindUnreached` is now `Wide.BindUnredeemed`, since it also binds a requeue answered from a host's record.
- `Pool.Lost` is now `Wide.Lost`, which the controller calls through its fence with its own clock. `db.HeartbeatInterval` is the interval the API tells a runner, and `db.LostAfter` is three of them. A test holds both to the documented figures.
- `agk.TriggerKind` has the seven kinds the documentation names, and `cron` is now `schedule`.
- `agk.LogURI` addresses a log by the task that wrote it: `agk://log/<run>/<task>`.
- `secret_declarations` keeps where each secret of a namespace lives, provider and path, one row per secret and behind the namespace policy. No column could hold a value, and a test holds the columns.
- `secret_values` keeps the built-in store's values sealed, one row per secret behind the namespace policy. A write takes the next version under a row lock, nothing lowers one, and a forgotten value keeps its count.
- `secret_values` also refuses a delete, a truncate, a row inserted holding a value, a row moved to another name and a forgotten value filled again at its own version, so a row kept from before a rotation never comes back in its place.
- A workflow, step, port or secret name is stored as the `identifier` domain. `0001` had called the domain `name`, so its columns got PostgreSQL's own `name` type, which checked nothing and cut a name at 63 bytes; one of up to 255 characters is now kept whole, and a longer one or one off the grammar is refused.
- `Wide.Redeemable` makes every check `Wide.Redeem` makes and writes nothing, so a redemption can be checked before it is answered and bound once it is.
- `db.NewRun.Inputs` is the JSON object a run was started with, written down as it arrived rather than decoded and encoded again.
- `db.RunRoute` is an eighth reason to step past the namespace: a route naming a run and nothing it is of finds which namespace and workflow the run is of, and nothing else.
- `runs.cancel_requested_at` is when a run was first asked to cancel: the API writes it and the controller reads it, and asking again keeps the first moment. Migration `0018_cancel_requested.sql`.
- A `cancelled` run may finish without having started, as one cancelled from `queued` does. Any other run that has finished has started. Migration `0018_cancel_requested.sql`.

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
- `Bus.Ended` answers a task whose key the host already ended: it acknowledges the message and reports the recorded ending under the message's `task_id`. An ending of another key is not sent.
- A runner redeems a task's grant before it acknowledges the message, and pulls only after, where it acknowledged on take. A host that dies before redeeming leaves the message to another runner once `bus.AckWait` has passed, and one that dies after leaves a bound task the heartbeat's sweep declares lost. Nothing sweeps a task nobody redeemed. Secret values are read before the pull.
- `Taken.Refused` acknowledges a task whose redemption was refused, another runner's or one that is over, and starts nothing.
- A runner no longer holds back a task it redeemed because `Taken.Held` failed: every other runner is refused the message that comes round again.
- `bus.AckWait` is a minute, sized for a take and a redemption rather than a task.
- `Bus.Ended` publishes the recorded ending before it acknowledges the message, so a requeue whose report did not go out stays on the queue. Acknowledged first, it left the queue bound to nobody, out of any sweep's reach.
- A redemption that failed without refusing the task, with no answer, the runner's own credential refused or the API failing on its side, is not acknowledged, and the runner keeps the key, names it in its heartbeat and redeems again. Letting go, it left a task its lost answer had bound for the sweep to declare lost before the message came round, spending a requeue on a host that was never lost.
- A test holds `bus.AckWait` to the documented minute.

### Driver

- A redelivered task never starts its container a second time: a running one is waited on, an exited one is collected as it stands, and a delivery of a task already in flight on the host is refused.
- A redelivery reads a container its deadline stopped as `timed_out`, and gives one that was created and never started its envelope on standard input.
- A key that has completed on a host is never started there again, even once its container is gone: every ending is written under `.keys` in the work root before the container is removed and kept seven days, and a later delivery is refused with `driver.ErrCompleted` before anything is created.
- `Docker.Hold` writes a key down when a runner takes it, before the message is acknowledged, and refuses one that has completed.
- `Docker.Hold` also holds the task in memory, so a stop that lands between the redemption and `Run` is kept and its container is never started. `Docker.Release` lets go of a key the runner will not run.
- `Docker.Hold` refuses a key its host still has in flight with `driver.ErrTaskInFlight`, before anything is redeemed. A requeue reaching the host still running its key waits unredeemed and is answered from the record, where it was bound, never answered and lost a second time, so one cut spent two of `max_requeues`.
- A secret the source cannot give fails saying the value comes from the redemption, made before the pull, rather than at the last moment.
- A container that ran to its end ends its key even when what it left cannot be collected or uploaded: Run still answers the error, and the key is written down `failed`.
- The record of a key's ending keeps what it left by reference and never a payload: each port's envelope by digest and count, each artifact by digest and size, the log's address and length.
- Each port's envelope is written to the store before the ending is recorded, and the terminal `driver.Event` names it in `Outputs` by the digest the store answered, through `artifact.Store.PutEnvelope`. An envelope the store refuses ends the key `failed`, naming nothing, charged to the platform.
- A key that has ended is refused with a `driver.Completed` holding that ending, so a runner answers a requeue that comes back to it without running the brick again.
- A `driver.Completed` is whole with its `Ending` alone, so one written as a literal reads as `driver.ErrCompleted` and names its key instead of dereferencing nothing.
- A secret mount is one file directly under `/agk/secrets/`, on the grammar the manifest, the task message and the redemption now share. `client.key` is mounted; `.`, which replaced the secrets directory with the value, and `..`, which failed as the platform's fault, are refused, as is any name beginning with a dot.
- `driver.LoadPolicy` reads every host setting of `/etc/agentiik/runner.toml`, strictly: a key it does not read or spelled in another case, a wrong type, or a value outside its setting is refused, naming the line where it has one. `nproc` follows `pids_limit` unless written. `[hooks]` is read past until v0.9.0, and the driver says so.
- `Policy.Seccomp` is the profile's JSON, which the Engine API takes, rather than a path the daemon cannot decode. `seccomp_profile` names the file it is read from.
- A runner refuses a daemon that applies no seccomp profile with `driver.ErrSeccompRequired`, and no setting lifts it; `agk run --local`, `agk validate` and `agk brick test` say so instead. A daemon with neither AppArmor nor SELinux is taken and said out loud, and a profile or label it would ignore is refused.
- A `seccomp_profile` that cannot be read refuses the file without wrapping `fs.ErrNotExist`, which says there is no `runner.toml` and has its caller drop every setting.

### API

- Deny by default is structural: a route is registered with the permission it needs and the router checks it.
- A refusal at a namespace or a workflow is the same 404 as an absence. 403 is for the installation scope, and a failure to decide is a 500.
- Until access control arrives in v0.3.0, every route that needs a permission is refused.
- A ninth permission, `secret:write`, declares and removes a namespace's secrets.
- Push a version, start a run, list runs, read one. Starting a run answers 202 and creates no task. A body with an unknown field, or anything after its document, is refused.
- A version stores the entry point, every file the loader read and every image manifest, so it rebuilds with no tree and no registry. It is built before it is saved, and pushing the same commit again changes nothing.
- A runner pool is a row an administrator creates, holding its labels, accepted namespaces and ceilings.
- A join token names one pool and the exact labels a machine may claim, all of them labels that pool carries, and is spent on use. Every bad token gets the same answer.
- Runner routes have a guard of their own, and every bad runner credential is the same 401. Draining is told in the heartbeat; a revoked credential just stops working.
- A heartbeat keeps alive only the runner's own tasks. A runner silent for three intervals leaves its tasks `lost`, not `failed`.
- Only a task a runner has redeemed can be `lost`, counted from its last heartbeat or its redemption, so a task waiting on the queue of a full pool is neither failed nor requeued for the wait.
- Grants, join tokens and runner credentials carry 256 bits, are stored hashed and are shown once.
- The grant of a dispatch that was lost is refused, and so is every grant of a key that has completed. A requeue redeems a grant of its own.
- A grant carries what its task was dispatched with. Redeeming it returns URLs for the input envelopes and the artifacts they name, the secret values and somewhere to write, and binds the task to that runner.
- A grant is never replaced. A task published again, because the pass that published it could not record the dispatch, gets another grant beside the first, and the message the bus kept still redeems; the first redemption binds the task for both. Before, the message on the queue carried a grant that opened nothing. Migration `0014_grants_kept.sql`.
- A presigned URL names one method, one object, one run and an expiry. A presigned write is hashed as it arrives and refused if the bytes do not match their digest. With the built-in store, the API serves the objects, at `/objects/{key...}` beside `/api/v1` so the two route sets can share one router.
- An installation with no secret provider holds nothing, and a task naming a secret fails saying which one.
- A redemption tells the runner whether a secret it names is not held or held and unreadable, and hands the store's reason to `RunnerOptions.Trouble` for whoever runs the installation.
- A redemption binds its task only once it has an answer to give. A secret the store cannot give, or an input envelope it cannot read, is refused and binds nothing, as a missing tree already did: the refused runner's report that no container ran ends the dispatch, and only a second delivery could redeem it. A runner that dies before that report has not acknowledged the message, so the next runner of the pool is handed it, where the heartbeat found it lost before. The values are read last, once nothing else can refuse, and in no transaction.
- A test has runners redeem one task at once, all of them past the check before any is bound, and holds that one is given the task and the rest are refused with no value.
- A version keeps its tree: each file is stored content-addressed, and the version holds a manifest (`path`, `sha256`, `size`, `mode`) with a counted reference to each object, so the collector never takes a file a version names.
- Redeeming a grant also answers the tree of the task's version, one presigned GET per object, in the `grantRedemption` shape of `wire.schema.json`. The controller names the version in the grant; the runner never speaks git.
- A redemption is asked with `task_id` and `idempotency_key`, both required, and answered in the `grantRedemption` shape: artifacts under the port whose envelope names them, and each secret with its `mount` and an `encoding`, base64 when the value is not text. A `mount` the manifest allows and the response pattern does not, such as `/agk/secrets/api.key`, is answered as written.
- Somewhere to write is one signed POST policy per task, answered at the first redemption, bounded to `<namespace>/sha256/` and the run, and good until the grant expires and not after, so a task redeems once and its secrets are read once. The request no longer takes `upload`. The built-in store takes the form at `POST /objects/{namespace}`, only under a key that is the prefix and 64 lowercase hex characters, and hashes the file as it arrives, as it does a PUT.
- A posted form carries at most 64 KiB before its file. The file itself is bounded by `artifact_max_bytes` alone, and above it the post is refused with 413 and nothing is stored.
- A push names its commit by the whole 40-character hash, and is refused with 409 when that commit is already recorded with other files. A tree is at most 4 MiB counted with its paths, 4,096 files, 255 bytes a name and 2,048 a path: limits of the interim JSON push, until the installation hosts the repository. A path a runner could lay out as `.git`, or outside the tree, is refused.
- A push to a workflow whose name is off the identifier grammar or past 255 characters is refused with 400, and one whose files write a name past 255 with 422, before any of its tree is stored.
- `GET /api/v1/{ns}/secrets` lists a namespace's secret declarations, and `GET`, `PUT` and `DELETE /api/v1/{ns}/secrets/{name}` read, write and remove one: name, provider (`builtin`, `env` or `vault`), path and mount point, never a value. Reading takes `workflow:read` and writing `secret:write`.
- A `builtin` declaration's `PUT` may carry its value, `base64` when it is not text, handed to the built-in store in the declaration's transaction and never answered. With no store attached it is a 503. Removing a secret, or moving it out of the store, forgets its value.
- Removing a secret, or moving it out of the built-in store, forgets its value with no store attached too, since another process of the installation may have written it.
- A declaration is confined to its namespace when it is written. `env` is refused unless the installation opts in, and then takes only a variable under the prefix it gives that namespace; `vault` is refused until its provider arrives.
- Every `env` prefix begins with `AGK_DEV_`, under which the API reads nothing for itself, so no namespace reaches the API's own variables.
- A secret's name is at most 255 characters, since a step is given its value in a file named after it, and a path at most 1 KiB.
- A declaration's `declared_at` is the stored time, in UTC, in the answer to its `PUT` as in every read.
- A request body is read a token at a time, into what its route keeps, and every collection is counted as it is read. Reading one costs at most two and a half times its route's cap, where a 16 MiB push of empty tree entries cost 295 MiB and 8 MiB of inputs written `[{},{},...]` 508 MiB.
- Each route has a cap of its own: 64 KiB for a pool, a join token, a join, a redemption and a bus credential, 1 MiB for a heartbeat, and 4 MiB, one envelope, for starting a run. A body past its cap, or a list past its count, is refused with 413: 1,024 labels or namespaces, 4,096 keys in a heartbeat, 4,096 includes or manifests in a push.
- A run's inputs are counted, at most 100,000 values, and written down as they were sent rather than decoded. A number no 64-bit float holds is refused.
- A field, a tree file, an include or a manifest written twice is refused, and so are a field named in another case and text that is not UTF-8.
- A body is held as it arrives, not as it declares: a push declared and never sent holds 4 KiB rather than 16 MiB. A body sent in chunks costs what one declaring its length does, where it cost up to five times its cap.
- A number in a run's inputs that a 64-bit float holds only as zero, or that reaches more than 340 digits from the point, is refused with 400. PostgreSQL writes a number back at the scale it was sent with, so `0e-16383` was read back as 16 KB at every decision, and `1e-16384` was a 500.
- Inputs holding U+0000 in a string or a name are refused with 400, where PostgreSQL refused them with a 500.
- A body that is not JSON is refused saying where it stops being JSON, and no longer repeats the bytes there, which could be part of a secret's value.
- A route whose body is optional, a bus credential or a cancellation, reads one that declares no length, as a body sent in chunks does. A field it refuses was accepted and dropped that way.
- `api.OnRun` authorises a route whose path names a run and nothing it is of against the namespace and workflow the run is of, found by its identifier alone. `POST /api/v1/runs/{run}/cancel` is the first to take it. A run that is not there, or an identifier no run was minted with, is the same 404 as a run the caller may not reach, where U+0000 or bytes that are not UTF-8 were a 500.
- `POST /api/v1/runs/{run}/cancel` asks for a run to be cancelled, with `workflow:run` on its workflow. It writes the request and notifies, and the controller does the rest. The answer is 202 and the run, the same whether the run is going or has ended, since its state is for `run:read` to show. Asking twice is asking once. The audit log records it once there is one.

### Secrets

- Package `secret`, the built-in store: a fresh AES-256-GCM data key per value, wrapped by a master key read from a file only the API user can open.
- Master keys rotate through a keyring. Resealing is idempotent and does not change a value's version.
- A test holds that the API is the only component reading a secret value. It follows imports transitively from every package, wherever they lead, and exempts `api` itself but not what imports it.
- A workflow's `secrets` block is a list of names, `secrets: [billing]`. Where a value lives is the namespace's declaration, and a block still writing a provider or a path is refused.
- A test sends a request to every API route but the redemption, with a secret store that fails if it is read.
- A test decides a run with the namespace's declarations and the built-in store's values out of the controller's reach, and holds that each task's grant names the secrets its step mounts, with their mounts, and nothing a value could be kept under.
- A test holds that every redemption reads each secret its grant names from the store again, in the grant's namespace and nothing else, so a value rotated after the dispatch arrives rotated, and that no value the store held is written anywhere in the database. What asking again answers after a rotation is left open.
- `secret.Builtin` keeps a `builtin` value in `secret_values`: sealed on the declaration's `PUT` at the next version, opened as bytes under whichever key of the ring sealed it. A row copied into another namespace, another name or over a later write does not open.
- `secret.Env` reads the API's environment for development: only for a namespace the installation opts in, only under the prefix it gives that namespace, held again at every read. A variable set to nothing is not a value.
- `secret.Providers` fills `api.Secrets`: it reads a secret through the namespace's declaration, from the store the declaration names, as bytes. A store the installation does not read, or does not know, is refused naming the secret and never a value.
- `secret.Attach` wires both stores into the API's options from one configuration, so the routes and the redemption cannot disagree. `api` holds only the interfaces, and `cmd/agk` links none of it.

### Command line

- `agk push` sends a version and the commit's tree, both read from git's objects rather than the working copy. A dirty tree is refused unless `--allow-dirty`, which pushes the commit and leaves the edits behind. `--commit` takes a hash, a branch or a tag. Symbolic links, submodules, SHA-256 repositories and a directory outside a repository are refused before any file is read. The credential comes from `AGENTIIK_TOKEN`, never a flag.
- `agk validate` and `agk run --local` refuse a name longer than 255 characters in a workflow file or a brick's manifest: a step, a port or a secret becomes a file or a directory name, and none is longer.

### Tests

- The PostgreSQL and NATS tests run in CI. `internal/dbtest` gives each test its own database and role.
- `driver` has a boundary test, like `graph`.
- A requeue answered from a host's record is checked acknowledged on the pool's consumer, which a second take inside AckWait could not tell.
- `Wide.RedeemedBefore` is held to leaving out a dispatch redeemed after the one asked about.
- A key lost past `max_requeues` is held through the controller to going out no more, its last grant opening nothing even to the runner that held it, and failing its run, at the default and at a number the installation sets.

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
