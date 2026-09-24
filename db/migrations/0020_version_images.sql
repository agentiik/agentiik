-- The digest each image a version names by tag was resolved to when it was pushed, held in graph.
--
-- "Images by digest in production: a tag is a mutable pointer, and a commit must determine what
-- ran." A version whose steps name ghcr.io/acme/agk-invoice:1.4.0 ran whatever that tag pointed at
-- on the host that pulled it, at the moment it pulled it: two shards of one step could run two
-- images, and a replay six months on a third. The task message cannot carry a tag either, since
-- the wire's imageRef is name@sha256:<hex>. So agk push resolves every tag to the digest its
-- registry serves, the version records it, and the graph is rebuilt with the digest in the tag's
-- place: every run of the version names the same bytes.
--
-- In graph rather than a column of its own, and 0012 is the contrast. The tree is what a container
-- is given and is read on its own by a redemption; the digests are part of what the graph is
-- rebuilt from, with no tree and no registry in reach, exactly as the manifests are, and nothing
-- reads them on their own. Settled at the first push of a commit like the manifests: a second push
-- of the same commit after the tag moved changes nothing a run of it names.
--
-- Nothing to add to the table, then, and nothing to fill for a row written before this: such a
-- version names its images as its document wrote them, and a tag there is refused when the graph
-- is rebuilt rather than handed to a runner, which would refuse it in turn.

comment on column workflow_versions.graph is
  'What it takes to rebuild this version with no tree and no registry: the entry point, its includes, the brick manifests, and the digest each image named by a tag was resolved to when it was pushed. Written by the API when a version is pushed, read by the controller when a run of it is decided.';
