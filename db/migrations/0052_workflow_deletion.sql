-- A workflow deleted: "answered as absent from then on, its runs still going cancelled and the data
-- of every run expiring at once; the leading controller purges the runs, then removes the versions,
-- the refs and the packs, and the name is free once that is done."
--
-- The row stays until then, since the runs, the versions and the packs it holds name it, and the
-- purge takes each of them in its turn; deleted_at is what every reader of a workflow asks first,
-- so that from the answer on nothing is served from it, and what the purge finds its work by.
-- deleted_by is who asked, which the audit log records too, kept here beside it for as long as the
-- row is, as created_by is.
alter table workflows
  add column deleted_at timestamptz,
  add column deleted_by text check (deleted_by <> ''),
  add constraint workflows_deleted check ((deleted_at is null) = (deleted_by is null));

-- The purge's work, which is a handful of rows beside every workflow there is.
create index workflows_deleted on workflows (deleted_at) where deleted_at is not null;
