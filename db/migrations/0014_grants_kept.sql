-- Every grant a dispatch was handed out with, and each of them still redeemable.
--
-- 0007 kept one grant per task row, and issuing another replaced it. A pass that publishes a task
-- and then fails to record the dispatch leaves the task to be planned again, and the pass that
-- plans it issues the same row a new grant and publishes it once more. The first message may
-- already be on the queue, and inside the duplicate window it is the only one there: the stream
-- deduplicates a task on its row and answers the second publish that it has it already. Replacing
-- the grant left the one message a runner could take carrying a credential that opened nothing, so
-- the runner could only report a task that never reached a container, and the task failed for
-- something the controller did.
--
-- So a grant, once issued, is never replaced, and a row holds one for every message the controller
-- prepared for it. That is still "the per-task grant": each is scoped to its one task and expires
-- with it, and the first to be redeemed binds the task, so a second message delivered to another
-- machine is answered as somebody else's work rather than run twice. The hash tells them apart,
-- since the clear value is 256 bits minted afresh each time.
alter table task_grants drop constraint task_grants_pkey;
alter table task_grants add primary key (namespace, task_id, hash);
