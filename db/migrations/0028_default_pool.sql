-- The pool a step that names no runner label goes to.
--
-- A step goes to the pool whose labels include every label of its runs_on, and a step that names
-- none is included by every pool, so it goes to the pool named default instead. The installation
-- creates it here, with the schema, rather than leaving it to an administrator: every installation
-- runs its migrations before anything else, a fresh one and an upgraded one alike, and an
-- installation without it would fail every such step on the infrastructure's account until
-- somebody noticed why.
--
-- It carries no label, so it is reachable only by a step that names none; it accepts every
-- namespace and has no ceiling, which is what an installation with one pool runs with. An
-- installation that created a pool of this name already keeps its own.
insert into runner_pools (name, created_by) values ('default', 'installation')
  on conflict (name) do nothing;
