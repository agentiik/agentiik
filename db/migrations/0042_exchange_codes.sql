-- agk login's one-time codes. New in v0.3.0, and nothing to carry over at the upgrade: no code was
-- minted before it.
--
-- agk login opens the sign-in page with the loopback address it listens on and the SHA-256 of a
-- verifier it keeps, PKCE's challenge (RFC 7636). A sign-in completed there mints one of these, which
-- the page hands agk at its loopback address, and agk trades it, with the verifier, for an API token
-- at POST /api/v1/auth/exchange. Kept as its SHA-256, as every credential is, and taken once: the
-- exchange removes it whatever it then answers, so that a code seen on the way is worth one
-- presentation at most, and that one worthless without the verifier.
--
-- A minute: the time a browser takes to follow a redirect to a port of its own machine, and short
-- enough that one read off a browser's history afterwards opens nothing.
--
-- The code records who signed in and with which credential, as a session records what opened it,
-- because what the exchange may mint is the policy's to say when it is asked: a code a password
-- minted where the policy has come to require a passkey the account does not hold mints no token,
-- as the session beside it enrols passkeys and nothing else from then on. Removing the user or the
-- credential removes the code with them.
--
-- The installation's rather than a namespace's, as every credential is: it is how somebody proves
-- who they are, before any namespace is in question.
create table exchange_codes (
  hash           bytea primary key check (octet_length(hash) = 32),
  login          text not null references users (login) on delete cascade,
  credential     text not null,
  -- The SHA-256 of agk login's verifier, base64url without padding, as the sign-in page was
  -- opened with it: openapi.json's codeChallenge.
  code_challenge text not null check (code_challenge ~ '^[A-Za-z0-9_-]{43}$'),
  issued_at      timestamptz not null,
  expires_at     timestamptz not null,
  constraint exchange_codes_expiry
    check (expires_at > issued_at and expires_at <= issued_at + interval '1 minute'),
  -- The credential is the user's own, and goes with it.
  foreign key (credential, login) references credentials (id, login) on delete cascade
);

-- What minting a code removes: those past their minute, which nothing takes any more.
create index exchange_codes_by_expiry on exchange_codes (expires_at);
