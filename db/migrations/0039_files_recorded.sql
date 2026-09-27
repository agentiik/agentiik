-- Whether the artifact files of a run are recorded.
--
-- v0.2 recorded a row of artifacts only for an output its workflow gave a retain, so the files of
-- every other output, what travelled between two steps above all, are named by the envelopes their
-- steps published and by no row: no count holds them, nothing expires them and the collection never
-- reaches them. A controller of this release records every file its run's steps have published at
-- every decision, and says so here in the same statement; so a run that has finished and does not
-- say so was finished by a controller that did not: v0.2's, before the upgrade, or one still
-- deciding in the moments an upgrade takes to replace it. A mark taken once, as the runs finished
-- when this migration ran, would miss those last ones.
--
-- init, migrate and the controller that leads read the envelopes of those runs from the store and
-- record each file they name as an artifact of its run, expiring the namespace's
-- max_retention_days after the run finished, as migration 0038 dates the run's envelopes, then set
-- the column, in one transaction a batch of runs, which is what makes the recording idempotent. A
-- migration cannot do it itself, since the envelopes are in the object store and not here. A run
-- still going is decided by a controller of this release, which records its files, those published
-- before the upgrade included, at its next decision.
--
-- While a namespace has such a run, the collection takes no file of its objects' directory that no
-- row names: that file may be one of the files this run is waiting to have recorded.
--
-- A constant default, so that no row of runs is written by this migration.
alter table runs add column files_recorded boolean not null default false;

create index runs_files_unrecorded on runs (namespace, id)
  where finished_at is not null and not files_recorded;
