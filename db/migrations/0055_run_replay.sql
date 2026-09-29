-- A replay: "Replay from a step. Reuses the envelopes and artifacts already produced upstream."
--
-- A replay is a new run of the commit the run it replays pinned, over the same inputs, and it says
-- which run it replays and from which step, so that a person reading it knows why the steps above
-- that one carry a verdict and no task: they were reused rather than run. A replay from the start
-- names the run and no step. The run it replays is named by its identifier and not by a key onto
-- it, since the replay outlives it: a run's retention is its own, and a replay's history says what
-- it was a replay of after that run has gone.
alter table runs
  add column replay_of ulid,
  add column replay_from identifier,
  add constraint runs_replay check (replay_from is null or replay_of is not null);
