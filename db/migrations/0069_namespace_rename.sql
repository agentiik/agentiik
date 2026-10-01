-- A namespace renamed, and given a picture: "a shared namespace is renamed by its owner, whoever holds
-- grant:manage at its scope, or by an administrator ... The rename is one transaction and carries
-- everything at once", and "a namespace's picture, held to a user's photo's rules". New in v0.6.0,
-- with nothing to carry over at the upgrade: every namespace keeps its name, holds no former one, is
-- stored under the name it has, and has no picture.
--
-- The storage name is the name a namespace was created with, which its objects are kept under, the
-- first segment of every key, and its secrets' values and its webhooks' secrets are sealed under: "the
-- storage name never changes, so a rename moves no byte however much a namespace holds, and a value
-- sealed in it still opens". It is written by the insert where the insert names none, and refused
-- every change after. It is unique, and never another namespace's name: it is always the namespace's
-- own name or one of its former names, and those are held against every other namespace and login
-- while it lives.
--
-- The former names are the names a namespace held before a rename, in the order it left them, each
-- still the namespace's: every address naming one reaches it, and no other namespace and no login
-- takes one until the namespace is removed. They are kept on the namespace's row rather than in a
-- table of their own, so that renaming is one statement on one row, the name and the names it held
-- changing together, and so that the triggers below, which run inside that statement's cascades,
-- read the rename as already made; the names are held unique across rows by the trigger that holds
-- logins and namespace names apart, under the lock it takes. The namespace's own name is never among
-- them: a rename back to one takes it out.
--
-- The picture is the PNG the API re-encoded, at most 512 by 512, kept in the row as a user's photo is
-- (migration 0066), with when it was set, null with it.
create function agentiik_given_names(names text[]) returns boolean
  language sql immutable
  as $$ select coalesce(bool_and(agentiik_given_name(n)), true) from unnest(names) n $$;

alter table namespaces
  add column storage           text,
  add column former_names      text[] not null default '{}',
  add column avatar            bytea check (octet_length(avatar) between 1 and 2097152),
  add column avatar_updated_at timestamptz,
  add constraint namespaces_avatar_updated_with_it check ((avatar is null) = (avatar_updated_at is null)),
  add constraint namespaces_former_names_named check (
    agentiik_given_names(former_names)
    and array_position(former_names, null) is null
    and array_position(former_names, name) is null);

-- The storage name is held to the grammar the name is, and to no bound the name is not held to: a
-- namespace v0.2 made may carry a longer name than one made since, and is stored under it.
update namespaces set storage = name;
alter table namespaces
  alter column storage set not null,
  add constraint namespaces_storage_named check (storage ~ '^[a-z0-9]+(-[a-z0-9]+)*$');
create unique index namespaces_storage on namespaces (storage);
-- Asked of every address naming a namespace, as former_names @> array[name].
create index namespaces_former_names on namespaces using gin (former_names);

-- The storage name, written once: the name the insert gives, where it names no other, and never
-- changed after, as a workflow's repository key is never changed (migration 0051).
create function agentiik_namespace_storage() returns trigger
  language plpgsql
  as $$
begin
  if tg_op = 'INSERT' then
    new.storage := coalesce(new.storage, new.name);
  elsif new.storage is distinct from old.storage then
    raise exception 'namespace % is stored under %, and a storage name is never changed: its objects are kept under it and its sealed values bound to it',
      old.name, old.storage
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger namespaces_storage_kept before insert or update of storage on namespaces
  for each row execute function agentiik_namespace_storage();

-- Logins and namespace names share one name space, the former names among them: a login is refused
-- where a namespace holds it, as its name or a former one; a namespace created is refused a login, or
-- another namespace's former name; and a namespace renamed is refused the same, its own former names
-- excepted, which it takes back. A personal namespace's name is its user's login, which never
-- changes, so it is never renamed. The lock is the one migration 0032 takes, so that a login and a
-- namespace written at once are checked one after the other.
create or replace function agentiik_names_are_shared() returns trigger
  language plpgsql
  as $$
