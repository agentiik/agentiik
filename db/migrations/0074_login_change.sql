-- A user given a new login: "from v0.7.0, login as well, for a user whose login a later release made
-- a reserved word ... Everything naming the user follows in one transaction", their personal namespace
-- renamed with them and keeping the old login as a former name, and the user told with a
-- login_changed notification. New in v0.7.0, with nothing to carry over at the upgrade: no login has
-- changed before, and nothing has told one.
--
-- The keys naming a user by their login carry a new login through, as migration 0069 made every key
-- naming a namespace carry a rename: those onto users, and the two that name a credential or an
-- enrolment link together with its user's login, so that a session is still one of its credential's
-- user's and a challenge one of its link's. The login is then one statement on the user's row, and no
-- credential, session, link, code, challenge or membership is left naming the old one: a session and
-- a token stay valid, since they are the user's rather than the login's. Found by what they
-- reference, as 0069 finds them, and each made again with ON UPDATE CASCADE beside what it already
-- carried. They are made again in the order they were first made: PostgreSQL checks a row's keys in
-- the order of their triggers' names, which follow the order they were made in, and where a row
-- breaks two keys at once, a session or an agk login code of a login that is no user's, the one
-- checked first is the refusal the code names (ErrNoPrincipal rather than ErrNoCredential).
--
-- The keys onto principals are left as they are. A user's principal is written anew under the new
-- login and the old one deleted once nothing refers to it, as a renamed namespace's service accounts
-- are (db.Wide.RenameNamespace), since a personal namespace's owner is its name: the two change in the
-- one statement that renames it, and a key carrying the principal's change through would write the
-- owner alone, which namespaces_personal_owner refuses.
do $$
declare k record;
begin
  for k in
    select c.conrelid::regclass as tbl, c.conname, pg_get_constraintdef(c.oid) as def
    from pg_constraint c
    where c.contype = 'f'
      and c.confrelid in ('users'::regclass, 'credentials'::regclass, 'enrolment_codes'::regclass)
      and pg_get_constraintdef(c.oid) not like '%ON UPDATE%'
    order by c.oid
  loop
    execute format('alter table %s drop constraint %I', k.tbl, k.conname);
    execute format('alter table %s add constraint %I %s ON UPDATE CASCADE', k.tbl, k.conname, k.def);
  end loop;
end
$$;

-- Logins and namespace names share one name space, as before, and a login changed is held to it as a
-- login created is: refused where a namespace holds it, as its name or a former one. A personal
-- namespace is renamed with its user's login and in no other way: once no user holds the name it
-- leaves, and with its owner written to the name it takes in the same statement, which is what
-- following a login the user was given is. A rename of one of its own is still refused, since its
-- name is its user's login.
create or replace function agentiik_names_are_shared() returns trigger
  language plpgsql
  as $$
begin
  perform pg_advisory_xact_lock(474080961907);
  if tg_table_name = 'users' then
    if tg_op = 'UPDATE' and new.login = old.login then
      return new;
    end if;
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
    if old.kind = 'personal'
       and (new.owner is distinct from new.name or exists (select from users where login = old.name)) then
      raise exception 'namespace % is the personal namespace of the user %, named after their login, and is renamed only with it', old.name, old.name
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

create trigger users_share_names_when_renamed before update of login on users
  for each row execute function agentiik_names_are_shared();

-- A new login told to its user: login_changed, naming the login they had, from_login, the one they
-- were given, to_login, and who gave it, acted_by, a login or operator for the bootstrap token. Each is
-- named rather than referred to, as on the other kinds, so that what the user was told reads as it
-- happened whatever is renamed or removed after: a notification keeps the logins it named when it was
-- told, and the recipient alone follows a later login, since it is who reads it. from_login is held to
-- the grammar a namespace's former name is, rather than to a login's reserved words, since it is a
-- word a release reserved after it was given.
--
-- Told unless the user gave it themselves, which tells them nothing, and dismissed by its reader as
-- the other kinds but the recovery codes' are: nothing about it gives anybody a way into the account.
alter table notifications
  add column from_login text check (agentiik_given_name(from_login)),
  add column to_login   text check (agentiik_given_name(to_login)),
  drop constraint notifications_kind_check,
  add constraint notifications_kind_check
    check (kind in ('admin_access_widened', 'passkey_counter_refused', 'break_glass_recovery',
                    'recovery_code_issued', 'recovery_code_used', 'login_changed')),
  drop constraint notifications_one_kind,
  -- Every kind named, and false for any other, since a case answering null would pass the check.
  add constraint notifications_one_kind check (case kind
    when 'admin_access_widened' then
      namespace is not null and access_grant is not null and act is not null and acted_by is not null
        and credential is null and (login is not null) = (act = 'joined_group')
        and from_login is null and to_login is null
    when 'passkey_counter_refused' then
      credential is not null and namespace is null and access_grant is null and login is null
        and act is null and acted_by is null and from_login is null and to_login is null
    when 'break_glass_recovery' then
      login is not null and namespace is null and access_grant is null and credential is null
        and act is null and acted_by is null and from_login is null and to_login is null
    when 'recovery_code_issued' then
      acted_by is not null and namespace is null and access_grant is null and credential is null
        and login is null and act is null and from_login is null and to_login is null
    when 'recovery_code_used' then
      acted_by is not null and credential is not null and namespace is null and access_grant is null
        and login is null and act is null and from_login is null and to_login is null
    when 'login_changed' then
      from_login is not null and to_login is not null and acted_by is not null and namespace is null
        and access_grant is null and credential is null and login is null and act is null
    else false
  end);
