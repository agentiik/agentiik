-- Who somebody is, how they prove it, and what they may do: the principals of v0.3.0, their
-- credentials, tokens and sessions, and the grants that give them access.
--
-- Storage names the tables: users, with "no credential material", credentials, one row per
-- credential, groups and group_members, service_accounts, api_tokens, sessions, namespaces with
-- their kind, quotas and allowed runner pools, and grants. Beside them sit three the access
-- decisions force: principals, the one table a grant refers to, so that "a grant binds to any of
-- them without a special case"; the enrolment codes that bring the first administrator and a
-- recovery in; and the authentication policy and the bootstrap state, which the API reads before
-- any namespace is known. The shapes are wire.schema.json's, $defs/user to $defs/quotas.
--
-- An installation upgrading from v0.2 applies this at its next docker compose up, with its data in
-- place, so nothing here changes what is already there: every column added to namespaces is
-- nullable or defaults to what the row already meant, and no table that existed gains a
-- constraint its rows could break. The text operator, which v0.2 wrote into runs.triggered_by,
-- audit_log.actor, secret_declarations.declared_by and runner_pools.created_by, stays what it
-- was: those columns keep naming a principal by the string that names it, which is the key of
-- principals below, and operator is refused as a login, so an old row never reads as the act of
-- a user created later.

-- --------------------------------------------------------------------------------------
-- Names

-- The words the API routes on, which name no namespace, and so no login, group or service account:
-- the first path segment after /api/v1/ decides the route. agk.ReservedNamespaces is the same list,
-- and a test holds the two together.
create function agentiik_reserved(name text) returns boolean
  language sql immutable
  as $$ select name in ('auth', 'me', 'users', 'groups', 'service-accounts', 'namespaces',
                        'runners', 'runner-pools', 'bus', 'tasks', 'bricks', 'runs', 'artifacts') $$;

-- A name an administrator gives, on the grammar namespaces are named in: lowercase words joined by
-- hyphens, and at most 255 characters, the most a directory or a subject token holds.
create function agentiik_given_name(name text) returns boolean
  language sql immutable
  as $$ select name ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$' and length(name) <= 255 $$;

-- The entries of three lists, each held to its grammar as a label is: a domain rather than a check
-- on the column, because what is held is every entry of an array.
create domain permission as text
  check (value in ('workflow:read', 'workflow:run', 'workflow:write', 'workflow:delete',
                   'run:read', 'run:read_data', 'secret:use', 'secret:write', 'grant:manage'));

-- A namespace, or NS/WORKFLOW with the workflow half on the identifier grammar.
create domain grant_scope as text
  check (value ~ '^[a-z0-9]+(?:-[a-z0-9]+)*(?:/[A-Za-z0-9][A-Za-z0-9_-]*)?$');

create domain runner_pool_name as text
  check (agentiik_given_name(value));

-- --------------------------------------------------------------------------------------
-- Principals

-- One row per principal, keyed by the one string that names it in a grant, the API, agk and the
-- audit log: the login for a user, group:NAME for a group, NS/NAME for a service account. The
-- string rather than a number minted here, because every column that already names who acted
-- holds that string, and a grant written with it reads the same in the table as on the wire.
-- The three forms cannot collide: a login holds neither the colon nor the slash.
--
-- Each kind has a table of its own, which refers back here through (id, kind), so that a row
-- here is one kind of principal and never two, and removing it removes the user, group or
-- service account it is, with every credential, token, session, membership and grant it holds.
create table principals (
  id         text primary key,
  kind       text not null check (kind in ('user', 'group', 'service_account')),
  created_at timestamptz not null default now(),
  unique (id, kind),

  -- A login is also the name of its owner's personal namespace, and operator names the v0.2
  -- operator on old rows, so a user of that name would read as their author.
  constraint principals_id_check check (
    case kind
      when 'user' then
        agentiik_given_name(id) and not agentiik_reserved(id) and id <> 'operator'
      when 'group' then
        id like 'group:%' and agentiik_given_name(substr(id, 7))
        and not agentiik_reserved(substr(id, 7))
      else
        agentiik_given_name(split_part(id, '/', 1)) and agentiik_given_name(split_part(id, '/', 2))
        and id = split_part(id, '/', 1) || '/' || split_part(id, '/', 2)
        and not agentiik_reserved(split_part(id, '/', 2))
    end)
);

