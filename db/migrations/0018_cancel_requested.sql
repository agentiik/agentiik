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
