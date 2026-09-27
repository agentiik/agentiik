-- max_artifact_bytes: "Total live artifact storage; beyond it, new writes are refused."
--
-- What a namespace holds against it is the bytes of its live artifacts, each digest once, since
-- identical bytes are one object in a namespace: the references in artifacts neither retired nor
-- past their expiry, which one past it is not however long the next sweep takes to come round. To
-- those are added the objects
-- written and not yet referenced: an object is posted by a runner before the controller hears of
-- the result that references it, and counted only by its reference, two uploads that each fit
-- alone would both be let through, and every upload of one task too.
--
-- One row per write, held from before its bytes are read, at the most they may be, until they are
-- stored, then at their size. It lapses a quarter of an hour after the policy or the URL it was
-- written with, which expires with the task: by then the result that references it has been heard,
-- a controller's failover included, or never will be. A write that failed takes its row back. Two
-- writes of one object are two rows, so that one failing gives back its own room and not the
-- other's; the object counts once, at the most either may be.
create table artifact_uploads (
  namespace  text not null references namespaces (name) on delete cascade,
  id         ulid not null,
  digest     digest not null,
  bytes      bigint not null check (bytes >= 0),
  until      timestamptz not null,
  primary key (namespace, id)
);

create index artifact_uploads_by_digest on artifact_uploads (namespace, digest);

-- What a namespace with the quota holds, as last counted and with the room made since, and when it
-- was counted. Counting every live artifact of a namespace at every write would hold the next write,
-- and a connection of the API's, for as long as the count takes, which grows with the namespace: a
-- write adds what it may take to held under a lock on this row, and the whole count is taken again
-- only once it is a minute old, or before a write is refused on a count more than a second old. Room
-- a write gives back, or does not need once its bytes are in, is taken off at once; artifacts that
-- expired and uploads that lapsed are found at the next count, which may refuse a write in the
-- second after a count, and lets none through that the last count and the room made since leave no
-- room for.
--
-- Only a namespace that sets the quota has a row, written at its first write: a namespace that sets
-- none takes no lock and holds nothing, as before v0.3.0.
create table artifact_room (
  namespace  text not null primary key references namespaces (name) on delete cascade,
  held       bigint not null check (held >= 0),
  -- Null until the first write counts it.
  counted_at timestamptz
);

do $$
declare t text;
begin
  foreach t in array array['artifact_uploads', 'artifact_room']
  loop
    execute format('alter table %I enable row level security', t);
    execute format('alter table %I force row level security', t);
    execute format($f$
      create policy %I_by_namespace on %I
        using (namespace = agentiik_namespace() or agentiik_installation())
        with check (namespace = agentiik_namespace() or agentiik_installation())
    $f$, t, t);
  end loop;
end
$$;

-- The live references of a namespace, by digest, with what the count needs beside each, so that
-- counting them reads the index and not the table.
create index artifacts_live_by_digest on artifacts (namespace, digest) include (size_bytes, expires_at)
  where status = 'live';
