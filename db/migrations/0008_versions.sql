-- What a version actually holds, which is not what this column was first described as.
--
-- "The resolved graph, which is what the evaluator produced for this commit and what a run is
-- planned from." The intent was right and the word was wrong: nothing in the engine can write a
-- resolved graph down. A graph.Graph holds unexported state, a graph.Workflow holds the document
-- it was parsed from so that a refusal can name a line, and neither has a serialised form. The
-- column was not null and no Go could fill it.
--
-- What a version has to be is reconstructible without the repository, for ever, because "A
-- version is a commit ... names exactly one tree, permanently" and a branch that moves afterwards
-- must change nothing about a run already pinned to it. So what is stored is what it takes to
-- rebuild: the entry point as it was at that commit, every file it included, and the manifest of
-- every image it names. Loading those back gives the same workflow, resolved the same way, with
-- no tree and no registry in reach.
--
-- The alternative was to teach package graph to write a resolved workflow out as a document, which
-- is the more literal reading and a good deal more code: thirty exported types, a marshaller for
-- every duration, enumeration and embedded schema, and a round trip to hold them all to. It is
-- worth doing one day, because a resolved workflow is a thing a person should be able to read.
-- It is not worth doing to fill a column, and a column that says "graph" while holding something
-- else would be the worse of the two mistakes.

comment on column workflow_versions.graph is
  'What it takes to rebuild this version with no tree and no registry: the entry point, its includes and the brick manifests. Written by the API when a version is pushed, read by the controller when a run of it is decided.';
