-- The dispatches of one step, in the order they were made.
--
-- GET /api/v1/runs/{id}/steps/{step}/logs reads them each time the log it follows moves on, and
-- again on every sweep, for as long as somebody reads it. Without this the only way to a step's
-- tasks is the primary key, which is every task of the namespace, so the cost of an open stream
-- would grow with everything the namespace ever ran rather than with the step it follows.
create index tasks_by_step on tasks (namespace, run_id, step, id);
