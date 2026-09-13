-- The controller's term, which is what makes the election safe.
--
-- "The active controller holds a session-level advisory lock, not a lease with an expiry",
-- and that alone is not enough: "Because a partitioned former holder may not yet know it has
-- lost, every controller write carries the lock's acquisition counter as a fencing token and
-- a write bearing an older counter is refused. Election alone would not be safe; the fencing
-- token is what makes it so."
--
-- PostgreSQL does not hand out an acquisition counter for an advisory lock, so the counter is
-- kept here and raised by whoever takes the lock. One row, for ever: the table is a counter
-- and not a history, and a second row would be a second controller.

create table controller_term (
  -- The primary key of a table with one row. A boolean that can only be true is the shortest
  -- way to say once and have the database hold it. Spelled sole because only is a reserved
  -- word here: select ... from only t is how PostgreSQL says do not descend into the
  -- children of an inherited table.
  sole        boolean primary key default true check (sole),
  token       bigint not null default 0 check (token >= 0),
  -- Who holds it, for a person reading the table during an incident. Nothing depends on it:
  -- a name is what an operator gave a process, and two processes can be given one name.
  holder      text,
  acquired_at timestamptz
);

insert into controller_term (sole) values (true);

-- The term is the installation's and belongs to no namespace, so it has no policy, the same
-- way namespaces and runners have none.
