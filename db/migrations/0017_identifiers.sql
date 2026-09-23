-- Every name the workflow file writes, held to the grammar it is written on and kept whole.
--
-- 0001 wrote a domain for that grammar, "letters, digits, hyphens and underscores, beginning with
-- a letter or a digit", and called it name, which is also what PostgreSQL calls its own type for
-- the identifiers of its catalog. pg_catalog is searched before any schema of ours, so every
-- column 0001 typed name was given that type and never the domain: workflows.name,
-- workflow_versions.workflow, runs.workflow, steps.step, tasks.step, artifacts.step and
-- artifacts.port. That type checks nothing and cuts a value at 63 bytes without saying so, so a
-- step called "two words" was stored as written, and two workflow names sharing their first 63
-- bytes were one workflow.
--
-- The domain is renamed rather than qualified at every use. identifier is what the language
-- chapter calls the grammar, and a name PostgreSQL does not hold is one a column can be typed with
-- bare, which is how every other column here is written and how the next one will be. Left as
-- name, the next column typed with it would be PostgreSQL's again.
--
-- And it stays unbounded, as the grammar is. The documentation gives an identifier no length, the
-- evaluator accepts one of any length, and the driver digests a task identifier too long for a
-- network name rather than cutting it; a length here would refuse at the database what everything
-- before it had accepted. So a name past 63 bytes is now kept whole, which keeps two such names two.

-- The one name this file cannot write bare, since bare it is PostgreSQL's. 0001 created the domain
-- wherever an unqualified name went, which is the schema an unqualified name goes to now.
do $$
begin
  execute format('alter domain %I.name rename to identifier', current_schema());
end
$$;

-- So that a refusal names the rule by what it is called now.
alter domain identifier rename constraint name_check to identifier_check;

alter table workflows
  alter column name type identifier;

alter table workflow_versions
  alter column workflow type identifier;

alter table runs
  alter column workflow type identifier;

alter table steps
  alter column step type identifier;

-- tasks.step is read by the idempotency key, and PostgreSQL retypes no column a generated one is
-- computed from. So the key goes and comes back, on the expression 0001 wrote, with the two
-- indexes 0013 left on it: every row is computed again from the same four columns, and the keys
-- come out as they went in, since retyping a step changes none of its bytes.
drop index tasks_by_idempotency_key;
drop index tasks_by_dispatch;

alter table tasks
  drop column idempotency_key;

alter table tasks
  alter column step type identifier;

alter table tasks
  add column idempotency_key text generated always as (
    run_id || '/' || step || '/' || attempt ||
    case when shard_index is null then ''
         else '/' || shard_index || '/' || shard_of end
  ) stored;

create unique index tasks_by_dispatch on tasks (namespace, idempotency_key, requeue);
create unique index tasks_by_idempotency_key on tasks (namespace, idempotency_key)
  where state <> 'lost';

alter table artifacts
  alter column step type identifier,
  alter column port type identifier;

-- A secret's name is an identifier too, and 0015 and 0016 wrote the grammar out by hand only
-- because the domain could not be reached. It is the domain now, and the bound stays the column's:
-- 255 is about the file a step reads the value from, which is a secret's reason and not every
-- name's.
alter table secret_declarations
  alter column name type identifier,
  drop constraint secret_declarations_name_check,
  add constraint secret_declarations_name_check check (length(name) <= 255);

alter table secret_values
  alter column name type identifier,
  drop constraint secret_values_name_check,
  add constraint secret_values_name_check check (length(name) <= 255);
