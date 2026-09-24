-- A drain and a revocation as an administrator orders them, POST /api/v1/runners/{runner}/drain and
-- /revoke: who asked and when, beside the reason drain_reason already keeps, and for a revocation
-- the end of its grace.
--
-- "Revoking a credential never destroys work already done." A revoked runner is still answered at
-- the heartbeat, told to drain, until the revocation plus revocation_grace, and its results are
-- taken until then, so the grace is written on the row as an instant rather than worked out from a
-- setting at every request: an installation that changes the setting afterwards does not move the
-- end of a grace already given, and the heartbeat answers the instant the credential is judged by.
--
-- Who asked is kept here until the audit log exists, #160, which records both orders once it does.
-- No installation holds a runner yet, as 0021 says, so a revoked row that could not say when it was
-- revoked cannot exist, and the rule is made a constraint rather than filled in.

alter table runners
  -- The drain order: who gave it, and when. Kept after a revocation, since a runner drained and
  -- then revoked was ordered twice, by perhaps two people.
  add column drained_by text check (drained_by <> ''),
  add column drained_at timestamptz,
  add constraint runners_drained_whole
    check ((drained_by is null) = (drained_at is null)),

  -- The revocation: who gave it, when, and the end of its grace, the instant after which every
  -- call the runner makes is refused and the sweep declares lost whatever it still holds.
  add column revoked_by text check (revoked_by <> ''),
  add column revoked_at timestamptz,
  add column results_accepted_until timestamptz,
  add constraint runners_revoked_whole
    check ((state = 'revoked') = (revoked_at is not null)
       and (revoked_at is null) = (revoked_by is null)
       and (revoked_at is null) = (results_accepted_until is null)),
  add constraint runners_grace_after_revocation
    check (results_accepted_until > revoked_at);
