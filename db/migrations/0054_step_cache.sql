-- Memoisation: "With cache: true, a hit republishes the same envelopes without starting a
-- container. It never crosses a namespace boundary."
--
-- One row per key a cached step's task succeeded under, naming what it published: each port's
-- envelope by the digest it is stored under, with its count of items and its size, and the run
-- that published it. The envelopes themselves are the store's objects, counted by that run as
-- every envelope of it is, so an entry holds nothing up: "a cache entry pointing at an expired
-- artifact is not a hit", and it is asked whether each object it names is still there when it is
-- found, rather than kept alive for a lookup that may never come.
--
-- The entry goes with the run that published it: with its row, when the workflow's runs are
-- purged, and with its envelopes, when their retention runs out, since past that nothing it names
-- is there to republish. A key published again replaces the entry, the latest run's envelopes
-- being the ones the longest kept.
create table step_cache (
  namespace  text not null,
  key        text not null check (octet_length(key) <= 512),
  run_id     ulid not null,
  step       name not null,
  -- [{port, digest, items, size}], one per port the step published, the digest written
  -- sha256:<hex> as the envelopes of a run's document are.
  ports      jsonb not null check (jsonb_typeof(ports) = 'array'),
  created_at timestamptz not null default now(),
  primary key (namespace, key),
  foreign key (namespace, run_id) references runs (namespace, id) on delete cascade
);

create index step_cache_run on step_cache (namespace, run_id);

alter table step_cache enable row level security;
alter table step_cache force row level security;
create policy step_cache_by_namespace on step_cache
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- A task a hit ended rather than a container: the run whose task published what it republished,
-- which the run detail names, so that a person reading a step that took no time and ran on no
-- runner is told why and where its outputs were made.
alter table tasks add column memoised_from ulid;
