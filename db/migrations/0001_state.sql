-- The state the documentation puts in PostgreSQL, and nothing else.
--
-- Every table here is one the Storage chapter names, except two that a sentence of it
-- forces: namespaces, because retain "is capped by the namespace quota and cannot exceed
-- it" and the cap has to be somewhere before this group can honour it, and
-- artifact_objects, which is the other half of the row the chapter writes as one. That
-- second split is the whole of How long an artifact lives in a schema: "Expiry applies to
-- the reference, never to the object. The row in artifacts binding a run, a step and a
-- port to a digest is what is dropped; the physical sha256/<digest> object is collected
-- once its reference count reaches zero, and not before." A reference and a count of
-- references cannot be one row, so artifacts is the reference and artifact_objects is
-- what is counted.
--
-- Envelopes and logs are not here. The chapter is explicit: "Envelopes and logs are not
-- stored in the database: it keeps only their digests and URIs." A published port is a
-- digest and a count; a log is a URI, a line count and whether it was truncated.

-- --------------------------------------------------------------------------------------
-- Vocabularies
--
-- Written as domains rather than as PostgreSQL enum types. An enum is altered with a lock
-- and cannot drop a member at all, and these two lists are fixed by the documentation
-- rather than by this schema: what is wanted is a check that fails loudly when a writer
-- invents a tenth state, not a type that makes adding the tenth a migration ceremony. The
-- lists are the nine task states and the seven run states, and a test holds each against
-- the Go vocabulary in package agk so the two cannot drift.

create domain run_state as text
  check (value in ('queued', 'running', 'waiting', 'succeeded', 'failed', 'cancelled', 'timed_out'));

create domain task_state as text
  check (value in ('pending', 'dispatched', 'running', 'publishing', 'succeeded',
                   'failed', 'lost', 'timed_out', 'cancelled'));

-- What happened to one step, which is not what happened to one task and cannot be held by
-- the same list. A step whose if condition is false "moves to skipped and publishes empty
-- envelopes on all its ports", and skipped is not a state a container can be in: no task ran.
-- A task is dispatched, publishing, lost or timed out, and none of those is a thing a step
-- is either, because a step is however many attempts of however many shards it took.
--
-- Six, and they are agk.Verdict's own, which is one type for the state a step reaches and for
-- the value a downstream when reads: "a second enumeration for the reading would let the two
-- disagree over what succeeded means". A test holds this list against that one.
create domain step_verdict as text
  check (value in ('pending', 'running', 'succeeded', 'failed', 'skipped', 'cancelled'));

-- A name written in the workflow file, on the one grammar the language chapter fixes for
-- all of them: "letters, digits, hyphens and underscores, beginning with a letter or a
-- digit". A step, a port and a workflow name are all this.
create domain name as text
  check (value ~ '^[A-Za-z0-9][A-Za-z0-9_-]*$');

-- A digest as everything writes it, algorithm and hex, so that a column holding one
-- cannot hold a bare hex string that some reader would then have to guess the algorithm
-- of.
create domain digest as text
  check (value ~ '^sha256:[0-9a-f]{64}$');

-- An identifier the engine mints. The alphabet is held and the length is not, which is
-- the reading the envelope took and the wire repeated: the documentation prints run
-- identifiers of twelve and fifteen characters and mints twenty-six, and a length here
-- would refuse the documentation's own examples.
create domain ulid as text
  check (value ~ '^[0-9A-HJKMNP-TV-Z]+$');

-- --------------------------------------------------------------------------------------
-- Namespaces
--
-- The ceiling this group needs is max_retention_days, "Upper bound on what the namespace
-- may ask to keep". The rest of the row the chapter names, the kind and the allowed
-- runner pools, arrives with the group that owns namespaces; the column is here because
-- an artifact written today has to be capped today.

