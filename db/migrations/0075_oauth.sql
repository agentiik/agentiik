-- The installation as its own OAuth 2.1 authorisation server, for /mcp and each collection: "Agentiik
-- issues its own tokens", on its own sign-in, its accounts staying local. New in v0.7.0, and nothing
-- to carry over at the upgrade: no client was allowed anything before it.
--
-- An authorisation code is agk login's one-time code with what OAuth binds it to besides, "the client
-- identifier, code challenge and redirect URI" (draft-ietf-oauth-v2-1-13, section 4.1.2), and what the
-- person allowed: the client by its client_id, its name as its document gave it when they allowed it,
-- which its grant is listed under, the redirect URI the browser was sent back to, the resource the
-- token is for and the permissions allowed. It is kept beside agk login's in exchange_codes, since
-- each is a sign-in on its way to a token: taken once, lapsing within its minute, and going with its
-- user and the credential that signed in. The exchange refuses a code that names a client and the
-- token endpoint one that names none, so that neither door opens the other's.
alter table exchange_codes
  add column client_id    text check (length(client_id) between 1 and 2048),
  add column client_name  text check (length(client_name) between 1 and 256),
  add column redirect_uri text check (length(redirect_uri) between 1 and 2048),
  add column resource     text check (length(resource) between 1 and 2048),
  add column scope        permission[]
    check (cardinality(scope) > 0 and array_position(scope, null) is null),
  add constraint exchange_codes_oauth check (
    (client_id is null) = (client_name is null) and (client_id is null) = (redirect_uri is null)
    and (client_id is null) = (resource is null) and (client_id is null) = (scope is null));

-- An OAuth grant is one row of its person's tokens, "a token of the principal, narrowed to what it was
-- granted, revocable and listed as the principal's other tokens are": counted among their live ones,
-- revoked by DELETE /api/v1/auth/tokens/{id}, and gone with them. Its hash is its current access
-- token's, which each refresh replaces, and access_expires_at when that one lapses, an hour after it
-- was issued; expires_at stays the grant's end. audience is the one resource the token is accepted at,
-- its URL under the public URL, and client_id the client it was issued to, whom its refresh tokens are
-- bound to. A grant is narrowed by the permissions allowed and never by a within, which no consent
-- names, and the three columns are all there or none, since an access token of no resource would be an
-- API token nobody minted as one.
alter table api_tokens
  add column audience          text check (length(audience) between 1 and 2048),
  add column client_id         text check (length(client_id) between 1 and 2048),
  add column access_expires_at timestamptz,
  add constraint api_tokens_oauth check (
    (audience is null) = (client_id is null) and (audience is null) = (access_expires_at is null)
    and (audience is null
         or (scope_permissions is not null and scope_within is null
             and access_expires_at > created_at and access_expires_at <= expires_at)));

-- The refresh tokens of the grants, each kept as its SHA-256 and good for one use: a refresh spends
-- the one it presents and issues the next, as OAuth 2.1 has an authorisation server rotate a public
-- client's. A spent one is kept while its grant lasts, since "the previous refresh token is
-- invalidated but information about the relationship is retained by the authorization server", and
-- one presented again after it was spent ends the grant: the client and whoever copied it can no
-- longer be told apart. Those of a grant revoked or past its end are removed a few at a time by the
-- next refresh token issued, since nothing reads them any more, and all of them with the grant.
--
-- The installation's rather than a namespace's, as every credential is.
create table oauth_refresh_tokens (
  hash      bytea primary key check (octet_length(hash) = 32),
  token     ulid not null references api_tokens (id) on delete cascade,
  issued_at timestamptz not null,
  used_at   timestamptz,
  constraint oauth_refresh_tokens_use check (used_at is null or used_at >= issued_at)
);

create index oauth_refresh_tokens_by_token on oauth_refresh_tokens (token);

-- One refresh token open per grant: the refresh that spends it issues the one that replaces it, so a
-- second open beside it would be a way to the grant its client was never handed.
create unique index oauth_refresh_tokens_one_open on oauth_refresh_tokens (token) where used_at is null;
