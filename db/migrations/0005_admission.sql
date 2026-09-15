-- What queued is waiting on.
--
-- "queued: Created, waiting on a concurrency lock or on namespace quota." Two things, and
-- neither had anywhere to live: the namespace held one quota and no ceiling on how much of the
-- fleet it could take, and nothing recorded which concurrency group a run belongs to.

alter table namespaces
  -- "max_concurrent_tasks: Caps how much of the runner fleet one namespace can hold at once, so
  -- a fan-out of ten thousand items cannot starve everyone else." The default is the one the
  -- worked provider example writes, since a number has to be something and a number an
  -- operator has already read is the least surprising one.
  add column max_concurrent_tasks integer not null default 20
      check (max_concurrent_tasks > 0);

alter table runs
  -- The group this run belongs to, from the workflow's concurrency block, and null where the
  -- workflow declares none. It is stamped by the controller rather than written by whoever
  -- created the run, because it is read out of the graph and the API does not read graphs.
  --
  -- The lock itself is not a row. A group holds at most one started run at a time, so what
  -- holds it is the run that started, and asking which one is a query rather than a record to
  -- keep in step. A lock table would be a second place the truth lives, and the one thing worse
  -- than a lock nobody releases is a lock that says it is held by a run that ended.
  add column concurrency_group text
      check (concurrency_group is null or concurrency_group <> '');

-- A group is scoped to its namespace, so "one namespace can never block or cancel another's
-- runs" is the shape of the index as well as a sentence.
create index runs_by_group on runs (namespace, concurrency_group, created_at)
  where concurrency_group is not null and state in ('queued', 'running', 'waiting');
