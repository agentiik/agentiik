-- Where each secret of a namespace lives, and nothing that could hold what it is.
--
-- "Secret declarations: provider and path of each value. Never the values, which no route
-- reads." A workflow names the secrets it uses and no longer says where they are: a path written
-- in the file would let anybody able to push the workflow aim it at whatever the store holds. So
-- the namespace declares each one, through the API or agentiik_secret, and this is where the
-- declaration is kept.
--
-- One row per secret rather than one document per namespace, because a declaration is written
-- one secret at a time: two Terraform applies each rewriting the whole set would silently drop
-- each other's change, and two rows cannot.
--
-- There is no column a value could be written into, and that is the table's shape rather than a
-- convention about how it is used: a test holds the columns to this list, so that a value never
-- lands here in the clear because a column was there to take it.

create table secret_declarations (
  namespace   text not null references namespaces (name),
  -- The name a workflow writes in its secrets block and a step mounts it by, on the one
  -- grammar every name in the file is written on. Written out rather than typed with the name
  -- domain of 0001, because a column typed name is resolved to PostgreSQL's own identifier
  -- type, which pg_catalog holds and finds first, and which checks nothing.
  name        text not null check (name ~ '^[A-Za-z0-9][A-Za-z0-9_-]*$'),

  -- Which store holds the value. The three identifiers the installation knows; whether this
  -- installation has configured the one named is a question for the moment a value is read,
  -- and not a property of the declaration.
  provider    text not null check (provider in ('builtin', 'env', 'vault')),

  -- Where the value sits inside that store. The built-in store keeps a value under the
  -- namespace and the name it is declared by, so a declaration naming it carries no path: one
  -- would name something that store does not have. The other two are read at a path the
  -- declaration gives, and one without it names nothing.
  path        text check (path <> ''),
  check ((provider = 'builtin') = (path is null)),

  -- Who last declared it and when. Not the audit log, which is separate and append-only, but
  -- what somebody reading the declarations needs to know whom to ask.
  declared_by text not null,
  declared_at timestamptz not null default now(),

  primary key (namespace, name)
);

-- Behind the same policy as everything else a namespace owns. A declaration names a store and a
-- path in it, and one namespace reading another's would be one namespace learning where the
-- other keeps its credentials.
alter table secret_declarations enable row level security;
alter table secret_declarations force row level security;

create policy secret_declarations_by_namespace on secret_declarations
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
