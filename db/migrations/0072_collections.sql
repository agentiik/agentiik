-- Collections: "a connector of a principal's own: the workflows they choose, each one a tool, served
-- at /mcp/collections/{id}, which is the URL a client is given".
--
-- A collection is one principal's arrangement of what they may already run, and grants nothing: who
-- may call a member's tool is decided at every call by workflow:run on its workflow, never by the
-- collection holding it. So nothing here is read by an authorisation decision, and nothing here is
-- a namespace's: a collection spans the namespaces of its members and belongs to its owner.

create table collections (
  -- Minted when it is made and never changed, since the url a client is given carries it: "renaming
  -- a collection changes nothing a client is configured with".
  id             ulid primary key,

  -- The principal that made it, a user or a service account, and nobody else: a group holds no
  -- credential to call through it with, and the bootstrap token, which is nobody, owns nothing.
  -- Removed with its owner, since it is nobody else's.
  principal      text not null,
  principal_kind text not null check (principal_kind in ('user', 'service_account')),
  foreign key (principal, principal_kind) references principals (id, kind) on delete cascade,

  -- How its owner tells it from their others, unique among them.
  name           identifier not null,
  unique (principal, name),

  -- One line of at most 280 characters, the empty string where none was written: what it is for,
  -- for its owner reading the list, and nothing a client is told.
  description    text not null default ''
                 check (length(description) <= 280 and description !~ '[[:cntrl:]]'),

  created_at     timestamptz not null default now()
);

create index collections_principal on collections (principal);

create table collection_members (
  collection ulid not null references collections (id) on delete cascade,

  -- The workflow, by namespace and name, as a grant on one workflow names it. No key onto the
  -- workflow: a member whose workflow was deleted stays, offering nothing beside not_runnable, so
  -- that a tool missing from a client's list is one its owner can account for. Carried with its
  -- namespace's name where the namespace is renamed, as every key onto namespaces is, and removed
  -- with it, which only a namespace holding no workflow can be.
  namespace  text not null references namespaces (name) on delete cascade on update cascade,
  workflow   identifier not null,

  -- The branch or the tag it is read at, short or in full, and null for the default branch,
  -- whichever branch that is when the list is read. Held to git's rules by the API.
  ref        text check (length(ref) between 1 and 1024),

  -- The name its tool goes by in this collection, and null for the name its mcp block gives.
  as_name    identifier,

  -- Where it sits in the collection: a member added goes last, and one written again keeps its
  -- place, so that a client's list keeps its order as its owner changes a member.
  position   integer not null,

  primary key (collection, namespace, workflow),
  unique (collection, position)
);

-- Read through the installation's handle, for the principal whose collections they are: a
-- collection is nobody's namespace's, so a namespace's handle reads none of them. A member names a
-- namespace and is behind its policy all the same, as every row naming one is, so that a statement
-- written against it through a namespace's handle reads that namespace's rows and nobody else's.
alter table collections enable row level security;
alter table collections force row level security;
create policy collections_installation on collections
  using (agentiik_installation()) with check (agentiik_installation());

alter table collection_members enable row level security;
alter table collection_members force row level security;
create policy collection_members_by_namespace on collection_members
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- A tool call is a run, "the calling principal's, with trigger_kind: mcp and the collection it came
-- through, which the run records as collection, the collection's id and the tool it was called as,
-- and keeps once the collection is gone". So no key onto collections: the run outlives the
-- collection, and what it keeps of it is what a reader of the run is owed. Plain text held to the
-- ulid and identifier grammars by a check and a cast, as 0055 says why: a column typed as a domain
-- carrying a check has PostgreSQL rewrite every row of runs, which an upgrade leaves where it is.
alter table runs
  add column collection      text check (collection is null or collection::ulid is not null),
  add column collection_tool text check (collection_tool is null or collection_tool::identifier is not null),
  add constraint runs_called_through_a_collection
    check ((trigger = 'mcp') = (collection is not null and collection_tool is not null));
