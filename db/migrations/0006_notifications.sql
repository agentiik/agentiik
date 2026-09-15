-- What is worth waking somebody for.
--
-- The controller "emits the notification events that the API turns into push messages: failure,
-- approval requested, completion of a run the recipient started". Two processes, so the events
-- are a table: the controller writes them when it decides, the API reads them and resolves who
-- wants them, and neither calls the other.
--
-- An event carries an identifier and a state and nothing else, which is the same rule the push
-- message itself is held to: "A push message carries an identifier and a state, never a payload,
-- a log line or a workflow name from a namespace the device may have lost access to. The
-- application fetches the detail over the API after the notification is tapped, so that a
-- revoked grant takes effect between the alert and what is shown." An event that carried a
-- workflow name would put that name past the grant check, in a row, before anybody had asked.
--
-- Who receives one is not decided here. That is notification_prefs, "per principal, per
-- namespace, per event kind", and the grants behind it, and both belong to the group that owns
-- principals. What the controller knows is what happened and who started it.

create table notification_events (
  namespace   text not null,
  id          ulid not null,

  -- The three the page names. approval_requested has nothing emitting it yet, because nothing
  -- in the language can suspend a run, and it is here so that the group that teaches it to
  -- does not also have to widen a check constraint.
  kind        text not null
              check (kind in ('failure', 'approval_requested', 'completion')),

  run_id      ulid not null,
  -- The state the run was in when this was emitted, which with the identifier is the whole of
  -- what a push message carries.
  run_state   run_state not null,

  -- Who started the run, so that the API can answer "completion of a run the recipient
  -- started" without reading the run. Null where a schedule or an event started it, since
  -- those are attributed to the namespace service identity rather than to a person.
  started_by  text,

  created_at   timestamptz not null default now(),
  delivered_at timestamptz,

  primary key (namespace, id),
  foreign key (namespace, run_id) references runs (namespace, id) on delete cascade,

  -- One event of one kind per run. A run reaches a terminal state once, in one committed
  -- decision, and emitting is part of that decision, so this is a belt on top of a structural
  -- guarantee rather than the guarantee itself.
  unique (namespace, run_id, kind)
);

create index notification_events_undelivered on notification_events (created_at)
  where delivered_at is null;

-- Behind the same policy as everything else a namespace owns. An event names a run and a
-- principal, so one namespace reading another's would be one namespace learning that the other
-- has runs and who starts them, which is the first thing the isolation chapter says nobody may
-- learn: "The existence of workflows they cannot read, in any listing, search result or error
-- message."
alter table notification_events enable row level security;
alter table notification_events force row level security;

create policy notification_events_by_namespace on notification_events
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
