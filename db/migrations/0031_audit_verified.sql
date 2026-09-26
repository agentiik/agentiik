-- How far the leading controller has verified the audit log's chain in the database.
--
-- The leading controller verifies the chain at the start of each term, so that an entry changed in
-- the database after it was exported is noticed without the copy outside. Reading the whole log
-- each time would cost more with every act the installation ever recorded, so a verification starts
-- from the last entry the one before it reached, which is kept here with its hash.
--
-- The row is a hint, never a proof: the application's role writes it, so a verification checks the
-- entry it names against the hash recorded beside it and against its own fields before carrying on
-- from it, and verifies the whole log again where they disagree.
create table audit_verified (
  one      boolean primary key default true check (one),
  through  bigint not null check (through >= 0),
  hash     bytea not null check (octet_length(hash) = 32)
);

insert into audit_verified (through, hash) values (0, decode(repeat('00', 32), 'hex'));

-- It is kept, and moves only forward, whoever writes it: taken back, it would have a controller read
-- again what was verified, which costs time and proves nothing more, and removed, it would stop the
-- verification from carrying on at all. One moved forward past what was verified is not refused
-- here, since the row cannot tell, and the check of the entry it names is what finds it.
create function audit_verified_only_moves_forward() returns trigger
  language plpgsql
  as $$
begin
  if new.through < old.through then
    raise exception 'the audit log was verified through entry %, and the record never goes back to %', old.through, new.through;
  end if;
  return new;
end
$$;

create trigger audit_verified_only_moves_forward before update on audit_verified
  for each row execute function audit_verified_only_moves_forward();

create function audit_verified_is_kept() returns trigger
  language plpgsql
  as $$
begin
  raise exception 'how far the audit log was verified is kept, and only moves forward';
end
$$;

create trigger audit_verified_is_never_removed before delete on audit_verified
  for each row execute function audit_verified_is_kept();

create trigger audit_verified_is_never_truncated before truncate on audit_verified
  for each statement execute function audit_verified_is_kept();
