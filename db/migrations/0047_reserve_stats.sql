-- stats joins the words the API routes on, for GET /api/v1/stats/pools, which v0.6.0 serves: a word
-- is reserved in a release before the route that needs it, so that no login, group or service
-- account created meanwhile takes it. agk.ReservedNamespaces is the same list, and a test holds the
-- two together.
--
-- Replacing the function changes what a principal may be created under, and checks no row that is
-- there already: PostgreSQL checks a row at its insert or update, and no principal's id is ever
-- updated. A namespace's name is not held to this list here, so a namespace created under stats
-- before v0.3.0 keeps its name, as the API keeps serving it.
create or replace function agentiik_reserved(name text) returns boolean
  language sql immutable
  as $$ select name in ('auth', 'me', 'users', 'groups', 'service-accounts', 'namespaces',
                        'runners', 'runner-pools', 'bus', 'tasks', 'bricks', 'runs', 'artifacts',
                        'stats') $$;
