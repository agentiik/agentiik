-- The values of the built-in secret store, sealed, and nothing that would open one.
--
-- "Envelope encryption with AES-256-GCM data keys, wrapped by a master key held outside the
-- database." Every column here is a part of what that encryption produces and none of them is the
-- key: package secret seals a value before it reaches this table and opens it after it leaves,
-- under a master key read from outside the database, so "a database dump alone yields nothing". A
-- dump yields this table, and this table is ciphertext, wrapped keys, and the name of a key it
-- does not hold.
--
-- One row per secret, keeping the current write and not a history. "Rotation is a write, never a
-- read-then-write": a new value is sealed under a data key of its own and replaces the old one,
-- and a history would be every credential a namespace ever rotated away from, kept for the day
-- somebody opens it.

create table secret_values (
  namespace   text not null references namespaces (name),
  -- On the grammar and the bound of secret_declarations.name, which is the name this value is
  -- declared by, so that a value is never kept under a name no declaration could hold.
  name        text not null check (name ~ '^[A-Za-z0-9][A-Za-z0-9_-]*$' and length(name) <= 255),

  -- Which write this is, and part of what a sealed value is bound to: a ciphertext opens only in
  -- the namespace, under the name and at the version it was sealed for. It goes up by one on
  -- every write and never down, and a value forgotten keeps its count rather than its row, so
  -- that a name declared again carries on from where it was. Starting again at one would give
  -- two writes one version, and a ciphertext kept from the first would open in place of the
  -- second. Zero is a row that has never held a value.
  version     integer not null check (version >= 0),

  -- The sealed value, as secret.Sealed holds it: the master key that wrapped the data key, by
  -- its identifier, the salt the wrapping key was derived over, the data key wrapped and the
  -- nonce that wrapped it, and the value sealed and the nonce that sealed it. All of them or
  -- none: a value forgotten is a row holding none, and half a sealed value is a row nothing can
  -- open and nothing should pretend to.
  master      text,
  salt        bytea,
  wrapped_key bytea,
  wrap_nonce  bytea,
  ciphertext  bytea,
  nonce       bytea,
  check (num_nulls(master, salt, wrapped_key, wrap_nonce, ciphertext, nonce) in (0, 6)),
  check (master is null or version > 0),

  primary key (namespace, name)
);

-- A version never goes back, whatever writes the row. A write raises it, a reseal under a new
-- master key and a forgotten value keep it, and nothing lowers it: a row copied back from a
-- backup over the current one is refused here rather than opening as the credential somebody
-- rotated away from.
create function secret_values_never_go_back() returns trigger
  language plpgsql
  as $$
begin
  if new.version < old.version then
    raise exception 'the value of % in % is at version % and a write never takes it back to %',
      old.name, old.namespace, old.version, new.version;
  end if;
  return new;
end
$$;

create trigger secret_values_never_go_back before update on secret_values
  for each row execute function secret_values_never_go_back();

-- Behind the same policy as everything else a namespace owns. A ciphertext opens nowhere but
-- where it was sealed, and a namespace still has no business reading another's.
alter table secret_values enable row level security;
alter table secret_values force row level security;

create policy secret_values_by_namespace on secret_values
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
