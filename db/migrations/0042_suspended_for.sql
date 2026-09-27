-- Why an account is suspended, where the authentication policy suspended it. New in v0.3.0, and
-- nothing to carry over at the upgrade: nothing suspended an account before it.
--
-- Forbidding passwords suspends every account that holds no passkey the policy accepts, rather than
-- leaving it reachable by a password the policy says no longer exists, and enrolling a passkey from
-- an enrolment link or a recovery code is how such an account comes back: the enrolment lifts the
-- suspension, and only that one. A suspension written for another reason, or for none, is not the
-- enrolment's to lift, since a passkey answers the one reason and no other; so the reason is kept
-- beside the flag rather than read off the credentials, which change after it was written.
--
-- no_passkey is the one reason there is. Absent on an account not suspended, and on one suspended
-- for no reason recorded here.
alter table users
  add column suspended_for text
    constraint users_suspended_for check (suspended_for in ('no_passkey')),
  add constraint users_suspended_for_suspended check (suspended_for is null or suspended);
