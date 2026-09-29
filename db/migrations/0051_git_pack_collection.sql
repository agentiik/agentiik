-- A repository's packs are collected, and repacked: "Repack and garbage-collect a repository's
-- packfiles without interrupting a clone in progress."
--
-- collecting is a pack the collection has claimed, a receiving one a push never made live or a
-- superseded one a repack replaced, each past the grace, whose files it is deleting from the store
-- before it deletes the row: the files first, so that no file under git/ is ever left with nothing
-- naming it, which the orphan sweep, walking sha256/ alone, would never find. A claim is a state
-- rather than a lock held across the store's deletions, so that a pass that dies between the two
-- leaves rows the next pass finds and finishes, and so that a push sending the same pack again, by
-- the checksum that names it, is refused rather than handed a row whose files are going.
--
-- A collecting pack keeps when it was superseded, where it was, so that a claim tells which of the
-- two it was: superseded_at is set exactly where the pack was superseded, and receiving and live
-- carry none.
alter table git_packs drop constraint git_packs_state_check;
alter table git_packs add constraint git_packs_state_check
  check (state in ('receiving', 'live', 'superseded', 'collecting'));
alter table git_packs drop constraint git_packs_superseded;
alter table git_packs add constraint git_packs_superseded
  check ((state <> 'superseded' or superseded_at is not null)
     and (state not in ('receiving', 'live') or superseded_at is null));

-- What the collection and the repack look for across the installation: packs that are not live, few
-- beside the live ones, and the live ones counted by repository.
create index git_packs_state on git_packs (state, namespace, repository);
