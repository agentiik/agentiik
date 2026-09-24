-- A stopped task's exit code.
--
-- 0001 allowed a code on a task that succeeded or failed and on no other, reasoning that a task
-- stopped at its deadline or by a cancellation decided nothing. A stopped container exits all the
-- same, and how it exited is what a person reading the run wants to know: "a timed_out or cancelled
-- task carries an exit code wherever a container ran", 143 where it obeyed SIGTERM and 137 where it
-- was killed after the grace. The runner reports it, and the row now keeps it. The verdict still
-- reads no code off a stopped task, since the stop decided it.
--
-- A lost task still carries none, "because the point of lost is that there is no outcome to
-- report".
--
-- The constraint was written unnamed, as the third check of the table, and PostgreSQL named it
-- tasks_check2.
alter table tasks drop constraint tasks_check2;
alter table tasks add constraint tasks_exit_code_check
  check (exit_code is null or state in ('succeeded', 'failed', 'timed_out', 'cancelled'));
