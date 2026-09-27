-- The passkey ceremonies: the handle a user's passkeys are registered under, and the challenges
-- the options issue and a verification takes once.
--
-- New in v0.3.0, and nothing to carry over at the upgrade: no user held a passkey before it.

-- The user handle, WebAuthn's user.id: 32 random bytes the API mints the first time a
-- registration is started for the user, and never changes after, since an authenticator keeps it
-- beside every passkey registered under it and hands it back at every assertion. Random rather
-- than the login, because WebAuthn asks that it name nobody: an authenticator shows it to nobody,
-- but keeps it where the person does not choose. Unique, since it is what an assertion names the
-- account by. Absent until a registration is started.
alter table users
  add column webauthn_handle bytea unique check (octet_length(webauthn_handle) = 32);

-- A challenge the options issued, single use and good for five minutes: the time a person takes
-- to find an authenticator and unlock it, and short enough that a challenge copied off a screen is
-- dead by the time anybody could use it. A verification takes it, and a challenge taken or past its
-- minutes answers nothing, which is what stops a ceremony's answer being replayed.
--
-- An assertion names nobody, since the passkey the authenticator offers names the account. A
-- registration names the user it enrols, and the enrolment code that let it start where no
-- session did, so that the verification spends that code and no other. The code is held here and
-- not spent: a ceremony dismissed halfway does not burn the link.
--
-- The installation's rather than a namespace's: a ceremony is how somebody proves who they are,
-- before any namespace is in question.
create table webauthn_challenges (
  challenge      bytea primary key check (octet_length(challenge) = 32),
  ceremony       text not null check (ceremony in ('registration', 'assertion')),
  login          text references users (login) on delete cascade,
  enrolment_code bytea,
  issued_at      timestamptz not null,
  expires_at     timestamptz not null,
  constraint webauthn_challenges_expiry
    check (expires_at > issued_at and expires_at <= issued_at + interval '5 minutes'),
  constraint webauthn_challenges_registration_names_its_user
    check ((ceremony = 'registration') = (login is not null)),
  constraint webauthn_challenges_code_registers
    check (enrolment_code is null or ceremony = 'registration'),
  -- The code is the user's own, and goes with it.
  foreign key (enrolment_code, login) references enrolment_codes (hash, login) on delete cascade
);

-- What issuing a challenge removes: those past their minutes, which nothing takes any more.
create index webauthn_challenges_by_expiry on webauthn_challenges (expires_at);
