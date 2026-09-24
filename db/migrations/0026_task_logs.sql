-- A task's log as the API takes it from the runner holding the task, POST /api/v1/tasks/logs.
--
-- "Envelopes and logs are not stored in the database: it keeps only their digests and URIs." The
-- lines go to the object store, one object per chunk the runner shipped, and what is kept here is
-- the index to them: which chunk is where, how many lines it holds, and where the log stands. A
-- chunk says where it sits rather than relying on arrival, so the index is what tells a chunk
-- shipped twice from a chunk shipped once, and what a reader follows to put the log back in order.
--
-- One log per dispatch rather than per key. The URI names the key, agk://log/<run>/<task>, and a
-- requeue after loss keeps its key, so the dispatch that was lost and the one that replaced it can
-- both be shipping at once, each from its own seq 1: one index for the two would take the second's
-- first chunk for a redelivery of the first's. Each is kept apart under its task row, as the purge
-- already clears a log by its row.

-- Where one dispatch's log stands.
create table task_logs (
  namespace     text not null,
  task_id       ulid not null,

  -- The seq the API expects next. Chunks are taken in order and only in order, so it is one more
  -- than the last chunk taken, and a chunk past a gap is answered with it rather than kept. Once
  -- the cap is reached nothing more is written and there is no order left to keep, so a chunk
  -- then moves it past itself without a row of its own: a runner that goes on shipping cannot
  -- grow this index a row per request.
  next_seq      integer not null default 1 check (next_seq >= 1),

  -- How far into the log the runner has shipped, by its own count of lines, which is where the
  -- next chunk's first_line has to begin. It runs ahead of lines once the cap cut a chunk short.
  shipped_lines integer not null default 0 check (shipped_lines >= 0),

  -- What the API holds: the lines written and their bytes, counted where they were written so
  -- that the result message reports what the store can show.
  lines         integer not null default 0 check (lines >= 0),
  bytes         bigint not null default 0 check (bytes >= 0),
  truncated     boolean not null default false,

  -- The seq of the chunk that said final, once one has. Nothing after it is written.
  final_seq     integer check (final_seq >= 1),

  updated_at    timestamptz not null default now(),
  primary key (namespace, task_id),
  foreign key (namespace, task_id) references tasks (namespace, id) on delete cascade,
  check (final_seq is null or final_seq < next_seq)
);

-- One chunk the API took before the cap was reached, including the one the cap fell inside.
create table task_log_chunks (
  namespace  text not null,
  task_id    ulid not null,
  seq        integer not null check (seq >= 1),

  -- Where the chunk began in the runner's count, and how many lines it carried, which are what a
  -- redelivery is compared on beside its digest.
  first_line integer not null check (first_line >= 1),
  shipped    integer not null check (shipped >= 0),

  -- The digest of what was shipped, so that a chunk sent again with other lines under the same
  -- seq is refused rather than answered as the one already held.
  shipped_digest text not null check (shipped_digest ~ '^[0-9a-f]{64}$'),

  -- What was written: the lines kept, their bytes, and the object holding them with the digest
  -- of its bytes, both null where the cap kept none of them.
  lines      integer not null check (lines >= 0 and lines <= shipped),
  bytes      bigint not null check (bytes >= 0),
  object_key text check (object_key is null or object_key <> ''),
  object_digest text check (object_digest ~ '^[0-9a-f]{64}$'),

  written_at timestamptz not null default now(),
  primary key (namespace, task_id, seq),
  foreign key (namespace, task_id) references task_logs (namespace, task_id) on delete cascade,
  check ((object_key is null) = (lines = 0)),
  check ((object_key is null) = (object_digest is null))
);

-- Every object a log is written to, recorded before the write in a transaction of its own.
--
-- The index above is written in the same transaction as the chunk it names, after the object, so
-- that it never names an object that is not there. That leaves the other failure: an object
-- written and a transaction that never commits, because the API died or the request was dropped,
-- and a runner that never ships the chunk again, because it died too or its grace ended. Nothing
-- could name that object, since the store is never listed. So its key is written here first, and
-- this, not the index, is what the purge deletes by: a key recorded for a write that never
-- happened costs the purge a deletion of nothing.
create table task_log_objects (
  namespace  text not null,
  task_id    ulid not null,
  object_key text not null check (object_key <> ''),
  recorded_at timestamptz not null default now(),
  primary key (namespace, task_id, object_key),
  foreign key (namespace, task_id) references tasks (namespace, id) on delete cascade
);

alter table task_logs enable row level security;
alter table task_logs force row level security;

create policy task_logs_by_namespace on task_logs
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

alter table task_log_objects enable row level security;
alter table task_log_objects force row level security;

create policy task_log_objects_by_namespace on task_log_objects
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

alter table task_log_chunks enable row level security;
alter table task_log_chunks force row level security;

create policy task_log_chunks_by_namespace on task_log_chunks
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
