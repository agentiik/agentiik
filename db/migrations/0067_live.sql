-- The live connection, GET /api/v1/me/live: what a console shows says that it changed, on the
-- channel agentiik_live, from the transaction that changed it, which PostgreSQL delivers only once
-- that transaction commits, so that a console told of a change finds it when it reads.
--
-- Said by triggers rather than by each statement that writes a run, a step, a task, a notification
-- or a runner, since those are written from the API, the controller and the purge, and a writer
-- that forgot to say so would leave a screen stale with nothing to tell why. A payload names what
-- changed and nothing of it, as the run channel's and the log channel's do: the API reads what it
-- names through the route that answers it, under that route's permissions. PostgreSQL sends one
-- notification for identical payloads sent in one transaction, so that a step whose eight shards
-- move together is said once.
--
--   run NAMESPACE WORKFLOW RUN   a run, one of its steps or one of its tasks changed
--   notifications PRINCIPAL      a notification told to the principal, or dismissed
--   runners                      a runner or a pool changed, a heartbeat included

create function live_run() returns trigger
  language plpgsql
  as $$
begin
  perform pg_notify('agentiik_live', 'run ' || new.namespace || ' ' || new.workflow || ' ' || new.id);
  return null;
end
$$;

-- What the screens show of a run and its steps: their state, their times, a cancellation asked for
-- and what they published. Not the controller's bookkeeping, wake_at among it, which moves with no
-- change anybody sees.
create trigger runs_live after insert or update of state, started_at, finished_at, cancel_requested_at, outputs on runs
  for each row execute function live_run();

-- A step and a task name their run and not its workflow, which is read from the run, in the
-- namespace the writer's transaction is bound to, as the row itself is.
create function live_run_of() returns trigger
  language plpgsql
  as $$
declare
  of text;
begin
  select workflow into of from runs where namespace = new.namespace and id = new.run_id;
  if of is not null then
    perform pg_notify('agentiik_live', 'run ' || new.namespace || ' ' || of || ' ' || new.run_id);
  end if;
  return null;
end
$$;

create trigger steps_live after insert or update of state, attempts, started_at, finished_at, ports on steps
  for each row execute function live_run_of();

-- A task's state and the runner it went to, which the run inspector names: the rest of its row is
-- the bookkeeping of its dispatch and its log, which no screen shows.
create trigger tasks_live after insert or update of state, runner on tasks
  for each row execute function live_run_of();

create function live_notifications() returns trigger
  language plpgsql
  as $$
begin
  if tg_op = 'DELETE' then
    perform pg_notify('agentiik_live', 'notifications ' || old.recipient);
  else
    perform pg_notify('agentiik_live', 'notifications ' || new.recipient);
  end if;
  return null;
end
$$;

create trigger notifications_live after insert or delete on notifications
  for each row execute function live_notifications();

-- Every change, a heartbeat's included: the runners view says how long ago each runner was heard
-- from and what it said of itself, and a runner it last read ten seconds ago would otherwise be
-- said to be silent.
create function live_runners() returns trigger
  language plpgsql
  as $$
begin
  perform pg_notify('agentiik_live', 'runners');
  return null;
end
$$;

create trigger runners_live after insert or update or delete on runners
  for each row execute function live_runners();

create trigger runner_pools_live after insert or update or delete on runner_pools
  for each row execute function live_runners();
