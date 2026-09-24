-- What a runner reports of itself at every heartbeat, as wire.schema.json writes the request,
-- $defs/runnerHeartbeat/request: which agent it runs, what it will do with new work, and how many
-- tasks it will run at once. The inventory shows them, because "a runner that answers and refuses
-- work is present and useless, and present alone would hide that".
--
-- The agent's version already has a column, written at join, and the heartbeat writes it again:
-- "an agent upgraded in place keeps its record and never registers again", so after the join this
-- is the only message that can say what is now running there.

alter table runners
  -- What the runner says it will do with new work: ready takes tasks, draining finishes what it
  -- holds, unhealthy has taken itself out of service. Not state, which is the installation's
  -- word on the runner (ready, draining, revoked) and what the heartbeat answers a drain from:
  -- the two are different questions, and a runner told to drain says draining here only once
  -- the order has reached it, which is how a drain is watched rather than guessed at. Null until
  -- the first heartbeat, since a runner that has joined and never reported has said nothing.
  add column reported_state text
    check (reported_state in ('ready', 'draining', 'unhealthy')),

  -- The most tasks the host will run at once, AGK_RUNNER_CONCURRENCY, "reported on every
  -- heartbeat rather than once at registration because it is a line in a unit file and changes
  -- when the agent restarts". A bigint for the reason cpu is one: the wire sets a least and no
  -- most, and an integer would refuse a count the wire accepts.
  add column concurrency bigint
    check (concurrency >= 1),

  -- Reported together or not yet, since the wire requires both of every heartbeat.
  add constraint runners_report_whole
    check ((reported_state is null) = (concurrency is null));
