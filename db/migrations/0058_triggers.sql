-- The triggers armed: "Schedules, webhooks and events" run the default branch, so what the default
-- branch's head declares under on is what is armed, one row per entry, and nothing else is.
--
-- A row is armed when its version's commit lands on the default branch and disarmed when another
-- lands there that does not declare it, in the transaction that moves the branch, so that no head is
-- ever without its triggers or with another's. workflows.armed names the commit whose triggers are
-- armed, null while none has been, which is what tells the leading controller, at the start of each
-- term, a workflow whose head is not armed yet: one pushed before v0.5.0, or one whose arming failed.
--
-- A row keeps its state across a push that declares the same entry again: a schedule its next
-- occurrence and its last firing, so that a push at 05:59 neither skips nor doubles the run due at
-- 06:00, and a webhook the signature failures counted against its path and method. Its position is
-- the entry's place in its list under on, which is how a person names it: the second schedule.
alter table workflows add column armed text check (armed ~ '^[0-9a-f]{40}$');

create table triggers (
  namespace text not null,
  workflow  identifier not null,
  kind      text not null check (kind in ('schedule', 'webhook', 'event')),
  position  integer not null check (position >= 0),
  -- The version that armed it, whose file declares it.
  commit    text not null check (commit ~ '^[0-9a-f]{40}$'),
  -- The entry, as the version declares it with the values in force: a schedule's zone, a webhook's
  -- method, auth and response written in where the file leaves them out.
  declared  jsonb not null,

  -- A webhook's path and method, which one trigger of the namespace answers.
  path      text check (path ~ '^(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)+$' and length(path) <= 255),
  method    text check (method ~ '^[A-Z]+$'),
  -- An event's type and source, where the event consumer looks for a subscription.
  type      text,
  source    text,

  -- A schedule's next occurrence, the instant it is due, and when it fires, the due instant with
  -- its jitter drawn once, so that a failover neither draws again nor fires it twice. Computed from
  -- the row and never from memory: the next occurrence after the last one fired for, or after the
  -- instant it was armed.
  due_at    timestamptz,
  fire_at   timestamptz,

  -- Its last firing: when, for which occurrence, and the run it started; or why it started none,
  -- the namespace's max_runs_per_hour spent, recorded as a skipped firing.
  fired_at  timestamptz,
  fired_for timestamptz,
  fired_run text,
  skipped   text,

  -- A webhook's signature failures, counted and surfaced: "a sudden run of them is an unannounced
  -- rotation or somebody guessing".
  failures  bigint not null default 0 check (failures >= 0),
  failed_at timestamptz,

  armed_by  text not null,
  armed_at  timestamptz not null default now(),

  primary key (namespace, workflow, kind, position),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade on update cascade,
  check ((kind = 'webhook') = (path is not null and method is not null)),
  check ((kind = 'schedule') = (due_at is not null and fire_at is not null)),
  check (kind = 'event' or (type is null and source is null))
);

-- "Within a namespace a path and a method answer one trigger." The index is the reservation: a push
-- arming a pair another workflow of the namespace holds fails here whatever raced it.
create unique index triggers_hook on triggers (namespace, path, method) where kind = 'webhook';

-- What the leading controller fires next, across every namespace.
create index triggers_due on triggers (fire_at) where kind = 'schedule';

-- Where the event consumer looks.
create index triggers_event on triggers (namespace, type) where kind = 'event';

alter table triggers enable row level security;
alter table triggers force row level security;
create policy triggers_by_namespace on triggers
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
