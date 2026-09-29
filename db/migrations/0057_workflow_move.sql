-- Moving a workflow to another namespace: "a move answers 202 with Location, whatever else the body
-- names, and carries everything too, grants included, runs and artifacts re-keyed and re-counted
-- under the target, the workflow frozen meanwhile, its pushes, runs and replays answered 409".
--
-- workflow_moves is a move asked and not yet carried out, one per workflow, in the namespace it
-- leaves. Its row is what freezes the workflow, where a push, a run, a replay, a rename or a deletion
-- asks for it. move_targets is the name the move holds in the namespace it goes to, where a workflow
-- created, pushed or renamed under it, or another move to it, is refused: the move is carried out by
-- the controller that leads, after the answer, and the name has to be free when it is. Two tables,
-- each under its own namespace's policy, since a namespace reads its own rows and no other's; both
-- are written together, on the installation, where the move is asked.
create table workflow_moves (
  namespace text not null,
  workflow  identifier not null,
  target    text not null references namespaces (name),
  asked_by  text not null,
  asked_at  timestamptz not null default now(),
  primary key (namespace, workflow),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade,
  check (target <> namespace)
);
alter table workflow_moves enable row level security;
alter table workflow_moves force row level security;
create policy workflow_moves_by_namespace on workflow_moves
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

create table move_targets (
  namespace      text not null references namespaces (name),
  name           identifier not null,
  from_namespace text not null,
  primary key (namespace, name),
  unique (from_namespace, name),
  foreign key (from_namespace, name) references workflow_moves (namespace, workflow) on delete cascade
);
alter table move_targets enable row level security;
alter table move_targets force row level security;
create policy move_targets_by_namespace on move_targets
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- What a move leaves in the store under the namespace it left, the files of the workflow's logs and
-- packs it copied under the target's: nothing counts them, as nothing counts a log or a pack, so
-- they are kept here until the grace a superseded pack is kept for has passed, for the fetch or the
-- read that began before the move, and then deleted. The objects of a version, an envelope or an
-- artifact need no row: the move lowers their counts where it left, and the collection takes them
-- as it takes any object nothing counts.
create table moved_objects (
  namespace    text not null,
  key          text not null check (key <> ''),
  delete_after timestamptz not null,
  primary key (namespace, key)
);
create index moved_objects_due on moved_objects (delete_after);
alter table moved_objects enable row level security;
alter table moved_objects force row level security;
create policy moved_objects_by_namespace on moved_objects
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- And every key naming a run, a step, a task, a task's log or a repository carries a change of
-- namespace through, as 0053 made every key naming a workflow carry a rename: the move changes the
-- workflow's row, and its versions, refs, runs, grants, pins and manifests follow already, while
-- what a run holds, its steps, tasks, grants, logs, artifacts, approvals, notifications and cache
-- entries, and what a repository holds, its packs, would refuse the statement.
do $$
declare k record;
begin
  for k in
    select c.conrelid::regclass as tbl, c.conname, pg_get_constraintdef(c.oid) as def
    from pg_constraint c
    where c.contype = 'f'
      and c.confrelid in ('workflows'::regclass, 'runs'::regclass, 'steps'::regclass,
                          'tasks'::regclass, 'task_logs'::regclass)
      and c.conrelid in ('steps'::regclass, 'tasks'::regclass, 'artifacts'::regclass,
                         'approvals'::regclass, 'notification_events'::regclass,
                         'task_grants'::regclass, 'task_logs'::regclass, 'task_log_chunks'::regclass,
                         'task_log_objects'::regclass, 'step_cache'::regclass, 'git_packs'::regclass)
      and pg_get_constraintdef(c.oid) not like '%ON UPDATE%'
  loop
    execute format('alter table %s drop constraint %I', k.tbl, k.conname);
    execute format('alter table %s add constraint %I %s ON UPDATE CASCADE', k.tbl, k.conname, k.def);
  end loop;
end
$$;