-- A local account. No credential material: a password hash, a passkey's public key and a TOTP
-- secret are rows of credentials, so that a listing of users carries nothing a user proves
-- themselves with, and deleting a password is deleting a row rather than blanking a column.
create table users (
  login           text primary key,
  kind            text not null default 'user' check (kind = 'user'),
  display_name    text not null check (length(display_name) between 1 and 256),
  -- A platform administrator, which grants no run:read_data anywhere.
  admin           boolean not null default false,
  -- An account that opens no session, such as one that had not enrolled a passkey when passwords
  -- were forbidden.
  suspended       boolean not null default false,
  created_at      timestamptz not null default now(),
  last_sign_in_at timestamptz,
  foreign key (login, kind) references principals (id, kind) on delete cascade
);

-- A named set of users. Grants target groups, so membership changes without touching a grant.
create table groups (
  name       text primary key,
  kind       text not null default 'group' check (kind = 'group'),
  principal  text not null generated always as ('group:' || name) stored,
  created_at timestamptz not null default now(),
  foreign key (principal, kind) references principals (id, kind) on delete cascade
);

-- Members are users and never groups: a group inside a group would turn effective permissions
-- into a walk over a graph.
create table group_members (
  group_name text not null references groups (name) on delete cascade,
  login      text not null references users (login) on delete cascade,
  added_at   timestamptz not null default now(),
  primary key (group_name, login)
);

-- What the resolver asks at every decision: which groups is this user in.
create index group_members_by_login on group_members (login);

-- A non-human principal of one namespace, holding API tokens and never signing in to a client.
--
-- The installation's rather than the namespace's, although a namespace owns it: a token names
-- its service account before anybody knows which namespace a request is about, so identifying
-- one is a read across namespaces, as a user's is. Removing the namespace is refused while one
-- remains, since a service account removed with it would leave its principal, and so its tokens,
-- naming a namespace that is gone.
create table service_accounts (
  namespace  text not null references namespaces (name),
  name       text not null,
  kind       text not null default 'service_account' check (kind = 'service_account'),
  principal  text not null generated always as (namespace || '/' || name) stored,
  created_by text not null check (created_by <> ''),
  created_at timestamptz not null default now(),
  primary key (namespace, name),
  foreign key (principal, kind) references principals (id, kind) on delete cascade
);

-- --------------------------------------------------------------------------------------
-- Credentials

