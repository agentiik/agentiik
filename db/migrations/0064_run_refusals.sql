-- The runs a namespace was refused for max_runs_per_hour, a minute at a time, which the chart of a
-- namespace against its quotas draws beside the runs it created: "the runs created in each hour,
-- and those refused with 429, against max_runs_per_hour". A run refused is no run, and leaves no
-- row of runs to count, so the refusal is written down where it is decided, whatever asked for the
-- run: a request answered 429, a schedule or an event firing recorded as skipped, a call failed.
--
-- A count a minute rather than a row a refusal, because a client that keeps asking past the quota
-- is refused as often as it asks, and a row each time would let it write to the database as fast
-- as it can send: a minute holds one row however often it is refused in it. A minute because it is
-- the smallest bucket a series is counted in. The minutes older than the namespace's
-- max_retention_days are let go as another is written, since a series reaches back as far as the
-- namespace keeps its runs, and no further.
create table run_refusals (
  namespace text not null references namespaces (name) on delete cascade,
  minute    timestamptz not null check (minute = date_trunc('minute', minute)),
  refused   integer not null check (refused > 0),
  primary key (namespace, minute)
);

alter table run_refusals enable row level security;
alter table run_refusals force row level security;
create policy run_refusals_by_namespace on run_refusals
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());
