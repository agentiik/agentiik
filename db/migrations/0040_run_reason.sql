-- Why a run ended as it did, where nothing in its workflow is what ended it.
--
-- "Authorisation is re-evaluated when a run is created, because a trigger armed months ago can fire
-- long after the grant that armed it. From v0.3.0, a run whose principal no longer holds
-- workflow:run at that moment ends cancelled before any task, with a reason naming the grant that
-- lapsed." "The reason is on the run, where GET /api/v1/runs/{id} and agk status show it", and a run
-- had nowhere to keep one: its steps say what its workflow did, and such a run did nothing.
--
-- The controller writes it with the decision that ends the run. Null on every other run, and on
-- every run from before v0.3.0, which nothing ended that way: an upgrade has nothing to fill in.
alter table runs
  add column reason text check (reason <> '');

-- The grants revoked in one namespace, as the audit log recorded them.
--
-- A revoked grant is gone from grants, and the audit log is where what it was is kept, so the reason
-- a run is refused finds there the grant that gave its principal workflow:run before it was revoked.
-- It is asked only of a run refused, and the log holds every act of the installation, a manual run
-- started among them, so without this the question would read the whole chain each time. Partial,
-- since a revocation is a small part of what the log holds.
create index audit_log_grant_deletes on audit_log (namespace, seq) where action = 'grant.delete';
