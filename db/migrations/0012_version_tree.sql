-- The repository a version is, named file by file, in a column of its own.
--
-- "The whole tree is mounted read-only at /agk/repo/ in every step", and a runner "never speaks
-- git and never holds a credential, because the controller resolves a commit to a tree and the
-- runner fetches content-addressed objects with the task's grant, exactly as it fetches an
-- artifact". So a version has to name its tree: every path, the digest of its bytes and its
-- mode, with the bytes themselves in the object store under that digest.
--
-- Not in graph, and 0008 is why. That column holds what it takes to rebuild the version with no
-- tree and no registry, and a test of it is that loading it back gives the same workflow. The
-- tree is not part of that: nothing rebuilds a graph from it, and it is what a container is
-- given rather than what a run is decided from. Folding it in would make graph a column that
-- says one thing and holds two, which is the mistake 0008 was written to undo. It is also read
-- on its own, once per task, by a redemption that has no use for the rest, and a column of its
-- own is one it can select without decoding a document it does not need.
--
-- Nullable, because a row written before this carries no tree and there is nothing true to fill
-- it with: the installation holds no clone to read one out of. A redemption for such a version
-- is refused with a sentence rather than answered with an empty /agk/repo, which would start a
-- container on a directory that looks like a repository and is not one.
alter table workflow_versions
  add column tree jsonb;

comment on column workflow_versions.tree is
  'Every file of the commit''s tree as {path, sha256, size, mode}, sorted by path, with the bytes in the object store under the digest and one artifact_objects reference per distinct digest keeping them there. Written by the API when a version is pushed, read by the API when a grant is redeemed. Null for a version recorded without one, which a redemption refuses.';
