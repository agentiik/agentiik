-- The runs of one namespace, newest first.
--
-- max_runs_per_hour is "the runs created in the last 60 minutes, a sliding count", and it is counted
-- at every run a namespace with the quota creates, under a lock that holds the next creation until
-- the count is done. The indexes runs already has lead with the state or the workflow, so without
-- this the count would read every run the namespace ever created, and the lock would be held for as
-- long as that takes.
create index runs_by_creation on runs (namespace, created_at desc);
