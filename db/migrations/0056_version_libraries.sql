-- A library, and the files a workflow include read, held in graph.
--
-- "A workflow include reads the other repository's root agentiik.yaml, written as a fragment: a
-- library repository, which its own hook validates as a fragment and nothing runs." A library's
-- commit is recorded as a version all the same, marked library, so that a ref of it names a
-- version as every other ref does, a workflow include resolves to what its hook accepted rather
-- than to a commit nobody judged, and the tree route serves its files to the agk resolving an
-- include before a push.
--
-- And a version including one keeps what it read of it, the commit its ref resolved to and the
-- files of that commit resolution read, beside the files it included of its own tree. That is
-- what its graph is rebuilt from, with no repository in reach: the library may since have moved
-- its tag, been renamed or been deleted, and a run of the version has to decide as it did.
--
-- In graph rather than columns of their own, for the reason 0020 gave for the digests: nothing
-- reads them apart from what the graph is rebuilt from, save the listing of versions, which reads
-- the one flag. A row written before this includes no other repository, since a workflow include
-- was refused until now, and is no library.

comment on column workflow_versions.graph is
  'What it takes to rebuild this version with no tree, no other repository and no registry: the entry point, its includes, the files each workflow include read at the commit its ref resolved to, the brick manifests, the digest each image named by a tag was resolved to when it was pushed, and whether the commit is a library, which nothing runs. Written by the API when a version is pushed, read by the controller when a run of it is decided.';
