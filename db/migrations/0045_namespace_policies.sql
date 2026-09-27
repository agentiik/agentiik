-- The four tables that name a namespace and stood outside its policy: the audit log, the
-- authentication policy, the notifications and the service accounts.
--
-- "Every query path carries the namespace so that a missing filter fails closed rather than
-- returning another tenant's rows." Each of the four is read across the installation, which is
-- why none was put behind a policy when it arrived: the audit log is one chain the export reads
-- whole, the policy applies at sign-in, a principal's notifications are read wherever it asks who
-- it is, and a service account is identified before any namespace is in question. So every read of
-- them goes through the installation's door, and a handle on one namespace has only ever written
-- its own rows, an audit entry or an owner told of a grant. Behind no policy, the day a handle on
-- one namespace read one of them it would read every namespace's rows: the policy makes that read
-- answer its own namespace's rows and nothing else, and a write naming another namespace an error,
-- as the tables behind one already do.
--
-- A row naming no namespace, the installation's own policy, an act on the installation or a
-- notification about a sign-in, is read through the installation's door alone, since no handle on
-- a namespace is bound to nothing.
do $$
declare t text;
begin
  foreach t in array array['audit_log', 'auth_policy', 'notifications', 'service_accounts']
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
