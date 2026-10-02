-- A namespace's variables: "A namespace keeps variables of its own beside those a workflow file
-- writes", each a name, a JSON value and a visibility, read through the vars context by the
-- workflows it is shown to, a name the file writes taking the file's value.
--
-- One row per variable rather than one document per namespace, for the reason secret_declarations
-- gives: a variable is written one at a time, and two writers each rewriting the whole set would
-- drop each other's change without either seeing it happen.
--
-- Not a secret. The value is kept here in clear, answered to whoever reads the namespace's workflows
-- and kept on every run that read it: a credential is a secret, declared in secret_declarations and
-- sealed in secret_values, and nothing here is.

create table namespace_variables (
  -- Removed with its namespace, as its grants and its authentication policy are: a variable is a
  -- setting of the namespace and nobody's work, which a namespace refusing removal while it held one
  -- would treat it as. Carried with its namespace's name where the namespace is renamed, as every key
  -- onto namespaces is.
  namespace   text not null references namespaces (name) on delete cascade on update cascade,
  -- Read as vars.<name>, on the grammar a file's vars names its keys with.
  name        identifier not null,

  -- Any JSON value, null among them, each number as it was written but for an exponent, which the
  -- API writes out with a point so that a double reads back a double: jsonb keeps a number at the
  -- scale it was written with and writes an exponent out as a whole number.
  value       jsonb not null,
  -- What the namespace's bounds are counted from, as the API measured the value when it wrote it:
  -- the bytes it takes written as compact JSON, at most 64 KiB, and how many values it holds, the
  -- value itself and everything in it, as a run's inputs are counted. Kept rather than computed
  -- again at each write, which would read every value of the namespace to write one.
  value_bytes integer not null check (value_bytes between 1 and 65536),
  value_count integer not null check (value_count >= 1),

  -- all, read by every workflow of the namespace, or selected, read by those workflows names, sorted
  -- and each once, held to the workflow grammar by the API. A name no workflow holds is kept: it is
  -- read by the workflow created under it.
  visibility  text not null check (visibility in ('all', 'selected')),
  workflows   text[] check (cardinality(workflows) <= 1024),
  check ((visibility = 'selected') = (workflows is not null)),

  -- Who wrote it last and when. Not the audit log, which records every write, but what somebody
  -- reading the variables needs to know whom to ask.
  updated_by  text not null,
  updated_at  timestamptz not null default now(),

  primary key (namespace, name)
);

-- The variables a run reads are found by its namespace alone: all of them, filtered by visibility,
-- at most a thousand, which the primary key's index already serves.

alter table namespace_variables enable row level security;
alter table namespace_variables force row level security;

create policy namespace_variables_by_namespace on namespace_variables
  using (namespace = agentiik_namespace() or agentiik_installation())
  with check (namespace = agentiik_namespace() or agentiik_installation());

-- What a run read of its namespace's variables, frozen on it when it was created: the variables
-- shown to its workflow whose names its file does not write, a JSON object of names and values,
-- which the evaluator is started with under the file's own and a replay is written with, so that a
-- controller taking the run over and a replay read what it read rather than what is true now. Null
-- for a run that read none, and for every run from before v0.6.0, which read the file's own.
alter table runs add column namespace_vars jsonb check (namespace_vars is null or jsonb_typeof(namespace_vars) = 'object');
