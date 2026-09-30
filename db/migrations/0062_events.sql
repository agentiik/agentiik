-- Events published to a namespace: "CloudEvents 1.0 published to POST /api/v1/{ns}/events", heard by
-- the event triggers armed in it, and by those of another namespace that name it, "where it granted
-- the workflow's namespace read access".
--
-- An event trigger's row names the namespace it hears, its own where the file names none, which is
-- where a publication looks for the subscriptions it matches: across every namespace, since a
-- trigger of another namespace naming this one is one of them.
alter table triggers add column hears text;
update triggers set hears = namespace where kind = 'event';
alter table triggers add constraint triggers_hears check ((kind = 'event') = (hears is not null));

drop index triggers_event;
create index triggers_heard on triggers (hears, type) where kind = 'event';

-- An event published is remembered by its namespace, its source and its id for a day: "consumers may
-- assume that events with identical source and id are duplicates", and one published again within
-- the day is answered as the first was, with how many runs it started, and starts nothing. Let go by
-- the next publication into the namespace once the day is past.
create table event_deliveries (
  namespace text not null,
  source    text not null check (length(source) between 1 and 1024),
  id        text not null check (length(id) between 1 and 255),
  taken_at  timestamptz not null,
  publisher text not null,
  runs      integer not null default 0 check (runs >= 0),
  primary key (namespace, source, id)
);

create index event_deliveries_taken on event_deliveries (namespace, taken_at);

alter table event_deliveries enable row level security;
alter table event_deliveries force row level security;
create policy event_deliveries_by_namespace on event_deliveries
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
