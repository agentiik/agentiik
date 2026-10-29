-- A recovery code an administrator issues for a user, told to that user: recovery_code_issued when
-- it is issued, naming who issued it, and recovery_code_used when it is spent on the enrolment page,
-- naming who issued it and the credential it enrolled, by the identifier GET /api/v1/me/credentials
-- lists it with. The code lets whoever holds it sign in as the user, whose passkeys keep working, so
-- it is a way an administrator could reach their data that nothing else tells them of. The user
-- alone is told: they know whether they asked for it, and hold the way to undo it, removing the
-- credential it enrolled.
--
-- acted_by is who issued it, a login or operator for the bootstrap token, named rather than referred
-- to, as on admin_access_widened, so that removing them leaves what the user was told. Neither kind
-- keeps the code or its link, which no column here could hold. A break-glass code, issued by the
-- installation, is told to every administrator when it is issued (0043) and not again when spent.
--
-- Neither is dismissed by its reader, which the dismissal refuses (db.Wide.DismissNotification)
-- rather than this table, since one past its 90 days, or whose reader is removed, is deleted all the
-- same: whoever spent the code signs in as the user, and could otherwise dismiss what tells them.
--
-- A password a recovery code set keeps who issued the code, as recovered_by, where its user was told
-- of it, and nothing where anything else set it, so that a passkey registered from a session the
-- password opens that may only enrol is told too, naming it: where the policy requires a passkey,
-- such a password is a step to the passkeys it lets its holder register, and goes once the account
-- holds min_passkeys of them, so the passkey is the way in that lasts. Changed from a session, which
-- may be one that only enrols and whoever spent the code holds, it keeps the mark, as it keeps its
-- identifier; set by another code, it takes that code's.
--
-- Nothing to fill in at the upgrade: nothing before this told anybody of a recovery code but the
-- break-glass one, and no password set before it is marked.
alter table credentials
  add column recovered_by text,
  add constraint credentials_recovered_by check (recovered_by is null or type = 'password');

alter table notifications
  drop constraint notifications_kind_check,
  add constraint notifications_kind_check
    check (kind in ('admin_access_widened', 'passkey_counter_refused', 'break_glass_recovery',
                    'recovery_code_issued', 'recovery_code_used')),
  drop constraint notifications_one_kind,
  -- Every kind named, and false for any other, since a case answering null would pass the check.
  add constraint notifications_one_kind check (case kind
    when 'admin_access_widened' then
      namespace is not null and access_grant is not null and act is not null and acted_by is not null
        and credential is null and (login is not null) = (act = 'joined_group')
    when 'passkey_counter_refused' then
      credential is not null and namespace is null and access_grant is null and login is null
        and act is null and acted_by is null
    when 'break_glass_recovery' then
      login is not null and namespace is null and access_grant is null and credential is null
        and act is null and acted_by is null
    when 'recovery_code_issued' then
      acted_by is not null and namespace is null and access_grant is null and credential is null
        and login is null and act is null
    when 'recovery_code_used' then
      acted_by is not null and credential is not null and namespace is null and access_grant is null
        and login is null and act is null
    else false
  end);
