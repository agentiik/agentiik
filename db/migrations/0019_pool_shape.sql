-- A runner pool as wire.schema.json writes it, $defs/runnerPool: a name, the labels its runners
-- may claim, the namespaces it accepts, the ceilings one task may be given on it, and the tier
-- that contains its containers.
--
-- 0010 held a pool to less than the wire does, so the table could keep a pool the API cannot
-- answer: a name in capitals or with an underscore, and a label that is not key=value. Each is
-- held here to the grammar the wire gives it, so that nothing written through any door is a pool
-- no reader of the wire accepts.

-- A label as a step selects on it and a runner claims it: key=value, the key lowercase. A domain
-- rather than a check on each column, because what is held is every entry of an array, and a
-- check cannot look inside one without a function nobody would read. A runner's own labels are
-- not typed with it: they are its token's, compared entry by entry when it joins, so holding the
-- token's holds them.
create domain label as text
  check (value ~ '^[a-z0-9]+(?:[._-][a-z0-9]+)*=[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$');

-- A namespace named inside a pool's list, on the grammar the namespaces table already holds its
-- own names to. Not a foreign key, which an array cannot carry, and not needed: a pool may name a
-- namespace before it is created, and naming one that never is accepts nothing.
create domain namespace_name as text
  check (value ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$');

-- The name, lowercase words joined by hyphens: "the one this installation uses everywhere a name
-- is given rather than minted". And at most 255 characters, since the pool's durable consumer on
-- the bus is named after it and NATS names nothing longer, so a longer name would be a pool
-- whose work could never be taken.
alter table runner_pools drop constraint runner_pools_name_check;
alter table runner_pools add constraint runner_pools_name_check
  check (name ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$' and length(name) <= 255);

alter table join_tokens drop constraint join_tokens_pool_check;
alter table join_tokens add constraint join_tokens_pool_check
  check (pool ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$' and length(pool) <= 255);

alter table runner_pools
  alter column labels drop default,
  alter column labels type label[] using labels::label[],
  alter column labels set default '{}',
  alter column accepted_namespaces drop default,
  alter column accepted_namespaces type namespace_name[] using accepted_namespaces::namespace_name[],
  alter column accepted_namespaces set default '{}';

alter table join_tokens
  alter column labels drop default,
  alter column labels type label[] using labels::label[],
  alter column labels set default '{}';

-- The ceilings are the three a task asks for, cpu, memory and pids, written as the manifest and
-- the step write them so that a pool answers what its administrator wrote: two cores are "2" and
-- eight gibibytes are "8Gi", never a count of bytes somebody has to divide back. Null is no
-- ceiling of that kind, which is the ordinary case for a pool of uniform hosts where the host
-- itself is the limit.
--
-- There is no disk ceiling, because nothing can enforce one: the daemon caps a container's cpu,
-- memory and processes, and a task writes into a working directory on the host, bound into the
-- container, which no setting of a container bounds. A ceiling nothing applies would be a number
-- an administrator trusted and the runner ignored. No installation holds a pool yet, so the old
-- ceilings are dropped rather than converted.
alter table runner_pools
  drop column max_cpu,
  drop column max_memory_bytes,
  drop column max_disk_bytes,
  add column ceiling_cpu text
    check (ceiling_cpu ~ '^(?:0*[1-9][0-9]*(?:\.[0-9]+)?|0*\.[0-9]*[1-9][0-9]*)$'),
  add column ceiling_memory text
    check (ceiling_memory ~ '^[1-9][0-9]*(?:Ki|Mi|Gi|Ti)$'),
  add column ceiling_pids integer
    check (ceiling_pids > 0),
  -- What runs the containers here: "hardened is the default runtime with user-namespace
  -- remapping", sandboxed is gVisor's runsc and separated is hosts of their own. The column holds
  -- the three the wire names; which of them an installation can deliver is the API's to say.
  add column containment text not null default 'hardened'
    check (containment in ('hardened', 'sandboxed', 'separated'));
