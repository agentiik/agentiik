-- An installation keeps what it ran until somebody decides otherwise: a namespace's
-- max_retention_days bounds nothing until an administrator sets it, as max_runs_per_hour,
-- max_artifact_bytes and max_run_duration bound nothing until they are set, and a workflow's
-- retain still asks for less. NULL is the namespace that sets no bound, so that the four counts
-- and this one read alike; the check stays, since a bound written is still a whole number of
-- days from one.
--
-- 90 was the default every namespace was given, and nothing records which namespaces an
-- administrator set to 90 on purpose, so every namespace at 90 is taken to be one that kept the
-- default and goes without a bound. One whose administrator meant 90 sets it again. What its runs
-- and artifacts were already given to expire by keeps its date: the date was written when they
-- were, from the bound that held then, and moving it would mean telling a workflow's own retain
-- from the namespace's, which a run does not record.

alter table namespaces alter column max_retention_days drop not null;
alter table namespaces alter column max_retention_days drop default;

update namespaces set max_retention_days = null where max_retention_days = 90;
