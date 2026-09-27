-- Which act widened an administrator's access, and who did it, on every admin_access_widened
-- notification. The grant alone did not say: a group's grant brought by a membership was written by
-- somebody else, long before, and a deny lifted names whoever wrote the deny, so a reader told of
-- either could not tell who acted or how.
--
-- act is one of five: granted, a grant or a deny written by the installation's power, or a role
-- given to the administrator's own access; deny_lifted, a deny revoked from their own access;
-- joined_group, a user put in a group holding a role in the namespace, the administrator or
-- somebody else, whom login names; left_group, the administrator taking themselves out of a group
-- whose deny applied there; group_removed, the administrator removing a group they were in, whose
-- deny applied there. acted_by is who acted, named rather than referred to, as login is, so that
-- removing them leaves what the others were told.
--
-- The table is new in v0.3.0, so the upgrade from v0.2 finds it empty. Rows a build of v0.3.0
-- wrote before its release are filled in where the row says for certain: a grant told at the
-- instant it was written was written then, by its granted_by. One told later, a deny lifted or a
-- membership changed, names nobody who acted, and is removed rather than told with a guess. Written
-- across every namespace, which is what the installation scope is bound for, whatever policy the
-- table stands behind.
alter table notifications
  add column act text
    check (act in ('granted', 'deny_lifted', 'joined_group', 'left_group', 'group_removed')),
  add column acted_by text check (acted_by <> '');

select set_config('agentiik.scope', 'installation', true);
update notifications set act = 'granted', acted_by = access_grant->>'granted_by'
 where kind = 'admin_access_widened' and (access_grant->>'granted_at')::timestamptz = at
   and access_grant->>'granted_by' <> '';
delete from notifications where kind = 'admin_access_widened' and act is null;

alter table notifications
  drop constraint notifications_one_kind,
  add constraint notifications_one_kind check (case kind
    when 'admin_access_widened' then
      namespace is not null and access_grant is not null and act is not null and acted_by is not null
        and credential is null and (login is not null) = (act = 'joined_group')
    when 'passkey_counter_refused' then
      credential is not null and namespace is null and access_grant is null and login is null
        and act is null and acted_by is null
    else
      login is not null and namespace is null and access_grant is null and credential is null
        and act is null and acted_by is null
  end);
