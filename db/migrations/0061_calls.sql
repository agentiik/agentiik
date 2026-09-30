-- Calling a workflow from a workflow: a workflow: step starts a run of the workflow it calls, "with
-- trigger_kind: workflow and a from naming its caller", and waits on it in place of a container.
--
-- The run a call started names the run, the step and the dispatch that called it, and how deep in a
-- chain of calls it is: a run nothing called is at depth 0, and each call one deeper, which is what
-- "the call depth limit (8 by default)" is counted against. The dispatch is unique among the runs,
-- whatever their namespace, so that a pass that made the call and died before writing it down makes
-- it again and finds the run it started rather than starting a second.
--
-- The dispatch of the calling step names the run it started, called_run, which is what tells it from
-- a task a runner holds: it holds no slot of max_concurrent_tasks, no runner redeems it and no sweep
-- declares it lost, since nothing heartbeats for it, and it ends when the run it called does.
alter table runs add column caller_run  text;
alter table runs add column caller_step text;
alter table runs add column caller_task text;
alter table runs add column depth       integer not null default 0 check (depth >= 0);
alter table runs add constraint runs_called_by_a_step
  check ((trigger = 'workflow') = (caller_run is not null and caller_step is not null and caller_task is not null));
alter table runs add constraint runs_depth_of_a_call
  check ((caller_run is null) = (depth = 0));

create unique index runs_caller_task on runs (caller_task) where caller_task is not null;

-- What the controller looks for when a run it called ends.
create index runs_caller on runs (caller_run) where caller_run is not null;

alter table tasks add column called_run text;
