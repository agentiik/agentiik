-- A runner credential that rotates, as wire.schema.json writes the exchange, $defs/runnerRotation:
-- the runner presents its current credential and a signature by the key it joined with, and is
-- answered a new credential whose rotate_by is now plus the join rotation.
--
-- "The old credential keeps working until the new one is first used, so a lost answer locks nobody
-- out." So a runner holds two credentials for as long as the new one has not been presented: the
-- current one, which is the new one, and the previous one, which is what the runner still holds if
-- the answer never reached it. Each keeps its own rotate_by, because "a credential past rotate_by is
-- refused everywhere" and rotating is not a way for the old one to live longer.
--
-- No installation holds a runner yet, as 0021 says, and every runner a join creates has both a
-- credential and a rotate_by, so both are made required rather than filled in.

alter table runners
  alter column credential_hash set not null,
  alter column rotate_by set not null,

  -- The credential the runner held before its last rotation, hashed, and when it stops being
  -- accepted. Cleared the first time the new one is presented, which is when the runner has shown
  -- it holds it and the old one is worth only something to whoever else has a copy.
  add column previous_credential_hash text
    check (previous_credential_hash ~ '^[0-9a-f]{64}$'),
  add column previous_rotate_by timestamptz,
  add constraint runners_previous_whole
    check ((previous_credential_hash is null) = (previous_rotate_by is null)),

  -- The request time the runner signed in its last accepted rotation, by its own clock. A rotation
  -- is refused unless it signs a later one, so a signed request is good for one rotation and a
  -- copy of it, taken off a proxy's log or a disk, rotates nothing.
  add column rotation_signed_at timestamptz;

-- Every call a runner makes is authenticated by one of the two, so both are looked up by hash
-- rather than read through the table. Unique, because a credential opens one runner or none.
create unique index runners_by_credential on runners (credential_hash);
create unique index runners_by_previous_credential on runners (previous_credential_hash)
  where previous_credential_hash is not null;
