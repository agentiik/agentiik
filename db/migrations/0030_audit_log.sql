-- The audit log: append-only, one chain across the installation, each entry carrying the hash of
-- the one before it.
--
-- "The audit log is append-only and chained. Each entry carries the hash of the previous one, so an
-- edit or a deletion leaves a break, not a silence. It is exported continuously outside the
-- installation, since the incident's own host may be unreadable."
--
-- The chain is computed here rather than by the program that writes an entry. An entry is written
-- in the transaction of the act it records, so that an act never lands unrecorded, and two acts
-- committing at once would each read the same last entry and fork the chain if each hashed on its
-- own. So an insert takes the head of the chain under a row lock, which makes appends take turns
-- until each commits, and numbers, dates and hashes the entry itself. Whatever the writer put in
-- those four columns is replaced, so no writer can choose where its entry goes in the chain or what
-- it claims to follow.
--
-- It is the installation's, not a namespace's: one chain, since a chain per namespace would leave
-- the installation's own acts, a runner revoked or a pool created, in none of them. Nothing reads it
-- through a namespace, and the controller exports it across the installation.

-- The last link: how many entries there are and the hash of the last one, which the next entry
-- follows. One row, created with the chain at zero entries and the hash of nothing, thirty-two zero
-- bytes. The application may read it and never write it: only the function below, which runs as
-- the role that migrated, moves it, and only forward.
create table audit_head (
  one  boolean primary key default true check (one),
  seq  bigint not null check (seq >= 0),
  hash bytea not null check (octet_length(hash) = 32)
);

insert into audit_head (seq, hash) values (0, decode(repeat('00', 32), 'hex'));

create table audit_log (
  -- Its place in the chain, from one and without a gap, so that a deletion in the middle is a
  -- missing number as well as a hash that no longer follows.
  seq       bigint primary key check (seq > 0),
  -- When the entry was appended, by the database's clock and under the chain's lock, so that the
  -- chain's order and its dates agree.
  at        timestamptz not null,

  -- Who acted: the principal the request was authorised as.
  actor     text not null check (actor <> ''),
  -- What was done, one of package audit's actions, such as run.trigger or secret.write.
  action    text not null check (action ~ '^[a-z][a-z_]*\.[a-z][a-z_]*$'),
  -- The namespace it was done in, and null for an act on the installation, such as a runner
  -- revoked. No reference to namespaces: an entry outlives whatever it names.
  namespace text check (namespace <> ''),
  -- What it was done to: a run, a secret's name, a runner, a pool, a join token's identifier.
  target    text not null check (target <> ''),
  -- done where the act changed something, unchanged where it was asked of something already so,
  -- a run cancelled after it ended or a runner drained twice. A refused request is not an act, and
  -- its transaction, entry and all, is rolled back.
  result    text not null check (result in ('done', 'unchanged')),
  -- The rest of what the act was, as a JSON object in the text it was written in, since what is
  -- hashed is that text and jsonb would keep another. Never a secret's value.
  detail    text not null check (jsonb_typeof(detail::jsonb) = 'object'),

  -- The hash of the entry before it, and this entry's own: SHA-256 over the previous hash and the
  -- fields above, as audit_entry_hash spells them and package audit spells them again to verify.
  prev_hash bytea not null unique check (octet_length(prev_hash) = 32),
  hash      bytea not null unique check (octet_length(hash) = 32)
);

-- One field as the hash reads it: its length in four bytes, then its bytes in UTF-8, so that no two
-- different entries are spelled alike however their fields are cut.
create function audit_field(value text) returns bytea
  language sql stable strict
  set search_path = pg_catalog
  as $$ select int4send(octet_length(convert_to(value, 'UTF8'))) || convert_to(value, 'UTF8') $$;

-- An entry's hash. The moment is written to the microsecond in UTC, which is what PostgreSQL keeps,
-- and a namespace that is null is written empty, which no namespace is.
create function audit_entry_hash(prev bytea, seq bigint, moment timestamptz, actor text, action text,
                                 namespace text, target text, result text, detail text)
  returns bytea
  language sql stable
  set search_path = pg_catalog
  as $$
    select sha256(
      convert_to('agentiik audit 1', 'UTF8') || prev || int8send(seq)
      || public.audit_field(to_char(moment at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
      || public.audit_field(actor) || public.audit_field(action)
      || public.audit_field(coalesce(namespace, '')) || public.audit_field(target)
      || public.audit_field(result) || public.audit_field(detail))
  $$;

-- Appending: the head is locked, the entry is numbered and hashed after it, and the head moves to
-- it. The lock is held until the appending transaction ends, so the next append waits and then
-- follows this one if it committed, or the head as it was if it rolled back.
--
-- SECURITY DEFINER, so that it moves a head the application may only read. Its search path is its
-- own, since a function running as another role must not resolve a name through a schema its caller
-- put first.
create function audit_log_append() returns trigger
  language plpgsql
  security definer
  set search_path = pg_catalog, public
  as $$
declare
  head record;
begin
  select seq, hash into head from public.audit_head for update;
  new.seq := head.seq + 1;
  new.at := date_trunc('microseconds', clock_timestamp());
  new.prev_hash := head.hash;
  new.hash := public.audit_entry_hash(new.prev_hash, new.seq, new.at, new.actor, new.action,
                                      new.namespace, new.target, new.result, new.detail);
  update public.audit_head set seq = new.seq, hash = new.hash;
  return new;
end
$$;

create trigger audit_log_append before insert on audit_log
  for each row execute function audit_log_append();

-- Append-only, for whatever role writes, the one that migrated included: an entry is never changed
-- and never removed. The application's role is also granted no update and no delete on the table,
-- so this is the second of two locks, and the one a role that owns the table meets too. A superuser
-- can still switch a trigger off, which is what the chain and the export are for: what it changes
-- no longer hashes as it did, and the copy outside the installation still holds what it removed.
create function audit_log_is_kept() returns trigger
  language plpgsql
  as $$
begin
  raise exception 'the audit log is append-only: an entry is never changed or removed, and a correction is a new entry';
end
$$;

create trigger audit_log_is_never_changed before update or delete on audit_log
  for each row execute function audit_log_is_kept();

create trigger audit_log_is_never_truncated before truncate on audit_log
  for each statement execute function audit_log_is_kept();

create trigger audit_head_is_never_removed before delete on audit_head
  for each row execute function audit_log_is_kept();

create trigger audit_head_is_never_truncated before truncate on audit_head
  for each statement execute function audit_log_is_kept();

-- How far the export has reached: the last entry the sink outside the installation accepted, and
-- its hash, which the next entry exported has to follow. It only moves forward, so two controllers
-- exporting at once for a moment around a change of leader send an entry twice and never lose one.
create table audit_export (
  one      boolean primary key default true check (one),
  through  bigint not null check (through >= 0),
  hash     bytea not null check (octet_length(hash) = 32)
);

insert into audit_export (through, hash) values (0, decode(repeat('00', 32), 'hex'));
