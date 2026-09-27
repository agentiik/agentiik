-- A TOTP generator started and not confirmed yet. New in v0.3.0, and nothing to carry over at the
-- upgrade: no generator was enrolled before it.

-- Enrolling a generator is two requests: the first mints its secret and shows it once, for the
-- person to scan or type into an authenticator application, and the second, a code the application
-- then shows, proves the secret reached it. Until then the generator is not the account's: kept in
-- credentials, it would ask every password sign-in for a code the person may never have been able
-- to make, and lock them out with a secret nobody holds. So it waits here, apart from what signs
-- anybody in, and the confirmation moves it to credentials under the identifier minted here, which
-- its sealed secret is bound to.
--
-- One per user: starting again replaces the one waiting, whose secret was shown and may have been
-- lost. Ten minutes: the time a person takes to find their phone, open the application and scan,
-- after which the secret shown is dead for this purpose and a fresh one is started. Kept in the
-- database rather than by the replica that minted it, so that the confirmation may reach another.
--
-- The installation's rather than a namespace's, as every credential is: it is how somebody proves
-- who they are, before any namespace is in question.
create table totp_enrolments (
  login       text primary key references users (login) on delete cascade,
  id          text not null check (id ~ '^[A-Za-z0-9_-]+$'),
  -- Sealed by the API under the master key, bound to the login and the identifier, as the row of
  -- credentials it becomes holds it.
  totp_sealed bytea not null check (octet_length(totp_sealed) > 0),
  started_at  timestamptz not null,
  expires_at  timestamptz not null,
  constraint totp_enrolments_expiry
    check (expires_at > started_at and expires_at <= started_at + interval '10 minutes')
);
