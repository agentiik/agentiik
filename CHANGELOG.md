# Changelog

The releases of `agentiik`. Every repository carries the same version and is tagged at the same moment, so an entry may say that nothing changed; [Versioning](https://agentiik.github.io/docs#versioning) says why. `0.y.z` promises nothing beyond itself.

## Unreleased

### Runner

- A runner may claim no label: `agk-runner join` without `--labels` joins the token's pool claiming none, which with a token of the pool `default` takes the steps naming no `runs_on`. `serve` refuses `AGK_RUNNER_LABELS` in the environment of a runner that joined claiming none.
- `agk-runner join --replace` claims only the labels it is given, no longer those of the `runner.env` it replaces.

### API

- `agentiik-api namespace create NAME` creates a namespace and `namespace remove NAME` removes one holding no workflow, run or secret, where the API runs and with its database settings, until v0.3.0's routes. Both are audited as `namespace.create` and `namespace.delete`.

## v0.2.0, 2026-09-26

The first server release. An installation of the API, the controller, the bus and runners runs a pushed workflow as `agk run --local` runs it, and a test installation stood up from the checkout proves it in CI. Access control arrives in v0.3.0; until then only the operator token is allowed anything.

### Upgrading

- A run in flight across the upgrade from a v0.2.0 pre-release build reads a whole-number input as an int from its next pass, where it read a double, so drain runs before upgrading from one.

### Controller

- One active controller, elected by a PostgreSQL advisory lock, with a fencing counter on every write, so a partitioned former holder is refused. `Controller.Lead` checks the lock on every poll and ends the term with `controller.ErrLockLost` when it cannot.
- Woken by `NOTIFY` when the API writes a run, and sweeping on its own interval anyway. A pass that changes nothing writes nothing.
- Decides through the v0.1.0 evaluator. The run stores the evaluator's state, so failover is a resume, and envelopes are lifted into the object store and replaced by their digests.
- A task is published after its row commits and stamped once published; the sweep resends one whose message never went.
- Results are recorded idempotently, taken only from the runner its dispatch was bound to at redemption (`controller.ErrNotTheHolder`), and refused when they are not an ending, name no dispatch or name an object that is not their envelope (`controller.ErrNotAResult`). `controller.Answer` names outputs by digest.
- A task reads `running` while its container runs and `publishing` while its outputs go up. One that never reached a container is ended by the first runner to report it, with no exit code.
- Concurrency groups hold one started run, later ones queue in creation order, and `cancel_in_progress` cancels the running one first. `max_concurrent_tasks` holds tasks back rather than failing them.
- Retries wait out their backoff. A task carries its step's timeout as a deadline, the installation's ceiling where none bounds it (an hour by default), and a run past its root timeout is `timed_out`.
- A cancellation is acted on at the next pass, before admission; a run cancelled from `queued` has no `started_at`. A run that ends `cancelled` or `timed_out` ends every task not yet over the same way and stops every redeemed one.
- A task stopped as `superseded` or `sibling_failed`, or left in flight by a `merge: first` when its run ends, is written `cancelled` in the pass that sends the stop, freeing its slot at once.
- A `timed_out` or `cancelled` task keeps its container's exit code (137 or 143); a lost task and an ending no container reached have none (`graph.Result.NoExitCode`).
- A run that ends emits a completion carrying its identifier and state, preceded by a failure when it `failed` or `timed_out`.
- `retain` is resolved by the controller and recorded on the artifact reference.
- The sweep declares lost every task whose runner has said nothing of it for three heartbeat intervals. A lost task is requeued under the same idempotency key and a new `task_id` where the step is idempotent and `retry.on` names `lost`, spending no `retry.max` attempt, at most `max_requeues` times (3 by default); past it the step fails on the infrastructure's account. A redelivery or a wait on a full pool's queue is not a loss.
- A loss moves only the dispatch its `task_id` names, and is heard once however often it is reported.
- A requeue that reaches the host which already ended its key is answered with that ending.
- A step goes to the pool whose labels include every label of its `runs_on`, among the pools that accept the run's namespace, and one naming no label to `default`. No pool, or more than one, fails its pending shards with exit code 125 saying why (`graph.Result.Reason`). Resources are capped to the pool's ceilings.
- A run on a server starts with the workflow's `vars`.
- A number written without a fraction or an exponent is an int in an expression and any other a double, wherever it comes from, locally and on a server alike. `.inf` and `.nan` are refused.
- `agentiik-controller`: a static binary and a non-root image, linking no secret store. It stands by until it holds the lock, and asks PostgreSQL to probe its connections, so a controller cut off frees the lock within half a minute.
- The leading controller verifies the audit chain at the start of each term, from how far the last verification reached (`audit_verified`), and exports the log to `AGK_AUDIT_EXPORT_URL` as newline-delimited JSON, at least once. `agentiik-api audit-verify FILE` checks an export.
- Prometheus metrics on `AGK_METRICS_LISTEN`, behind a bearer token: dispatches, losses, retries, durations, latency, queue depth and runner slots. Only the leader reports, and a metric keeps at most 1,000 label sets. `controller.Options.Observer` is told each event.
- A run that ends is exported as one OpenTelemetry trace to `AGK_OTLP_ENDPOINT` (`internal/otlp`, standard library only). A collector that is down costs the spans and never a decision.
- `agentiik-api`, `agentiik-controller` and `agk-runner` end at a second SIGINT or SIGTERM (`internal/stopsignal`).

### State

- Package `db`: the schema, its migrations, and row level security on every namespaced table. A `Pool` has only three doors: `In` binds a namespace, `Installation` steps past it for a named reason, `Session` pins a connection. A superuser connection is refused.
- `db.Provision` applies the migrations under an advisory lock and creates, or brings back to shape, the `NOSUPERUSER NOBYPASSRLS` role the API and the controller connect as, with no superuser needed. It refuses a role that owns anything.
- Artifact and envelope references with reference counting, the three purges and the collector. A grant counts the input envelopes it names.
- `tasks` keeps one row per dispatch of a key, numbered by `requeue`, at most one of them not `lost`.
- A workflow, step, port or secret name is the `identifier` domain, up to 255 characters.
- `runs.cancel_requested_at` keeps the first request to cancel (`0018_cancel_requested.sql`).
- Runner pools hold their name, labels, namespaces and cpu, memory and pids ceilings (`0019_pool_shape.sql`); an installation is created with the pool `default`, which carries no label, accepts every namespace and has no ceiling (`0028_default_pool.sql`).
- A runner keeps its Ed25519 public key, its narrowed namespaces and its containment (`0021_runner_identity.sql`), its rotated credential until the new one is used (`0023_credential_rotation.sql`), and who drained or revoked it and when (`0025_revocation.sql`).
- A version keeps the digest each tag resolved to (`0020_version_images.sql`) and its tree, content-addressed, as a manifest of `path`, `sha256`, `size` and `mode`.
- Grants are kept, never replaced (`0014_grants_kept.sql`).
- `secret_declarations` keeps where each secret lives, never a value; `secret_values` keeps the built-in store's values sealed and versioned, refusing a delete, a truncate or a row moved.
- `artifacts.fetches_held_until` holds a fetch until it completes, so one that fails spends nothing.
- `tasks` may carry an exit code on a `timed_out` or `cancelled` row (`0027_stopped_exit_codes.sql`).
- The audit log, append-only and hash-chained, written in the act's transaction (`0030_audit_log.sql`); triggers refuse an update, a delete or a truncate to every role. Verification progress is kept in `audit_verified` (`0031_audit_verified.sql`).
- `agk.TriggerKind` has the seven kinds the documentation names, and `cron` is now `schedule`.
- `agk.LogURI` addresses a log by its task: `agk://log/<run>/<task>`.

### Bus

- Package `bus` over NATS JetStream: one WorkQueue stream with a subject per runner pool, results on a stream of their own, and stops on `agentiik.stops`, heard by every runner. The controller's half is `bus/control`, so a runner links `bus` without the controller or the database.
- Tasks, results, progress and stops travel as `wire.schema.json`, vendored with its fixtures. A task carries names and digests, not the input envelopes; a message nobody can read is taken off the queue and reported.
- A task is deduplicated on its `task_id`, and a result on its runner, `task_id` and ending, so a requeue always goes out and its ending always comes back.
- A result the controller could not record comes back after a pause, from a second doubling to a minute.
- A runner gets an hour-long credential from the API for its own pool: pull from that pool's consumer, acknowledge, publish on `agentiik.results.<runner>`, hear stops, reply under its own `bus.Inbox`, and nothing else. A revoked runner's credential only publishes and hears stops.
- A runner redeems a task's grant before acknowledging its message; `bus.AckWait` is a minute. A host that dies before redeeming leaves the message to another runner.
- `bus.NewInstallation` creates the operator and accounts and writes `accounts.conf`, `account.seed` and `control-plane.creds`, keeping the operator's seed nowhere. `bus.RenewControlPlane` renews the control plane's credential in place.
- `bus.Route` chooses a task's pool; `Bus.Depths` reads each pool's queue depth.
- A bus address in plaintext to anything but loopback is refused (`bus.ErrPlaintext`).

### Driver

- A redelivered task never starts its container twice, and a key that has completed on a host is never started there again: every ending is recorded under `.keys` in the work root for seven days, by reference and never a payload, and a later delivery is refused with a `driver.Completed` holding it.
- `Docker.Hold` writes a key down when a runner takes it and refuses one in flight (`driver.ErrTaskInFlight`) or completed; `Docker.Release`, `Docker.Recorded`, `Docker.Dispatched`, `Docker.Ended` and `Docker.Logged` read and amend the record.
- Each port's envelope is written to the store before the ending is recorded, and the terminal `driver.Event` names it by digest.
- `driver.WithSources` gives each task the store, secret source and tree its redemption answered.
- Every envelope is held to its limits before the first upload; a container that exited 0 with refused outputs is `failed` with exit code 121 (`driver.ExitContractBroken`), charged to the brick.
- A secret mount is one file directly under `/agk/secrets/`, and a name beginning with a dot is refused.
- Runner floors no line of `runner.toml` lifts: a remapped daemon needs `CAP_CHOWN`, `CAP_FOWNER` and `CAP_DAC_OVERRIDE` (`driver.ErrOwnershipCapabilities`), the secrets directory a tmpfs mounted `noexec,nosuid,nodev` (`driver.ErrSecretsTmpfsRequired`), the daemon a seccomp profile (`driver.ErrSeccompRequired`), and every image a digest (`driver.ErrImageNotByDigest`). They are read again after a daemon restart. `agk run --local` and `agk brick test` lift them.
- `driver.LoadPolicy` reads every setting of `/etc/agentiik/runner.toml` strictly, naming the line it refuses, and replaces `driver.ParseUsernsFloor`. A profile, label or cap the daemon would not honour is refused, and `[hooks]` runs nothing until v0.9.0.
- A task's `internal` network is isolated from the runner host through the gateway, and refused on a daemon older than Docker 28.0 (`driver.ErrInternalNotIsolated`). `Docker.Sweep` removes the networks a dead runner left.
- The pull and the manifest read are bounded by the task's deadline, and a pull refused for want of credentials says so.
- The terminal event carries the exit code, span, `cpu_seconds` and `max_rss_bytes` sampled from the daemon; a container gone before any sample reports neither.
- A task's log caps count standard error alone.
- `Docker.Pin` resolves a tag to the digest its registry serves, or `driver.ErrNotPushed`.
- A container is given `TRACEPARENT`, derived by `agk.RunID.Trace` and `agk.TaskSpan`, so the runner, the controller and `agk run --local` name the same trace.
- Empty task directories are swept from the work root and the secrets tmpfs after an hour.

### Runner

- `agk-runner`, one static binary and a `linux/amd64` and `linux/arm64` image (`build/runner.Dockerfile`), run as 65532 with the three capabilities as file capabilities, with the verbs `join`, `serve` and `version`.
- `join` generates the host's Ed25519 key, measures its capacity and joins, writing `/var/lib/agentiik/runner.key` and `/etc/agentiik/runner.env` (0600) once the API has answered. `--replace` replaces an existing identity.
- `serve` reads its settings from the environment or `runner.env`, read strictly and refused when anybody else can read it, and `/etc/agentiik/runner.toml`. It refuses root, a daemon below the floors and a `DOCKER_HOST` that is not a local unix socket, before any call to the API, and tells systemd `READY=1`.
- `serve` takes as many tasks as `AGK_RUNNER_CONCURRENCY` allows, and puts back one of a namespace `AGK_RUNNER_NAMESPACES` leaves out or that would exceed the host's memory and vCPU. Each is written down, redeemed, acknowledged, assembled (`runner.Assemble`), run and reported as `taskResult` (`runner.Carrier`).
- A redemption with no answer is tried again, from 1 s doubling to 30 s, until the deadline; one refused is reported with no container ran.
- A result is kept under `<work root>/.results` from before the task runs until the bus takes it, so a crash at any point loses none.
- A heartbeat every 10 s names every key the runner answers for; the answer's `cancel` stops tasks and `drain` stops taking. A 401 exits 3.
- Stops heard on `agentiik.stops` are sent to the driver once per key.
- Logs are shipped to `POST /api/v1/tasks/logs` while the container runs, a chunk a second or as soon as one is full (4,096 lines, 1 MiB), and resumed after a restart.
- `serve` renews its runner credential at two thirds of its window through `POST /api/v1/runners/rotate`, kept in `/var/lib/agentiik/credential`, and its bus credential at three quarters of its life on a second connection.
- A drained runner stays up, idle and reporting `draining`; a revoked one exits 3 once its results are published or its grace ends.

### Artifacts

- `artifact/granted` reads through the presigned GETs a redemption names and writes through the task's signed upload policy, refusing any other key without sending anything, and refusing `http` to anything but loopback.
- The store's refusals come back as `artifact.ErrNotSigned`, `artifact.ErrWrongDigest`, `artifact.ErrTooLarge` and `fs.ErrNotExist`, and no error names its URL.
- `artifact.Store.Describe` answers the entry `Put` would, writing nothing.

### API

- `agk.ReservedNamespaces` fixes the words the API routes on after `/api/v1/`, and a workflow may not use one as its namespace.
- Deny by default is structural: a route is registered with the permission it needs and the router checks it. A refusal at a namespace or a workflow is the same 404 as an absence; 403 is for the installation scope.
- A ninth permission, `secret:write`.
- Push a version: `agk push`'s commit, tree and `images` (each tag's digest). A version stores every file and image manifest, so it rebuilds with no tree and no registry, and pushing the same commit again changes nothing.
- Start a run (202), binding its inputs against the version's declaration as a local run does; an input refused is 422 with `error`, `input` and `rule`.
- `GET /api/v1/runs` and `GET /api/v1/{ns}/runs` list runs by the caller's `run:read` on each workflow, filtered by `namespace`, `workflow`, `state`, `since`, `until` and `limit`. `GET /api/v1/runs/{run}` reads one; its inputs need `run:read_data`.
- `POST /api/v1/runs/{run}/cancel` asks for a run to be cancelled, with `workflow:run`, answering 202.
- `GET /api/v1/runs/{run}/outputs/{name}`, `.../steps/{step}/outputs/{port}` and `.../steps/{step}/inputs/{port}` answer envelopes with `run:read_data`, 410 once purged.
- `GET /api/v1/artifacts/{uri}` redirects to a five-minute presigned URL, or serves the bytes where the artifact has a fetch budget.
- `GET /api/v1/runs/{run}/steps/{step}/logs` streams a step's log as server-sent events, history then live, resumable with `Last-Event-ID` (`0029_tasks_by_step.sql`). `POST /api/v1/tasks/logs` takes the runner's chunks (`0026_task_logs.sql`).
- Runner pools and single-use join tokens in the `runnerPool` shape, under `grant:manage`. A join speaks `runnerRegistration`, and a bad token always gets the same 401.
- Runner routes have a guard of their own, with one 401 for every bad credential. The heartbeat speaks `runnerHeartbeat` and answers `received_at`, `drain` and `cancel`.
- `POST /api/v1/runners/rotate` renews a runner credential, signed by the key the runner joined with.
- `POST /api/v1/runners/{runner}/drain` and `/revoke` take a `reason`. A revoked runner is heard for a grace (`AGK_REVOCATION_GRACE`, the task ceiling by default) to finish what it holds.
- Grants, join tokens and runner credentials carry 256 bits, are stored hashed and shown once. A grant is never replaced; a lost dispatch's grant, and every grant of a completed key, is refused.
- Redeeming a grant, with `task_id` and `idempotency_key`, answers in the `grantRedemption` shape: presigned URLs for input envelopes, artifacts and the version's tree, the secret values, and one signed upload policy. It binds the task only once it has an answer; a draining or revoked runner, or one whose pool or namespaces leave the task out, is refused, and what the installation can never answer is 422.
- A presigned URL names one method, one object, one run and an expiry. The built-in store serves objects at `/objects/{key...}` and takes uploads at `POST /objects/{namespace}`, hashing them as they arrive.
- `GET`, `PUT` and `DELETE /api/v1/{ns}/secrets/{name}` and the listing manage secret declarations (`builtin`, `env` or `vault`), never answering a value. A `builtin` `PUT` may carry the value. `env` is opt-in, under an `AGK_DEV_` prefix, and `vault` refused until its provider arrives.
- Bodies are read a token at a time under a cap per route, and a list past its count is 413; duplicate or case-varied fields, text that is not UTF-8 and U+0000 are refused.
- `POST /api/v1/bus/token` mints a runner's bus credential, and each pool's consumer is made ready as the pool is created.
- Manual triggers, cancellations, secret writes, pools, join tokens, drains and revocations are recorded in the audit log, never with a secret's value or a token.
- `agentiik-api`: a static binary and a non-root image, the one program that links the secret store, with `serve`, `migrate`, `bus-init`, `bus-credential` and `audit-verify`. Until v0.3.0, the token whose SHA-256 `AGK_OPERATOR_TOKEN_FILE` holds is allowed everything, and nobody else anything.

