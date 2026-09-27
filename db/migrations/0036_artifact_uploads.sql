-- max_artifact_bytes: "Total live artifact storage; beyond it, new writes are refused."
--
-- What a namespace holds against it is the bytes of its live artifacts, each digest once, since
-- identical bytes are one object in a namespace: the references in artifacts still live and not
-- past their expiry, whether or not a sweep has retired them yet. To those are added the uploads
-- written and not yet referenced, which is what this table holds: an object is posted by a runner
-- before the controller hears of the result that references it, and counted only by its reference,
-- two uploads that each fit alone would both be let through, and every upload of one task too.
--
-- One row per object being written or waiting for its reference, held from before its bytes are
-- read, at the length of the request carrying them, until they are stored, then at their size. It
-- lapses at until, the expiry of the policy or the URL it was written with, which is the task's
-- deadline: by then the result that references it has been sent or never will be. A write that
-- failed takes its row back. Only a namespace that sets the quota writes here, so a namespace that
-- sets none holds nothing in this table and takes no lock, as before v0.3.0.
create table artifact_uploads (
  namespace  text not null references namespaces (name) on delete cascade,
  digest     digest not null,
  bytes      bigint not null check (bytes >= 0),
  until      timestamptz not null,
  primary key (namespace, digest)
);

alter table artifact_uploads enable row level security;
alter table artifact_uploads force row level security;
create policy artifact_uploads_by_namespace on artifact_uploads
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- The live references of a namespace, by digest, with what the count needs beside each, so that
-- counting them reads the index and not the table. Counted at every write to a namespace with the
-- quota, under a lock that holds the next write until the count is done.
create index artifacts_live_by_digest on artifacts (namespace, digest) include (size_bytes, expires_at)
  where status = 'live';
