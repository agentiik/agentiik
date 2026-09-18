-- The pool a runner joins, and the policy every runner in it carries.
--
-- "An administrator creates a runner pool with its labels, its accepted namespaces and its
-- resource ceilings, then issues a join token."
--
-- Until now a pool was a string on a runner row, which made it a label a machine arrived
-- carrying rather than a thing an administrator had created. Nothing held the three properties
-- the sentence above gives a pool, so nothing could enforce them, and a join token could name a
-- pool nobody had ever made.

create table runner_pools (
  name text primary key check (name ~ '^[A-Za-z0-9_-]+$'),

  -- Every label a runner in this pool may be granted, and the set a join token draws from.
  -- A label therefore reaches a machine only where somebody wrote it here first, which is
  -- the whole of "Labels are not self-asserted. A runner can only ever claim labels its join
  -- token allowed, so a machine cannot add zone=lan to itself and start receiving the steps
  -- that were kept off the internet."
  labels text[] not null default '{}',

  -- The namespaces whose tasks this pool runs. It lives here and not on the runner row
  -- because a machine that declared its own would be a machine opting itself into work it
  -- was never meant to see, and because changing the policy of a pool should reach the
  -- runners already in it rather than wait for each to be rebuilt.
  accepted_namespaces text[] not null default '{}',

  -- Ceilings over what a task asks for, "resource ceilings over what the task asked for".
  -- Null is no ceiling of that kind, which is the ordinary case for a pool of uniform hosts
  -- where the host itself is the limit. A zero would read as a ceiling of nothing.
  max_cpu          integer check (max_cpu > 0),
  max_memory_bytes bigint  check (max_memory_bytes > 0),
  max_disk_bytes   bigint  check (max_disk_bytes > 0),

  created_at timestamptz not null default now(),
  created_by text not null
);

-- A runner and a join token both name a pool that exists. A token naming a pool nobody created
-- is a token that cannot be honoured, and finding that out at the moment a machine presents it
-- is finding it out in front of the wrong person.
delete from runners where pool not in (select name from runner_pools);

alter table runners
  add constraint runners_pool_exists foreign key (pool) references runner_pools (name),
  -- A runner's accepted namespaces are its pool's. The column was filled from what the
  -- machine said about itself.
  drop column accepted_namespaces;

alter table join_tokens
  add constraint join_tokens_pool_exists foreign key (pool) references runner_pools (name);
