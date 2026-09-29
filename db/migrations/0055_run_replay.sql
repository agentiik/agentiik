-- A replay: "Replay from a step. Reuses the envelopes and artifacts already produced upstream."
--
-- A replay is a new run of the commit the run it replays pinned, over the same inputs, and it says
-- which run it replays and from which step, so that a person reading it knows why the steps above
-- that one carry a verdict and no task: they were reused rather than run. A replay from the start
-- names the run and no step. The run it replays is named by its identifier and not by a key onto
-- it, since the replay outlives it: a run's retention is its own, and a replay's history says what
-- it was a replay of after that run has gone.
--
-- Text held to the ulid and identifier grammars by a check, rather than typed as those domains. A
-- column added with a type whose domain carries a check has PostgreSQL rewrite every row of the
-- table, since it verifies the null each row takes against the domain, and runs is the table an
-- upgrade must leave where it is: a check on a column of plain text is verified by reading the
-- rows, which it writes none of. The casts hold the values to the domains' own checks, so the
-- grammar is the one every other run and step is written to.
alter table runs
  add column replay_of text check (replay_of is null or replay_of::ulid is not null),
  add column replay_from text check (replay_from is null or replay_from::identifier is not null),
  add constraint runs_replay check (replay_from is null or replay_of is not null);
