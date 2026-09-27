-- Where the purges of a run's envelopes and of its logs stand, on the run.
--
-- Both purges find their work from the runs whose retention has run out, the oldest first. A run
-- stays once it has run out, its record being what is kept, so a purge that found its work by the
-- expiry alone would read every run it ever purged before reaching one it has not: a pass a little
-- slower each time, and a backlog of months, which an installation upgraded from v0.2 holds at its
-- first pass, worked through in a time growing with the square of its length. So each purge stamps
-- a run it is done with, and an index holds the runs it has not stamped, which is its work and
-- nothing else.
--
-- envelopes_purged_at is also what keeps one run's counts from being lowered twice: the purge takes
-- the run's row before it lowers anything and stamps it in the same transaction, so a second purge
-- at once, a controller's that has not noticed yet that it no longer leads, finds the row held or
-- stamped and passes it by. The steps keep their own stamp, which is what a reader of an envelope
-- is answered 410 by.
--
-- logs_purged_at is written once none of the run's tasks holds a log, and none can come to hold
-- one: a log is refused anything past its run's retention.
alter table runs
  add column envelopes_purged_at timestamptz,
  add column logs_purged_at timestamptz;

create index runs_envelopes_due on runs (expires_at)
  where expires_at is not null and envelopes_purged_at is null;
create index runs_logs_due on runs (expires_at)
  where expires_at is not null and logs_purged_at is null;

-- A run v0.2 finished has no expiry: v0.2 recorded none, and nothing would ever purge its envelopes
-- and its logs. It is given what its namespace's max_retention_days allows, which is what a run of a
-- workflow declaring no retain is kept for and the most any workflow could have declared: the
-- workflow's own defaults.retain was not recorded with the run, and keeping a run's envelopes longer
-- than it asked costs disk, where purging them sooner would lose what it asked to keep. Written
-- across every namespace, which is what the installation scope is bound for.
select set_config('agentiik.scope', 'installation', true);
update runs r set expires_at = r.finished_at + n.max_retention_days * interval '1 day'
from namespaces n
where n.name = r.namespace and r.finished_at is not null and r.expires_at is null;
