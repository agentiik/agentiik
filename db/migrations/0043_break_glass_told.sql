-- The break-glass path told to every administrator: agentiik-api recover, which issues an
-- administrator a recovery code with no credential, from the installation's host, writes a
-- break_glass_recovery notification to each of them, the one recovered included, naming the account
-- and when. It is the one way to a recovery code that no administrator vouches for, so its use is
-- never silent: an administrator who did not run it learns that whoever holds the host did.
--
-- The account is named by its login and not referred to, so that removing it, which is what the
-- others may do on reading this, leaves what they were told. Nothing to fill in at the upgrade:
-- nothing before this told anybody of a recovery.
alter table notifications
  add column login text check (agentiik_given_name(login)),
  drop constraint notifications_kind_check,
  add constraint notifications_kind_check
    check (kind in ('admin_access_widened', 'passkey_counter_refused', 'break_glass_recovery')),
  drop constraint notifications_one_kind,
  add constraint notifications_one_kind check (case kind
    when 'admin_access_widened' then
      namespace is not null and access_grant is not null and credential is null and login is null
    when 'passkey_counter_refused' then
      credential is not null and namespace is null and access_grant is null and login is null
    else
      login is not null and namespace is null and access_grant is null and credential is null
  end);
