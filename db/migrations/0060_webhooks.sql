-- Webhooks worth trusting: what each webhook checks a caller against, and the deliveries it has
-- taken, so that a captured request starts nothing a second time.
--
-- A credential belongs to one webhook of one workflow, named by the path and the method it answers,
-- and is written before or after the push that arms it: written first, the webhook refuses nothing
-- a sender signs from its first request. hmac checks the secret, sealed under the installation's
-- keyring as a built-in secret's value is, so that a dump of this table opens nothing; mtls checks the
-- SHA-256 of the one client certificate it accepts, which is no secret, since a sender presents the
-- certificate to whoever it connects to. "No role reads them back through the API, and a rotation is
-- a write": the API never answers the secret, and writing one replaces the last.
--
-- sealed is the secret as it is sealed, and version counts its writes, which the sealed value is
-- bound to, so that a ciphertext restored from a backup over a later write does not open. The row
-- goes with its workflow.
create table webhook_credentials (
  namespace   text not null,
  workflow    identifier not null,
  path        text not null check (path ~ '^(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)+$' and length(path) <= 255),
  method      text not null check (method ~ '^[A-Z]+$'),

  version     integer not null default 0 check (version >= 0),
  sealed      jsonb,
  certificate bytea check (length(certificate) = 32),

  written_by  text not null,
  written_at  timestamptz not null default now(),

  primary key (namespace, workflow, path, method),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade on update cascade,
  check (sealed is null or version > 0)
);

alter table webhook_credentials enable row level security;
alter table webhook_credentials force row level security;
create policy webhook_credentials_by_namespace on webhook_credentials
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- The deliveries a webhook took, by the identifier its sender gave each: "a signed timestamp, a
-- five-minute window and a nonce cache covering it". A request is refused past five minutes either
-- side of its timestamp, so a delivery older than that is one no request can repeat, and it is let go
-- by the next delivery to the namespace. run is the run it started, which the same delivery sent again
-- is answered with rather than a second.
create table webhook_deliveries (
  namespace text not null,
  path      text not null,
  method    text not null,
  id        text not null check (length(id) between 1 and 255),
  taken_at  timestamptz not null,
  run       text,
  primary key (namespace, path, method, id)
);

create index webhook_deliveries_taken on webhook_deliveries (namespace, taken_at);

alter table webhook_deliveries enable row level security;
alter table webhook_deliveries force row level security;
create policy webhook_deliveries_by_namespace on webhook_deliveries
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
