-- The per-task grant, which is the hinge a task message turns on.
--
-- "The bearer token issued with one task, and the only thing that turns the names in a task
-- message into values. It is scoped to that task and expires with it: the message names the
-- secrets the step mounts and carries none of them, and the input URLs it names work for nobody
-- holding anything else."
--
-- Its own table rather than a column on tasks, and not for tidiness. A credential is written
-- once, read on every redemption, and revoked or expired without the row it belongs to changing;
-- a task row is rewritten on every pass of the controller. Keeping them apart means a decision
-- that rewrites a task cannot touch what authenticates it.
--
-- It is not the grants the Storage chapter names. Those are "Principal, scope (namespace or
-- workflow), role, allow or deny, expiry, who granted it and when", which is authorisation and
-- belongs to a person. This is a bearer token belonging to one attempt of one shard.

create table task_grants (
  namespace   text not null,
  -- The task's ULID, which is what the grant names inside its own text, so that the API can
  -- refuse "a redemption where the two disagree rather than believing either alone".
  task_id     ulid not null,

  -- The hash and never the value: "stored hashed, shown once at creation". A copy of this table
  -- is not a set of working grants, and a listing shows every field but the one worth stealing.
  hash        text not null check (hash ~ '^[0-9a-f]{64}$'),

  -- "It is scoped to that task and expires with it." The instant is written down rather than
  -- derived from the task's deadline, because a task whose deadline moves on a retry is a new
  -- attempt with a grant of its own, and a grant that quietly followed a deadline would be a
  -- credential whose life somebody else could extend.
  expires_at  timestamptz not null,

  created_at  timestamptz not null default now(),
  -- When it was last exchanged for values. Not a count and not a limit: a runner that lost the
  -- answer has to be able to ask again, and what bounds a grant is its expiry. It is here so
  -- that a grant nobody ever redeemed is tellable from one that was used.
  redeemed_at timestamptz,

  primary key (namespace, task_id),
  foreign key (namespace, task_id) references tasks (namespace, id) on delete cascade
);

create index task_grants_expiring on task_grants (expires_at);

alter table task_grants enable row level security;
alter table task_grants force row level security;

create policy task_grants_by_namespace on task_grants
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