### Secrets

- Package `secret`, the built-in store: a fresh AES-256-GCM data key per value, wrapped by a master key from a file only the API user can open, rotating through a keyring.
- A workflow's `secrets` block is a list of names, `secrets: [billing]`; where a value lives is the namespace's declaration.
- `secret.Builtin`, `secret.Env` and `secret.Providers` read a secret through its declaration; `secret.Attach` wires them into the API.
- Each redemption reads its secrets again, so a rotated value arrives rotated.
- Tests hold that the API is the only component reading a secret value, through the redemption alone, and that no value reaches the database outside `secret_values`.

### Configuration

- Package `internal/config` reads each program's settings from its own `AGK_*` variables, and names every one missing or malformed.
- A secret is a file an `AGK_*_FILE` variable names, readable by its owner alone; a secret given as a value, a password in a URL, `PGPASSWORD` or `PGSSLPASSWORD` refuses the start. `config.Secret` prints as `[secret]`.
- No plaintext path: a database URL needs `sslmode` `verify-full`, `verify-ca` or `require` unless local, `AGK_BUS_URL` is `tls://` or `wss://`, and `AGK_PUBLIC_URL` is `https`. TLS 1.2 is the floor everywhere (`internal/tlsfloor`).
- `AGK_TLS_CERT_FILE` and `AGK_TLS_KEY_FILE` have the API and the metrics listener serve TLS themselves.
- The controller refuses `AGK_MASTER_KEY_FILE`, which is the API's alone.
- New settings: `AGK_MAX_REQUEUES`, `AGK_TASK_CEILING`, `AGK_REVOCATION_GRACE`, `AGK_ENV_PREFIXES`, `AGK_AUDIT_EXPORT_URL`, `AGK_AUDIT_EXPORT_TOKEN_FILE`, `AGK_METRICS_LISTEN`, `AGK_METRICS_TOKEN_FILE` and `AGK_OTLP_ENDPOINT`.

