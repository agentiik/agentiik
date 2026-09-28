-- The workflow is a repository: "Objects are packfiles in the object store; refs live in
-- PostgreSQL." Its refs are rows here, so that moving one is an ordinary transaction beside every
-- other piece of state, and its packs are objects under a key of its own, which the packs of this
-- file record.
--
-- A workflow that agk push sent as trees, from v0.2 or v0.3, becomes an empty repository: its
-- default branch unborn until its first git push, and every version it holds kept as it was, runs
-- and replays included. So no version, run or artifact is rewritten here: workflow_versions, runs
-- and artifact_objects keep every row as it was, and what workflow_versions gains is a column whose
-- value for the rows already there is a constant, which PostgreSQL answers without writing a row.

-- The key a workflow's packs are kept under, <namespace>/git/<repository>/pack-<name>.pack and
-- .idx: random rather than the workflow's name, so that a rename re-keys nothing, and never
-- changed, since every pack of the repository is found under it. 32 hexadecimal digits, a UUID's
-- random bits, drawn for every workflow already here as the column is added and for each one
-- created after. Unique within a namespace, which is what an object key is scoped to.
alter table workflows
  add column repository text not null default replace(gen_random_uuid()::text, '-', '')
    constraint workflows_repository check (repository ~ '^[0-9a-f]{32}$'),
  add constraint workflows_repository_key unique (namespace, repository);

create function workflows_repository_kept() returns trigger
  language plpgsql
  as $$
begin
  if new.repository is distinct from old.repository then
    raise exception 'the repository of workflow %/% is kept under %, and a repository key is never changed: its packs are found under it',
      old.namespace, old.name, old.repository
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger workflows_repository_kept before update of repository on workflows
  for each row execute function workflows_repository_kept();

-- Who created the repository, by POST /api/v1/{ns}/workflows. Null for a workflow from before
-- v0.4.0, which its first agk push created without saying who it was, and for one a tree push
-- creates, whose first version's author is who pushed it.
alter table workflows add column created_by text check (created_by <> '');

-- A ref name as git writes it in full, and as the wire's refName holds it: refs/heads/<branch> or
-- refs/tags/<tag>, the only two kinds a workflow repository holds, the rest on git
-- check-ref-format's rules. And at most 1,024 bytes, a bound git itself does not set: a ref is a
-- key of the index below, which holds a row to some 2,700 bytes, and git keeps a ref it writes as a
-- file under .git, whose path macOS holds to 1,024, so that no ref a clone can check out is past it.
create function git_ref_name(ref text) returns boolean
  language sql immutable
  as $$
    select ref ~ '^refs/(heads|tags)/[^\x01-\x20\x7f~^:?*\[\\]+$'
       and ref !~ '/[./]|\.\.|@\{|[/.]$|\.lock(/|$)'
       and octet_length(ref) <= 1024
  $$;

-- One row per branch and tag. HEAD is the default branch, which workflows.default_branch names,
-- and is never a row. commit is null only while a branch is unborn, which is what the default
-- branch of a repository nothing was pushed to is: its row is there before the branch is, so that
-- whether pushing to it takes grant:manage is decided before the first push rather than by it.
-- Deleting the default branch leaves it unborn again, protection kept.
--
-- commit is the commit the ref names, an annotated tag peeled to it, as the wire answers a ref;
-- tag is the annotated tag object itself, which the ref names in git and a fetch is answered with.
-- A ref moves by compare and swap on what git names, coalesce(tag, commit).
create table workflow_refs (
  namespace text not null,
  workflow  identifier not null,
  ref       text not null constraint workflow_refs_ref check (git_ref_name(ref)),
  commit    text check (commit ~ '^[0-9a-f]{40}$'),
  tag       text check (tag ~ '^[0-9a-f]{40}$'),
  protected boolean not null default false,
  moved_by  text check (moved_by <> ''),
  moved_at  timestamptz,
  primary key (namespace, workflow, ref),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade,
  -- Unborn is a branch nobody has moved: a commit, who moved it and when are there together or not
  -- at all.
  constraint workflow_refs_unborn check (
    (commit is null) = (moved_by is null) and (commit is null) = (moved_at is null)
    and (commit is not null or ref like 'refs/heads/%')),
  constraint workflow_refs_tag check (tag is null or (commit is not null and ref like 'refs/tags/%')),
  -- Only the default branch is protected, and only a branch can be it.
  constraint workflow_refs_protected check (not protected or ref like 'refs/heads/%')
);

-- The packs of each repository, recorded before a byte of one is written. receiving is a pack a
-- push is writing, which becomes live in the transaction that moves its refs, or is collected once
-- it has been receiving past the grace; live is one a fetch reads; superseded is one a repack has
-- replaced, which a fetch that listed it before may still be reading, and which is deleted a grace
-- after superseded_at. A pack is named by its checksum, and holds every object whole, so that no
-- pack depends on another.
--
-- A pack names its repository by key, and a workflow is not deleted while one does: removing a
-- repository deletes its packs from the store and then their rows, so that no file under git/ is
-- ever left with nothing naming it, which the orphan sweep, walking sha256/ alone, would never
-- find.
create table git_packs (
  namespace     text not null,
  repository    text not null,
  name          text not null check (name ~ '^[0-9a-f]{40}$'),
  size          bigint not null check (size > 0),
  objects       integer not null check (objects >= 0),
  state         text not null default 'receiving' check (state in ('receiving', 'live', 'superseded')),
  created_at    timestamptz not null default now(),
  superseded_at timestamptz,
  primary key (namespace, repository, name),
  constraint git_packs_repository foreign key (namespace, repository) references workflows (namespace, repository),
  constraint git_packs_superseded check ((state = 'superseded') = (superseded_at is not null))
);

do $$
declare t text;
begin
  foreach t in array array['workflow_refs', 'git_packs']
  loop
    execute format('alter table %I enable row level security', t);
    execute format('alter table %I force row level security', t);
    execute format($f$
      create policy %I_by_namespace on %I
        using (namespace = agentiik_namespace() or agentiik_installation())
        with check (namespace = agentiik_namespace() or agentiik_installation())
    $f$, t, t);
  end loop;
end
$$;

-- How a version arrived: git for a commit a git push carried, whose objects are in the
-- repository's packs, and tree for one agk push sent as a tree, whose files alone are stored.
-- Every version here was sent as a tree, and the constant default says so without writing a row;
-- one recorded from now on says which it is, git where it does not.
alter table workflow_versions add column source text not null default 'tree'
  constraint workflow_versions_source check (source in ('git', 'tree'));
alter table workflow_versions alter column source set default 'git';

-- Every workflow here is an empty repository whose default branch is unborn, and unprotected, so
-- that an editor's agk push keeps landing after the upgrade; whoever holds grant:manage on it turns
-- protection on. A default branch that is no ref name git could push, which only an API call
-- written by hand could have given a v0.2 or v0.3 push, is given no row: no push can ever create
-- it, and the branch is unborn and unprotected with or without one. Written across every
-- namespace, which is what the installation scope is bound for.
select set_config('agentiik.scope', 'installation', true);
insert into workflow_refs (namespace, workflow, ref, protected)
select namespace, name, 'refs/heads/' || default_branch, false
  from workflows
 where git_ref_name('refs/heads/' || default_branch);