create table namespaces (
  name                text primary key
                      check (name ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
  max_retention_days  integer not null default 90
                      check (max_retention_days > 0),
  created_at          timestamptz not null default now()
);

-- --------------------------------------------------------------------------------------
-- Workflows and their versions

create table workflows (
  namespace      text not null references namespaces (name),
  name           name not null,
  default_branch text not null default 'main',
  labels         jsonb not null default '{}'::jsonb,
  created_at     timestamptz not null default now(),
  primary key (namespace, name)
);

create table workflow_versions (
  namespace   text not null,
  workflow    name not null,
  -- The commit, which is what a version is: "A version is a commit.
  -- finance/monthly-invoicing@a3f9c1e names exactly one tree, permanently, because that
  -- is what a commit already is."
  commit      text not null check (commit ~ '^[0-9a-f]{7,40}$'),
  parent      text check (parent ~ '^[0-9a-f]{7,40}$'),
  -- The resolved graph, which is what the evaluator produced for this commit and what a
  -- run is planned from. It is state and not payload: it names steps, images and edges.
  graph       jsonb not null,
  author      text not null,
  created_at  timestamptz not null,
  primary key (namespace, workflow, commit),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade
);

-- --------------------------------------------------------------------------------------
-- Runs, steps and tasks

create table runs (
  namespace     text not null,
  id            ulid not null,
  workflow      name not null,
  commit        text not null check (commit ~ '^[0-9a-f]{7,40}$'),
  state         run_state not null default 'queued',
  -- What started it, and who. The trigger kinds are the language's own.
  -- The seven the Triggers table names, which is three declared blocks and four ways a run
  -- begins that no block describes. api was not one of them: a run started through the API
  -- is manual, which is what manual means. mcp and terraform were missing, and the page says
  -- why they are kinds of their own rather than folded into manual: "a run that appears here
  -- is reproducible from a configuration, which is exactly what a person looking at the run
  -- list wants to know". A test holds this list against the Go vocabulary so the two cannot
  -- drift.
  trigger       text not null
                check (trigger in ('manual', 'schedule', 'webhook', 'event',
                                   'mcp', 'terraform', 'workflow')),
  triggered_by  text,
  -- The declared inputs as they were bound, and the declared outputs as digests. The
  -- outputs are digests and not envelopes for the reason the chapter gives: the database
  -- keeps the digest and the URI, and the bytes live in the object store.
  inputs        jsonb not null default '{}'::jsonb,
  outputs       jsonb not null default '{}'::jsonb,
  -- Set by a purge when an input to a step this run could restart from has expired:
  -- "the run is marked as replayable from the start only, and the console says so where
  -- it would otherwise offer the step". A marker rather than a computation, because the
  -- page says the run is marked.
  replay_from_start_only boolean not null default false,
  -- When the envelopes and logs of this run may be purged. Retention "is declared per
  -- workflow within the namespace ceiling", and an envelope is not declared one by one the
  -- way an output is, so what a run's envelopes and logs live by is the workflow's default
  -- retain, capped, resolved to an instant once. Null while the run is going: a purge acts
  -- on what has stopped changing, and a run that never finishes is the controller's to end
  -- rather than a sweep's to tidy.
  expires_at    timestamptz,
  created_at    timestamptz not null default now(),
  started_at    timestamptz,
  finished_at   timestamptz,
  primary key (namespace, id),
  foreign key (namespace, workflow, commit)
    references workflow_versions (namespace, workflow, commit),
  -- A run that has finished has started, and a run that has not started has not finished.
  check (finished_at is null or started_at is not null),
  check (started_at is null or started_at >= created_at),
  check (expires_at is null or finished_at is not null)
);

create index runs_by_state on runs (namespace, state, created_at desc);
create index runs_by_workflow on runs (namespace, workflow, created_at desc);
create index runs_expiring on runs (expires_at) where expires_at is not null;

create table steps (
  namespace   text not null,
  run_id      ulid not null,
  step        name not null,
  -- A verdict and not a task state. The chapter says "per-step state within a run", and what
  -- a step's state is is the verdict the evaluator fixes when it ends.
  state       step_verdict not null default 'pending',
  -- One entry per published output port, keyed by port name, each
  -- {"digest": "sha256:...", "size": <bytes>, "items": <count>}. This is the chapter's
  -- "envelope digests for each port", and it is a column rather than a table because a
  -- port is not addressable on its own: what is published is the step's, and a fan-out's
  -- shard envelopes are concatenated before publication.
  --
  -- The digest is a row of artifact_objects like any other. An envelope is an object in the
  -- same content-addressed store, so two steps publishing identical bytes share one object,
  -- and an envelope purge that deleted by digest alone would delete what another step still
  -- names. Counting is what makes sharing safe, and there is one counter for every kind of
  -- object rather than one per kind.
  ports       jsonb not null default '{}'::jsonb,
  -- Stamped by the envelope purge once the bytes behind those digests are gone. The
  -- digests stay: they are the record of what was published, they cost nothing, and the
  -- chapter keeps "only their digests and URIs" in the database in the first place. What
  -- the column adds is the difference between an envelope nobody has purged and one whose
  -- object has been deleted, which is what keeps a sweep from claiming the same step for
  -- ever.
  envelopes_purged_at timestamptz,
  -- The highest attempt any shard of this step reached, which is what a person reading a run
  -- wants from one number: a step whose third shard took four goes says four. It is not a
  -- count of task rows, since those are countable by counting them.
  attempts    integer not null default 0 check (attempts >= 0),
  started_at  timestamptz,
  finished_at timestamptz,
  primary key (namespace, run_id, step),
  foreign key (namespace, run_id) references runs (namespace, id) on delete cascade
);

create table tasks (
  namespace       text not null,
  id              ulid not null,
  run_id          ulid not null,
  step            name not null,
  attempt         integer not null check (attempt >= 1),
  -- Absent where the step is not fanned out, which is how one container of eight is told
  -- from the only container there is.
  shard_index     integer check (shard_index >= 1),
  shard_of        integer check (shard_of >= 1),
  state           task_state not null default 'pending',
  -- run_id/step/attempt/shard, with the shard omitted where there is none, and the shard
  -- written index and cardinality because that is what a shard is: AGK_SHARD carries 3/8
  -- and agk.NewTaskID builds the same string. The column has to equal the identifier on the
  -- wire exactly. If it did not, the uniqueness rule enforced here would be over a different
  -- string from the one "a runner refuses to start a container for a key that has already
  -- completed" compares, and each would be right about its own key while the pair let a
  -- container start twice.
  --
  -- Generated rather than written, because it restates four columns that are already here
  -- and a writer that could get it wrong eventually would.
  idempotency_key text generated always as (
    run_id || '/' || step || '/' || attempt ||
    case when shard_index is null then ''
         else '/' || shard_index || '/' || shard_of end
  ) stored,
  runner          text,
  exit_code       integer,
  -- The log as the chapter allows it to be kept: a URI, a line count and whether it was
  -- capped. Never the lines.
  log_uri         text,
  log_lines       integer check (log_lines >= 0),
  log_truncated   boolean not null default false,
  dispatched_at   timestamptz,
  started_at      timestamptz,
  finished_at     timestamptz,
  deadline        timestamptz,
  primary key (namespace, id),
  foreign key (namespace, run_id, step) references steps (namespace, run_id, step) on delete cascade,
  -- A shard is an index and a cardinality or it is neither.
  check ((shard_index is null) = (shard_of is null)),
  check (shard_index is null or shard_index <= shard_of),
  -- An exit code exists where a container ran and decided something. A task stopped at
  -- its deadline or by a cancellation has none, because nothing decided anything, and a
  -- lost task has none because nothing came back.
  check (exit_code is null or state in ('succeeded', 'failed'))
);

-- One attempt of one unit of work exists once. This is the uniqueness rule the task bus
-- chapter states, written where it is enforced rather than in a comment.
create unique index tasks_by_idempotency_key on tasks (namespace, idempotency_key);
create index tasks_by_state on tasks (namespace, state) where state in ('dispatched', 'running', 'publishing');
create index tasks_by_runner on tasks (runner) where runner is not null;
-- The log purge walks the tasks of an expired run that still name a log object, and there
-- are far more tasks than there are logs worth sweeping.
create index tasks_with_logs on tasks (namespace, run_id) where log_uri is not null;

create table approvals (
  namespace    text not null,
  id           ulid not null,
  run_id       ulid not null,
  requested_at timestamptz not null default now(),
  decided_by   text,
  decision     text check (decision in ('approved', 'rejected')),
  comment      text,
  decided_at   timestamptz,
  primary key (namespace, id),
  foreign key (namespace, run_id) references runs (namespace, id) on delete cascade,
  -- A decision has a decider and a moment, or there is no decision yet.
  check ((decision is null) = (decided_at is null)),
  check (decision is null or decided_by is not null)
);

create index approvals_waiting on approvals (namespace, requested_at) where decision is null;

-- --------------------------------------------------------------------------------------
-- Artifacts: the reference and the object
--
-- artifacts is one row per agk://run/<run>/<step>/<port>/<name>, which is what expiry
-- drops. artifact_objects is one row per (namespace, digest), which is what the reference
-- count is of and what garbage collection deletes. An envelope is counted here too: it is
-- an object in the same store, and what points at it is an entry of steps.ports rather than
-- a row of artifacts. Deduplication is scoped per namespace,
-- so two namespaces never share a row here either: "an existence check can never reveal
-- what another namespace holds".

create table artifact_objects (
  namespace   text not null references namespaces (name),
  digest      digest not null,
  size_bytes  bigint not null check (size_bytes >= 0),
  media_type  text not null,
  refs        integer not null default 0 check (refs >= 0),
  created_at  timestamptz not null default now(),
  -- Set when the count reached zero, which is what the collector sweeps for. The row
  -- outlives the object briefly so that a collection is a fact somebody can read rather
  -- than a disappearance.
  collectable_at timestamptz,
  -- Set by the collector when it takes the object, and cleared by a writer that referenced
  -- it again. Two columns rather than one because they answer two questions: collectable_at
  -- is when the count reached zero, collecting_at is when a sweep decided to act on it. A
  -- writer that finds this set knows the bytes may be going and writes them again, which is
  -- what closes the window between a sweep deleting an object and a reference arriving for
  -- it. Package db carries the whole protocol and the grace period it rests on.
  collecting_at timestamptz,
  primary key (namespace, digest)
);

create index artifact_objects_collectable on artifact_objects (collectable_at)
  where collectable_at is not null and refs = 0;

create table artifacts (
  namespace   text not null,
  run_id      ulid not null,
  step        name not null,
  port        name not null,
  name        text not null check (name <> '' and name !~ '/'),
  digest      digest not null,
  -- The size and the media type are held here as well as on the object, and the repetition
  -- is the point: a reference outlives the object it names. The object is deleted once
  -- nothing references it, while the reference is kept so that a later request answers 410
  -- and "the run detail keeps showing the artifact's name, size and digest". Both are
  -- properties of the bytes and neither can drift, since a digest that names other bytes is
  -- a different digest.
  size_bytes  bigint not null check (size_bytes >= 0),
  media_type  text not null,
  -- live, expired or collected. A reference is retired and not deleted, because a later
  -- request has to answer 410 and not 404: "so that a client can tell this existed and is
  -- finished from this never existed, and the run detail keeps showing the artifact's
  -- name, size and digest with its collection recorded".
  status      text not null default 'live'
              check (status in ('live', 'expired', 'collected')),
  -- An instant and never a duration: retain is capped by the namespace when the reference
  -- is written, and "an operator raising the ceiling does not extend artifacts already
  -- expired".
  expires_at  timestamptz not null,
  -- The fetch budget, null where there is none. Only a workflow output may carry one,
  -- which is a rule about the graph and is refused at validation; what the table holds is
  -- that a budget never goes below zero.
  fetches_left integer check (fetches_left >= 0),
  created_at  timestamptz not null default now(),
  retired_at  timestamptz,
  primary key (namespace, run_id, step, port, name),
  foreign key (namespace, run_id, step) references steps (namespace, run_id, step) on delete cascade,
  -- And no foreign key onto artifact_objects, deliberately. A key there would make the row
  -- that has to outlive the object depend on it, and collection would be refused by the
  -- very rows collection exists for.
  -- A retired reference says when, and a live one does not.
  check ((status = 'live') = (retired_at is null))
);

create index artifacts_expiring on artifacts (expires_at) where status = 'live';
create index artifacts_by_digest on artifacts (namespace, digest);

-- --------------------------------------------------------------------------------------
-- Runners
--
-- The one table of this group that is not namespaced. A runner belongs to the
-- installation and serves several namespaces: "Pulls a batch of tasks from its consumer,
-- filtered by its declared labels and by the namespaces its policy accepts", and the
-- inventory is "Administrator only". Putting it behind a namespace policy would make the
-- heartbeat, which covers every task on a host, unable to write.

create table runners (
  id                 text primary key,
  pool               text not null,
  labels             text[] not null default '{}',
  accepted_namespaces text[] not null default '{}',
  cpu                integer not null check (cpu > 0),
  memory_bytes       bigint not null check (memory_bytes > 0),
  disk_bytes         bigint not null check (disk_bytes > 0),
  architecture       text not null,
  agent_version      text not null,
  state              text not null default 'ready'
                     check (state in ('ready', 'draining', 'revoked')),
  joined_at          timestamptz not null default now(),
  last_heartbeat_at  timestamptz
);

create index runners_by_heartbeat on runners (last_heartbeat_at);

-- --------------------------------------------------------------------------------------
-- The namespace, carried on every query path
--
-- "Object keys are prefixed by namespace, and every query path carries the namespace so
-- that a missing filter fails closed rather than returning another tenant's rows."
--
-- Fails closed is stronger than is filtered, and a forgotten WHERE in Go is a full table
-- read. So the rule lives in the database: row level security keyed on a setting the
-- application binds per transaction. With nothing bound, current_setting returns null,
-- the predicate is null, and a read returns no rows: not an error, not everything,
-- nothing.
--
-- FORCE is what makes it apply to the table's owner too, since the application role will
-- own these tables on a small installation.
--
-- The second arm is the door for what legitimately has no namespace: the controller's
-- sweep, the three purges, the garbage collector and a migration. Without it those read
-- nothing at all, which would be the correctness guarantee of the whole controller
-- silently reading an empty database. It is opened by binding agentiik.scope, which one
-- named handle does and nothing else may.

create or replace function agentiik_namespace() returns text
  language sql stable
  as $$ select current_setting('agentiik.namespace', true) $$;

create or replace function agentiik_installation() returns boolean
  language sql stable
  as $$ select coalesce(current_setting('agentiik.scope', true), '') = 'installation' $$;

do $$
declare t text;
begin
  foreach t in array array['workflows', 'workflow_versions', 'runs', 'steps', 'tasks',
                           'approvals', 'artifacts', 'artifact_objects']
  loop
    execute format('alter table %I enable row level security', t);
    execute format('alter table %I force row level security', t);
    execute format($f$
      create policy %I_by_namespace on %I
        using (namespace = agentiik_namespace() or agentiik_installation())
        with check (namespace = agentiik_namespace() or agentiik_installation())
    $f$, t, t);
  end loop;
end
$$;
