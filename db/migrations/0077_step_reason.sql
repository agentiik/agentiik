-- Why a step reached its verdict, in the words of the rule that decided it, where the verdict alone
-- does not say: a step failed before any task started, its input port fed what is no batch, a merge
-- that could not be made or an if that did not evaluate; a step skipped by its if or its when; a
-- step cancelled with its run. The evaluator writes it on the step's state, which only the run's
-- document kept, so GET /api/v1/runs/{id}, run.get and the console answered failed with no attempt,
-- no task and no log, and nothing else: the one place that said why was a column nobody reads.
--
-- The controller writes it with the step's verdict, from the projection of each decision. Null on a
-- step that ran and ended as its tasks did, whose tasks and logs say why.
--
-- The upgrade fills it in from the document of every run that has one, which holds the reason of
-- every step it gave one, so that a run that failed before the upgrade says why after it. Written
-- across every namespace, which is what the installation scope is bound for; the update of a column
-- the live trigger does not watch tells no console.
alter table steps
  add column reason text check (reason <> '');

select set_config('agentiik.scope', 'installation', true);
update steps s set reason = r.evaluation->'state'->'steps'->s.step::text->>'reason'
from runs r
where r.namespace = s.namespace and r.id = s.run_id
  and r.evaluation->'state'->'steps'->s.step::text->>'reason' <> '';
