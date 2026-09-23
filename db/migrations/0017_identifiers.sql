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
-- And it is bounded at 255 characters, which is NAME_MAX and not PostgreSQL's 63. The grammar is
-- one so that one name survives a URL, a directory and a tool list unchanged, and no directory
-- holds a longer name: a step is a directory of its task's work directory, a port a file under
-- /agk/out/ports/, a secret a file under /agk/secrets/. The evaluator, the manifest reader and the
-- push refuse a longer one where it is written, and the domain holds the same bound so that no
-- column can hold what they refuse. Unbounded, a name that is also a key of an index would be
-- refused there instead once it passed a few kilobytes, the most a btree holds a row to, as a
-- failure of the database at every run of the version naming it. So a name past 63 bytes is now
-- kept whole, which keeps two such names two, and one past 255 is refused.

-- The one name this file cannot write bare, since bare it is PostgreSQL's. 0001 created the domain
-- wherever an unqualified name went, which is the schema an unqualified name goes to now.
do $$
begin
  execute format('alter domain %I.name rename to identifier', current_schema());
end
$$;

-- So that a refusal names the rule by what it is called now.
alter domain identifier rename constraint name_check to identifier_check;

-- Before any column is typed with it, so that retyping them checks the bound as well.
alter domain identifier add constraint identifier_length check (length(value) <= 255);

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

-- A secret's name is an identifier too, and 0015 and 0016 wrote the grammar and its bound out by
-- hand only because the domain could not be reached. The domain holds both now, for the reason the
-- columns did: a step reads the value from a file named after the secret.
alter table secret_declarations
  alter column name type identifier,
  drop constraint secret_declarations_name_check;

alter table secret_values
  alter column name type identifier,
  drop constraint secret_values_name_check;