-- One row per credential, of one of three types. A passkey keeps what verifying an assertion
-- needs; a password keeps its hash; a TOTP keeps its secret. Each type's columns are required on
-- its own rows and absent on the others', so that a row is one credential and says which.
create table credentials (
  -- A passkey's credential ID as the authenticator minted it, base64url without padding, which is
  -- how Web Authentication writes it and what finds the credential an assertion names. A password
  -- or a TOTP has an identifier the API mints, on the same alphabet. Unique across the
  -- installation, because a credential ID registered to one user is refused to another.
  id              text primary key check (id ~ '^[A-Za-z0-9_-]+$'),
  login           text not null references users (login) on delete cascade,
  type            text not null check (type in ('passkey', 'password', 'totp')),
  label           text check (length(label) between 1 and 256),
  created_at      timestamptz not null default now(),
  last_used_at    timestamptz,

  -- A passkey's COSE public key as the authenticator sent it, its signature counter, and its
  -- AAGUID, stored as received: all zeros under attestation conveyance none, so it names no
  -- authenticator model. The counter is a bigint because Web Authentication's is an unsigned
  -- 32-bit number, which an integer would refuse past half its range.
  public_key      bytea,
  sign_count      bigint check (sign_count between 0 and 4294967295),
  aaguid          bytea check (octet_length(aaguid) = 16),
  -- Backup Eligibility and Backup State, which record whether the passkey is synced or
  -- device-bound. State cannot be set where Eligibility is not, as Web Authentication defines
  -- the two.
  backup_eligible boolean,
  backup_state    boolean constraint credentials_backup_state check (not backup_state or backup_eligible),

  -- A password's hash, in the self-describing form its hasher writes, parameters included, so a
  -- later cost reads an older hash.
  password_hash   text check (password_hash <> ''),

  -- A TOTP's secret, sealed by the API under the master key as a builtin secret value is: a code
  -- is checked against the secret itself, so it cannot be hashed, and a dump alone opens nothing.
  totp_sealed     bytea check (octet_length(totp_sealed) > 0),

  unique (id, login),
  constraint credentials_one_type check (case type
    when 'passkey' then
      public_key is not null and sign_count is not null and aaguid is not null
      and backup_eligible is not null and backup_state is not null
      and password_hash is null and totp_sealed is null
    when 'password' then
      password_hash is not null and totp_sealed is null and public_key is null
      and sign_count is null and aaguid is null and backup_eligible is null
      and backup_state is null
    else
      totp_sealed is not null and password_hash is null and public_key is null
      and sign_count is null and aaguid is null and backup_eligible is null
      and backup_state is null
  end)
);

create index credentials_by_login on credentials (login);

-- One password and one TOTP per user at most: a second would be a second secret nobody knows
-- which of to check.
create unique index credentials_one_password on credentials (login) where type = 'password';
create unique index credentials_one_totp on credentials (login) where type = 'totp';

-- --------------------------------------------------------------------------------------
-- Tokens, sessions and enrolment codes

-- A bearer credential of a user or a service account. The value is shown once and kept as its
-- SHA-256, so that the table opens nothing to whoever reads it.
create table api_tokens (
  id             ulid primary key,
  hash           bytea not null unique check (octet_length(hash) = 32),
  principal      text not null,
  -- Never a group, which holds no credential of its own: a group's token would be a way in that
  -- belongs to nobody.
  principal_kind text not null check (principal_kind in ('user', 'service_account')),

  -- The scope, $defs/apiToken's {permissions, within}: each absent where the token is not
  -- narrowed that way, and never empty, since an empty narrowing would be read one way by half
  -- the people writing it.
  scope_permissions permission[]
    check (cardinality(scope_permissions) > 0 and array_position(scope_permissions, null) is null),
  scope_within      grant_scope[]
    check (cardinality(scope_within) > 0 and array_position(scope_within, null) is null),

  device_label   text check (length(device_label) between 1 and 256),
  created_at     timestamptz not null default now(),
  -- Every token expires, a year after it was minted at most, so that no token is a credential
  -- for good and every one is renewed by somebody who still means it.
  expires_at     timestamptz not null,
  last_used_at   timestamptz,
  revoked_at     timestamptz,
  constraint api_tokens_expiry check (expires_at > created_at and expires_at <= created_at + interval '1 year'),
  foreign key (principal, principal_kind) references principals (id, kind) on delete cascade
);

create index api_tokens_by_principal on api_tokens (principal);

