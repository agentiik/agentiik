-- What a controller needs to decide a run twice and get the same answer.
--
-- "Failover is a state resume, never a rebuild: the state lives in the database, not in the
-- process." The evaluator's own state is what that sentence is about, and package graph says
-- the same thing from its side: "load the State, call Next, get the Plan the instance that
-- died would have got. A state that could only be rebuilt by replaying the run would make
-- that sentence false."
--
-- So the document is stored whole. It is the run's state in the fullest sense of the word the
-- Storage chapter uses when it says runs holds "Identifier, version, state, trigger,
-- triggering principal, timestamps, workflow inputs and outputs", and the rows this file
-- already fills, steps and tasks, are a projection of it written in the same transaction for
-- everything that queries rather than decides.
--
-- With one surgery, which the chapter forces: "Envelopes and logs are not stored in the
-- database: it keeps only their digests and URIs." The evaluator's state carries envelopes
-- inline, on every published port and on every shard of every step, and a ten thousand shard
-- fan-out at envelope_max_bytes would be forty gigabytes in a jsonb column rewritten on every
-- decision. Every envelope is therefore lifted out to the object store and replaced by its
-- digest before the document is written, and put back when it is read. Package controller
-- carries that half; what is here is a column holding digests.

alter table runs
  -- The evaluator's state, with the envelopes lifted out. Null until the run starts, because
  -- a run waiting on a concurrency lock or on quota has not been evaluated yet and a document
  -- for it would be a run that had started while saying it had not.
  add column evaluation jsonb,

  -- State.Seq, which "counts the decisions taken against this state ... what makes two
  -- writers of one run detectable rather than silent". Every write of the document carries
  -- the number it was read at and is refused if the row has moved, so two controllers each
  -- believing they hold the term cannot interleave decisions. The fencing token refuses the
  -- second one first; this refuses it again, for the case where both are the same controller
  -- deciding one run twice at once.
  add column seq integer not null default 0 check (seq >= 0),

  -- Plan.Wake: the moment to ask again, and what a sweep orders by. "Wake is zero when
  -- nothing waits on the clock. It is not zero when something does, and two things do: a
  -- retry backoff ... and the root timeout". Null is that zero.
  add column wake_at timestamptz;

-- The sweep's index. A run worth looking at is one that has not finished and whose clock has
-- come round, and there are far more finished runs than unfinished ones.
create index runs_actionable on runs (wake_at)
  where state in ('queued', 'running', 'waiting');

alter table tasks
  -- When the task's message was published on the bus, and null until it was.
  --
  -- This is the outbox, and it is here because the database is the record and the bus is a
  -- courier. A message published before the transaction commits is a task on a queue that no
  -- row accounts for and that nothing can recall; a row committed before its message goes is
  -- a task the sweep finds and publishes again, which the idempotency key makes free. So the
  -- order is commit, then publish, then stamp, and a row stuck without a stamp is the one
  -- case the sweep exists to repair.
  add column published_at timestamptz;

create index tasks_unpublished on tasks (namespace, run_id)
  where published_at is null and state = 'pending';