begin
  perform pg_advisory_xact_lock(474080961907);
  if tg_table_name = 'users' then
    if exists (select from namespaces where name = new.login or former_names @> array[new.login]) then
      raise exception 'the login % is the name of a namespace, or one a namespace held before it was renamed, and logins and namespaces share one name space', new.login
        using errcode = 'unique_violation', constraint = 'logins_and_namespaces';
    end if;
    return new;
  end if;
  if tg_op = 'UPDATE' then
    if new.name = old.name then
      return new;
    end if;
    if old.kind = 'personal' then
      raise exception 'namespace % is the personal namespace of the user %, named after their login, which never changes', old.name, old.name
        using errcode = 'check_violation', constraint = 'personal_namespaces_keep_their_name';
    end if;
  end if;
  if exists (select from users where login = new.name)
     and not (new.kind = 'personal' and new.owner is not distinct from new.name) then
    raise exception 'the namespace % is the login of a user, and logins and namespaces share one name space', new.name
      using errcode = 'unique_violation', constraint = 'logins_and_namespaces';
  end if;
  if exists (select from namespaces n
              where n.former_names @> array[new.name]
                and (tg_op = 'INSERT' or n.name <> old.name)) then
    raise exception 'the namespace % is a name another namespace held before it was renamed, and a name a namespace held stays its own until it is removed', new.name
      using errcode = 'unique_violation', constraint = 'namespace_former_names';
  end if;
  return new;
end
$$;

create trigger namespaces_share_names_when_renamed before update of name on namespaces
  for each row execute function agentiik_names_are_shared();

-- A sealed value stays where it was sealed, and a rename is no write: the row follows its namespace
-- to its new name, which the namespace now holds its old one as a former name beside, and the value
-- still opens, since it is sealed under the storage name, which a rename leaves alone. Any other
-- change of its namespace or its name is refused as before.
create or replace function secret_values_never_go_back() returns trigger
  language plpgsql
  as $$
begin
  if new.name <> old.name
     or (new.namespace <> old.namespace
         and not exists (select from namespaces
                          where name = new.namespace and former_names @> array[old.namespace])) then
    raise exception 'the value of % in % stays where it was sealed, and a write never moves it to % in %',
      old.name, old.namespace, new.name, new.namespace;
  end if;
  if new.version < old.version then
    raise exception 'the value of % in % is at version % and a write never takes it back to %',
      old.name, old.namespace, old.version, new.version;
  end if;
  if old.master is null and new.master is not null and new.version = old.version then
    raise exception 'the value of % in % holds nothing at version %, and only a write at a later version gives it one',
      old.name, old.namespace, old.version;
  end if;
  return new;
end
$$;

-- Every key naming a namespace, or a workflow or a move by its namespace, carries a rename through,
-- so that the rename is one statement on the namespace's row and no row is left naming the old
-- name. The keys onto workflows that migration 0053 made carry a workflow's rename carry this one
-- too; these are the rest: the namespace's own tables, and a move's two, which a rename never meets
-- since it waits for every move to be carried out, and which would otherwise refuse it. Found by what
-- they reference, as 0053 finds them, and each made again with ON UPDATE CASCADE beside what it
-- already carried. A service account's principal is generated from its namespace and its name, and
-- the principal it refers to is written under the new name before the rename, by the transaction
-- that makes it.
do $$
declare k record;
begin
  for k in
    select c.conrelid::regclass as tbl, c.conname, pg_get_constraintdef(c.oid) as def
    from pg_constraint c
    where c.contype = 'f'
      and c.confrelid in ('namespaces'::regclass, 'workflows'::regclass, 'workflow_moves'::regclass)
      and pg_get_constraintdef(c.oid) not like '%ON UPDATE%'
  loop
    execute format('alter table %s drop constraint %I', k.tbl, k.conname);
    execute format('alter table %s add constraint %I %s ON UPDATE CASCADE', k.tbl, k.conname, k.def);
  end loop;
end
$$;