-- A code that lets one user enrol a passkey and nothing else: the first administrator's, made
-- with the bootstrap token, or a recovery code an administrator issues. Kept as its SHA-256,
-- single use, and good for an hour.
create table enrolment_codes (
  hash       bytea primary key check (octet_length(hash) = 32),
  login      text not null references users (login) on delete cascade,
  kind       text not null check (kind in ('first-administrator', 'recovery')),
  -- Who issued it: an administrator's login, or operator for the bootstrap token.
  issued_by  text not null check (issued_by <> ''),
  issued_at  timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at    timestamptz,
  -- A code replaced before it was used: issuing a fresh link revokes the one before it.
  revoked_at timestamptz,
  unique (hash, login),
  constraint enrolment_codes_expiry check (expires_at > issued_at and expires_at <= issued_at + interval '1 hour'),
  constraint enrolment_codes_one_end check (used_at is null or revoked_at is null)
);

-- One code of each kind open for a user at a time, so that a link issued again leaves the one it
-- replaced unusable rather than beside it.
create unique index enrolment_codes_one_open on enrolment_codes (login, kind)
  where used_at is null and revoked_at is null;

-- A console session: an opaque identifier in a cookie, kept as its SHA-256. It records what opened
-- it, a credential or an enrolment code, so that a session that may only enrol a passkey is known
-- as one on the server and not by convention. Removing that credential ends the session with it.
create table sessions (
  hash            bytea primary key check (octet_length(hash) = 32),
  login           text not null references users (login) on delete cascade,
  credential      text,
  enrolment_code  bytea,
  created_at      timestamptz not null default now(),
  idle_expires_at timestamptz not null,
  revoked_at      timestamptz,
  constraint sessions_opened_by check ((credential is null) <> (enrolment_code is null)),
  constraint sessions_idle_expiry check (idle_expires_at > created_at),
  -- The credential or the code is the session's user's own.
  foreign key (credential, login) references credentials (id, login) on delete cascade,
  foreign key (enrolment_code, login) references enrolment_codes (hash, login) on delete cascade
);

create index sessions_by_login on sessions (login);

-- --------------------------------------------------------------------------------------
-- Namespaces, grants and policy

-- A namespace's kind and owner, and the quotas it lacked. Every row there is a namespace an
-- administrator created for whoever used it, so each is shared and owned by nobody yet: the owner
-- is a principal, and v0.2 had none. The quotas added are absent, which bounds nothing, so a
-- workflow that ran under v0.2 is refused nothing it was allowed; max_concurrent_tasks and
-- max_retention_days keep the defaults they had.
alter table namespaces
  add column kind text not null default 'shared' check (kind in ('personal', 'shared')),
  -- Who a self-granted administrator's access is reported to. Removing a principal that owns a
  -- namespace is refused, so that no namespace is left owned by somebody who is gone.
  add column owner text references principals (id),
  -- A sliding count of the runs created in the last 60 minutes.
  add column max_runs_per_hour integer check (max_runs_per_hour > 0),
  add column max_artifact_bytes bigint check (max_artifact_bytes > 0),
  -- On the grammar a timeout is written in, as the administrator wrote it.
  add column max_run_duration text check (max_run_duration ~ '^[1-9][0-9]*(ms|s|m|h|d)$'),
  -- The pools this namespace's steps may be sent to, by name. Absent allows every pool that
  -- accepts the namespace, and an empty list is refused rather than read, since a pool's own empty
  -- list of namespaces means every one.
  add column allowed_runner_pools runner_pool_name[]
    check (cardinality(allowed_runner_pools) > 0 and array_position(allowed_runner_pools, null) is null),
  -- A personal namespace is named after its owner's login, so its owner is that user: no group
  -- and no service account is written without a colon or a slash.
  add constraint namespaces_personal_owner check (kind = 'shared' or owner is not distinct from name);

