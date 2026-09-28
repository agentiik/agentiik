-- A session is opened by a credential of its user, and by nothing else. An enrolment code opened
-- one until the passkey ceremonies arrived; since then the code travels in the registration's
-- options and is spent when the passkey is recorded, which opens a full session, and no route opens
-- a session with a code. What such a session could do lasted no longer than its code's hour, so any
-- row left is over, and goes with the column that named its code.
delete from sessions where enrolment_code is not null;

alter table sessions drop constraint sessions_opened_by;
alter table sessions drop column enrolment_code;
alter table sessions alter column credential set not null;
