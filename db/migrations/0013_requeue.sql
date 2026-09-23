-- A lost task, handed out again under the key it was lost under.
--
-- "A requeue after loss keeps the idempotency key and takes a new task_id." 0001 made the key
-- unique, so a requeue could only be a new attempt under a new key: it spent a retry.max attempt
-- on a loss the brick had no part in, and a runner that had finished the task before it went quiet
-- did not recognise it when it came back. The key names the unit of work and the row names one
-- dispatch of it, so a key now has one row per dispatch: the idempotency rule is about the key,
-- and the audit trail about the task_id.

alter table tasks
  -- Which dispatch of its key this row records, counted the way the evaluator counts them in
  -- ShardState.Requeue: 0 for the first time the attempt was handed out, and one more for each
  -- time it was handed out again after a loss. It is what a decision writes a row by, because
  -- the evaluator holds no task_id and never will: a row is minted here, the key is derived, and
  -- this is the one number between them.
  add column requeue integer not null default 0 check (requeue >= 0);

-- Each dispatch of a key once.
create unique index tasks_by_dispatch on tasks (namespace, idempotency_key, requeue);

-- And the rule 0001 wrote, which is about the key and still holds: "one attempt of one unit of
-- work exists once". Every dispatch of a key but the last was lost, and a lost dispatch is over as
-- far as anyone can tell, so at most one row of a key is anything else. Two that were not lost
-- would be one unit of work handed out twice at once, which is the container starting twice that
-- the rule is there to prevent, and a writer that tried it is refused here rather than believed.
drop index tasks_by_idempotency_key;
create unique index tasks_by_idempotency_key on tasks (namespace, idempotency_key)
  where state <> 'lost';

-- What the controller reads on every pass to hear of the losses the heartbeat declared, which it
-- then decides on. There are far fewer lost tasks than tasks, and a run is read many times.
create index tasks_lost on tasks (namespace, run_id) where state = 'lost';
