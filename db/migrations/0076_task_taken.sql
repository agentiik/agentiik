-- When a runner took a dispatch: the instant its redemption bound the task to it (db.Wide.Redeem),
-- the first time, since a runner that lost the answer redeems again and took the task once. The
-- binding itself, runner, says only whose it is; the installation's activity counts the tasks a
-- runner holds apart from those handed to the bus and waiting for a slot, bucket by bucket, and a
-- bucket needs the instant one became the other. A runner takes no more than its concurrency, so the
-- tasks held are what the ready runners' slots are set against, and those waiting are the rest.
--
-- Nothing to fill in at the upgrade: a task bound before this has no instant, and counts as held
-- from when it was handed to the bus, as every task in flight counted before.
alter table tasks add column taken_at timestamptz;
