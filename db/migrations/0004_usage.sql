-- What one attempt cost, which a task result carries and nothing here could hold.
--
-- The result message ends with "usage": { "cpu_seconds": 12.4, "max_rss_bytes": 198443008,
-- "image_pull_ms": 0 }, and the Storage chapter's line for tasks, "Shard, attempt, state,
-- runner, idempotency key, exit code", does not reach it. It is a column rather than three
-- because it is the runner's measurement rather than the engine's vocabulary: a runner that
-- learns to measure something else should not need a migration to say so, and nothing here
-- decides anything by reading it.

alter table tasks
  add column usage jsonb not null default '{}'::jsonb;
