-- What one grant may be turned into.
--
-- "The runner obtains the value at the last moment, by redeeming at the API the per-task grant the
-- controller issued for that one task and that one secret." For that to mean anything the grant
-- has to carry what its one task was given, and it carried nothing: a hash and an expiry. So a
-- redemption had no way to answer "refusing anything the task does not name" except by evaluating
-- the workflow again, which is the controller's work done twice and by the wrong component.
--
-- It is written when the task is dispatched, by the only thing that knows: "the controller names
-- which secret a task may have and never sees its value". Storing it rather than recomputing it
-- also means the answer cannot drift from what was actually dispatched, which is the property that
-- matters when a run is replayed and a workflow has moved on since.
alter table task_grants
  add column scope jsonb not null default '{}';
