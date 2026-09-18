-- What a machine presents to become a runner, and what it holds afterwards.
--
-- "The API verifies the token, checks that the claimed labels are a subset of what the token
-- permits, refuses anything else, creates the runner record and returns a runner identifier and a
-- long-lived credential."

create table join_tokens (
  id         ulid primary key,
  pool       text not null check (pool ~ '^[A-Za-z0-9_-]+$'),
  -- The labels a runner may claim with this token, and no others. "Labels are not
  -- self-asserted. A runner can only ever claim labels its join token allowed, so a machine
  -- cannot add zone=lan to itself and start receiving the steps that were kept off the
  -- internet."
  labels     text[] not null default '{}',

  hash       text not null check (hash ~ '^[0-9a-f]{64}$'),
  issued_by  text not null,
  issued_at  timestamptz not null default now(),
  -- "One hour after it was issued is the default, and it is short because the token only has
  -- to survive the minutes between an administrator copying it and a machine presenting it."
  expires_at timestamptz not null,

  -- Single use, and spending it is writing this. "a token good for several machines is a
  -- credential worth stealing and there is nothing it would buy that a second token does not:
  -- one machine, one token, and a reimaged host joins again."
  redeemed_at timestamptz,
  redeemed_by text
);

create index join_tokens_live on join_tokens (expires_at) where redeemed_at is null;

alter table runners
  -- The credential the runner authenticates every later call with, hashed. "It is shown once,
  -- here, and stored hashed, so this answer is the only moment the value exists outside the
  -- machine that will hold it."
  add column credential_hash text check (credential_hash ~ '^[0-9a-f]{64}$'),
  -- "When this credential stops being accepted. Rotation is automatic and the agent renews
  -- well ahead of it; a runner that was offline past this instant has to join again, which is
  -- deliberate, because a machine that has been dark for a month should be reconsidered
  -- rather than readmitted."
  add column rotate_by timestamptz,
  -- Which token this machine joined with, so that a listing can say where a runner came from
  -- and an audit can follow one back to whoever issued it.
  add column joined_with ulid references join_tokens (id),
  -- Why it is draining, where somebody said. Free text, shown to the runner in its next
  -- heartbeat answer: "The runner stops accepting new tasks and drains what it holds."
  add column drain_reason text;

create index runners_by_pool on runners (pool, state);

-- The task a runner says it is holding, which is where liveness lives: "Liveness therefore lives
-- in the database beside the task state, rather than as traffic on a work queue that exists to
-- distribute work." Three missed intervals move a task to lost.
alter table tasks
  add column last_heartbeat_at timestamptz;

create index tasks_held on tasks (last_heartbeat_at)
  where state in ('dispatched', 'running', 'publishing');
