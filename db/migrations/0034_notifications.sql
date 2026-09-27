-- What the installation tells one principal of its own accord, listed in their GET /api/v1/me: an
-- administrator having written a grant in a namespace, or widened their own access there, told to
-- its owners, and a sign-in refused for a passkey's signature counter, told to the passkey's user.
-- The shape is the wire's, $defs/notification. Kept 90 days from when it was written, or until its
-- reader dismisses it.
--
-- The installation's rather than a namespace's: GET /api/v1/me reads a principal's across every
-- namespace, and a passkey's names none. A row is for one reader, a user or a service account,
-- whose token reads GET /api/v1/me as a user's does, and never a group, which reads nothing: what
-- is owed to a group is written once for each member, so that one member dismissing it dismisses it
-- for nobody else. Removing the reader removes what they were told, and removing a namespace what
-- anybody was told about it.
--
-- New in v0.3.0, and empty at the upgrade: nothing before it was told anything.
create table notifications (
  id           ulid primary key,
  recipient    text not null references principals (id) on delete cascade
    check (recipient not like 'group:%'),
  kind         text not null check (kind in ('admin_access_widened', 'passkey_counter_refused')),
  -- When it happened: the grant written or the deny taken away, or the sign-in refused. The 90
  -- days are counted from here.
  at           timestamptz not null,
  -- admin_access_widened: the namespace, and the grant as it was written or the deny as it was
  -- when it was taken away, kept whole rather than referred to, since it may be revoked before its
  -- reader comes to read it.
  namespace    text references namespaces (name) on delete cascade,
  access_grant jsonb check (jsonb_typeof(access_grant) = 'object'),
  -- passkey_counter_refused: the passkey, by its credential ID, kept after the passkey is removed,
  -- since removing it is what its user may do on reading this.
  credential   text check (credential ~ '^[A-Za-z0-9_-]+$'),
  constraint notifications_one_kind check (case kind
    when 'admin_access_widened' then
      namespace is not null and access_grant is not null and credential is null
    else
      credential is not null and namespace is null and access_grant is null
  end)
);

-- What GET /api/v1/me reads, newest first, and what it removes past its days.
create index notifications_by_recipient on notifications (recipient, at);
