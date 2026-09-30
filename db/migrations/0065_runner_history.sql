-- What the chart of the pools and the runners reads that no row kept: the silences between a
-- runner's heartbeats, and what the runner offered over time. "A runner posts one heartbeat every
-- 10 seconds", and runners.last_heartbeat_at holds the latest alone, so a silence that ended is
-- gone from it by the next heartbeat; and runners.concurrency holds what the runner offers now, so
-- a drain yesterday is gone from it too.
--
-- A silence is written by the heartbeat that ends it, from the one before, where they are 20
-- seconds or more apart: two intervals, the first a person reading the chart would call a gap. Those
-- of 30 seconds and more are the ones after which the sweep declared the runner's tasks lost, which
-- the chart reads from the tasks rather than keeping a count of its own. A silence still going is
-- the time since runners.last_heartbeat_at, and is written by no one until the runner is heard.
create table runner_silences (
  runner text not null references runners (id) on delete cascade,
  -- The last heartbeat before the silence, and the one that ended it, both on the API's clock, as
  -- runners.last_heartbeat_at is.
  began  timestamptz not null,
  ended  timestamptz not null,
  primary key (runner, began),
  check (ended >= began + interval '20 seconds')
);

-- What a runner offered, written by a heartbeat where it differs from what was written last: its
-- concurrency while the installation has it ready and it reports itself ready, and 0 while it is
-- drained, draining, unhealthy or revoked, since a slot nobody may be handed is no capacity. A
-- drain given between two heartbeats is written by the next, at most one interval late.
create table runner_capacity (
  runner   text not null references runners (id) on delete cascade,
  at       timestamptz not null,
  capacity bigint not null check (capacity >= 0),
  primary key (runner, at)
);

-- What each runner offers as this release starts, so that a runner not heard since the upgrade still
-- offers what it did, from the upgrade on.
insert into runner_capacity (runner, at, capacity)
select id, now(),
       case when state = 'ready' and coalesce(reported_state, 'ready') = 'ready' then coalesce(concurrency, 0) else 0 end
from runners
where state <> 'revoked';
