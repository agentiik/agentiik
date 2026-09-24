-- The fetches of a budget being served, each until when it is held.
--
-- "A fetch counts when the response completes", which is after the bytes have gone, and "a transfer
-- that is interrupted, refused, or ranged over part of the object does not consume the artifact".
-- So a fetch is held for a transfer before it begins, where nobody else can take it, and is either
-- spent when the transfer completes or given back when it does not. What is left to hand out is
-- fetches_left less what is held, and fetches_left is only ever lowered by a transfer that
-- completed, so one that did not can never retire the reference.
--
-- An instant per fetch held rather than a count, because the API holding one can die before it
-- gives it back, and a count would then keep the fetch for ever: "a client that loses its
-- connection retries against an artifact still there". Past its instant a hold is nobody's, and the
-- next transfer takes the fetch again. The API bounds every transfer so that it ends before its
-- hold does.
alter table artifacts
  add column fetches_held_until timestamptz[] not null default '{}';
