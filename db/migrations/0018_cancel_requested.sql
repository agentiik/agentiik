-- A run somebody asked to cancel.
--
-- "They share the database and nothing else. There is no remote call between them, in either
-- direction." So a cancellation is asked for the way a run is started: the API writes it on the
-- run's row and notifies, and the controller, which is what stops the tasks a run holds, reads it
-- there. The request is a column of its own rather than the run's state, because the state is the
-- controller's to write, fenced by its term and by the run's sequence. An API that wrote cancelled
-- itself would end a run without stopping anything it holds, and a decision already under way on
-- the same row would write running back over it.

alter table runs
  -- When the cancellation was first asked for, and null while nobody has. Asking again keeps the
  -- first moment, so that a client asking twice, or retrying a request whose answer it lost, has
  -- asked once.
  add column cancel_requested_at timestamptz;

-- And a run cancelled while it was queued has ended without starting. "A run that has finished
-- has started" held while nothing could end a run the controller had not let in; a request to
-- cancel now can, and is carried out before admission so that the run never is. Stamped as
-- started at the moment it was called off, it would read as a run that began and ran for no time
-- at all, and a queue wait worked out from started_at would end at a start that never happened.
alter table runs drop constraint runs_check;
alter table runs add constraint runs_check
  check (finished_at is null or started_at is not null or state = 'cancelled');