### Command line

- `agk push` sends a version and its commit's tree, read from git's objects, with every tag resolved to its digest. A dirty tree is refused unless `--allow-dirty`, and the credential comes from `AGENTIIK_TOKEN`.
- `agk run --namespace` starts a run of a pushed commit on an installation and follows it to its end as a local run does; `-o json` writes its output envelopes.
- `agk logs` follows a run's step logs, resuming a stream that drops without printing a line twice.
- `agk status` shows how a run stands.
- These four refuse an installation address that would carry `AGENTIIK_TOKEN` in plaintext.
- `agk run --local` ends a stopped `fail_fast` or `merge: first` sibling `cancelled`, as a server does: a `fail_fast` step hands out no further shard once one has failed.
- `agk validate` and `agk run --local` refuse a name longer than 255 characters.

### Tests

- The PostgreSQL, NATS and real-daemon tests run in CI, the remapped-daemon ones in a job of their own, and `internal/dbtest` gives each test its own database and role.
- Boundary tests for `driver`, `bus` and `agk-runner`, which links no database, controller, API or secret store and opens no inbound port.
- `internal/stoptest` plays one history through `agk run --local` and the controller, which must end it the same way.
- The real-daemon tests hold `network: internal` to what the kernel does.
- The runner image is built for both architectures and held to the floors.
- `e2e` stands up a test installation from the checkout: PostgreSQL, NATS, the API behind TLS, the controller, a registry and two runners on `docker:dind`. A runner killed mid-task is recovered by the other, and v0.1.0's milestone workflow gives the same envelopes there as under `agk run --local`. It runs under `AGENTIIK_E2E=1`.
- `cmd/agk/internal/diff` is now `internal/diff`.

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
