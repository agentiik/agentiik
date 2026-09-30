-- What fired a run, frozen on it: the trigger root, body, headers, query and scheduled_for, and an
-- event trigger's event, as expressions read them. "Freeze the trigger context on the run, so a
-- replay sees what fired it, not what is true now": the controller starts the evaluator with it, a
-- controller taking over from another reads the same, and a replay is written with its run's.
-- Null for a run nothing fired, a person's, and for every run from before v0.5.0.
alter table runs add column trigger_context jsonb check (trigger_context is null or jsonb_typeof(trigger_context) = 'object');
