-- The runs v0.2 finished whose artifact files are still to be recorded.
--
-- v0.2 recorded a row of artifacts only for an output its workflow gave a retain, so the files of
-- every other output, what travelled between two steps above all, are named by the envelopes their
-- steps published and by no row: no count holds them, nothing expires them and the collection never
-- reaches them. init and migrate read those envelopes from the store and record each file they name
-- as an artifact of its run, expiring the namespace's max_retention_days after the run finished, as
-- migration 0038 dates the run's envelopes, so that the purges expire and collect them in time. A
-- migration cannot do it itself, since the envelopes are in the object store and not here.
--
-- One row a run, taken away in the transaction that records its files, which is what makes the
-- recording idempotent and lets it go a batch of runs at a time: a run is recorded whole or not at
-- all, and a recording cut short leaves the runs it did not reach for the next init. The runs are
-- those that had finished when this migration ran, and no other: a run finishing from then on is
-- decided by a controller that records every file its steps publish, and so is one still running,
-- whose files the controller records at its next decision, those published before it included.
--
-- While a namespace has a run here, the collection takes no file of its objects' directory that no
-- row names: that file may be one of the files this run is waiting to have recorded.
create table artifacts_unrecorded (
  namespace text not null,
  run_id    ulid not null,
  primary key (namespace, run_id),
  foreign key (namespace, run_id) references runs (namespace, id) on delete cascade
);

alter table artifacts_unrecorded enable row level security;
alter table artifacts_unrecorded force row level security;
create policy artifacts_unrecorded_by_namespace on artifacts_unrecorded
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- Written across every namespace, which is what the installation scope is bound for.
select set_config('agentiik.scope', 'installation', true);
insert into artifacts_unrecorded (namespace, run_id)
select namespace, id from runs where finished_at is not null;