-- One principal, one scope, and one role or one denied permission.
--
-- A deny names a permission rather than a role, since no role is run:read_data alone. The scope
-- is the namespace, and a workflow of it where the grant is on one, which is removed with the
-- workflow, so that a workflow created again under the same name starts with none of the old
-- one's grants. Behind the namespace's policy like every table a namespace owns: what one
-- namespace grants is not another's to read.
create table grants (
  id         ulid primary key,
  namespace  text not null references namespaces (name) on delete cascade,
  workflow   identifier,
  principal  text not null references principals (id) on delete cascade,
  role       text check (role in ('viewer', 'operator', 'editor', 'owner')),
  deny       permission,
  -- When it ends by itself. Absent lasts until revoked.
  expires_at timestamptz,
  -- Who wrote it: a principal holding grant:manage at this scope, or operator for the bootstrap
  -- token. Text and not a reference, because the record of who granted outlives them.
  granted_by text not null check (granted_by <> ''),
  granted_at timestamptz not null default now(),
  constraint grants_role_or_deny check ((role is null) <> (deny is null)),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade
);

create index grants_by_principal on grants (namespace, principal);
create index grants_across_namespaces on grants (principal);

alter table grants enable row level security;
alter table grants force row level security;

create policy grants_by_namespace on grants
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- How people prove who they are: one row for the installation, and one per namespace that
-- tightens it. On the installation's row every setting is written, starting at the documented
-- defaults, so that a later release changing a default loosens no installation that never set
-- one. On a namespace's, an absent setting is the installation's. Whether a namespace's is
-- tighter needs both rows and is the API's to say.
--
-- The installation's rather than a namespace's to read: the policy applies at sign-in, before any
-- namespace is in question, to everybody holding a grant in one.
create table auth_policy (
  namespace         text unique references namespaces (name) on delete cascade,
  password          text check (password in ('allowed', 'forbidden')),
  passkey           text check (passkey in ('optional', 'required')),
  user_verification text check (user_verification in ('required', 'preferred')),
  device_bound_only boolean,
  -- At least one, since zero would let a password go from an account with nothing to replace it.
  min_passkeys      integer check (min_passkeys >= 1),
  updated_at        timestamptz not null default now(),
  constraint auth_policy_installation_whole check (namespace is not null or (password is not null and passkey is not null and
         user_verification is not null and device_bound_only is not null and
         min_passkeys is not null))
);

create unique index auth_policy_installation on auth_policy ((namespace is null))
  where namespace is null;

insert into auth_policy (password, passkey, user_verification, device_bound_only, min_passkeys)
  values ('allowed', 'required', 'required', false, 2);

-- The bootstrap token and whether it has ended. The v0.2.5 operator token becomes the bootstrap
-- token: it works as it did until the first administrator has enrolled a passkey, and then never
-- again. One row, created with no hash, which init writes.
create table bootstrap (
  one         boolean primary key default true check (one),
  token_hash  bytea check (octet_length(token_hash) = 32),
  -- When the first administrator completed their enrolment, which ended the token.
  enrolled_at timestamptz,
  -- An ended token keeps no hash, so that nothing written afterwards, a line left in .env
  -- included, can bring it back.
  constraint bootstrap_ended_keeps_no_hash check (enrolled_at is null or token_hash is null)
);

insert into bootstrap default values;

-- Ended once and for good, whoever writes it, since a bootstrap token that could start again
-- would be a second way to make an administrator with nobody watching.
create function bootstrap_ends_once() returns trigger
  language plpgsql
  as $$
begin
  if old.enrolled_at is not null and new.enrolled_at is distinct from old.enrolled_at then
    raise exception 'the bootstrap token ended at %, when the first administrator enrolled, and it never starts again', old.enrolled_at;
  end if;
  return new;
end
$$;

create trigger bootstrap_ends_once before update on bootstrap
  for each row execute function bootstrap_ends_once();

create function bootstrap_is_kept() returns trigger
  language plpgsql
  as $$
begin
  raise exception 'the bootstrap state is kept, since a row written again would be a bootstrap token that never ended';
end
$$;

create trigger bootstrap_is_never_removed before delete on bootstrap
  for each row execute function bootstrap_is_kept();

create trigger bootstrap_is_never_truncated before truncate on bootstrap
  for each statement execute function bootstrap_is_kept();
