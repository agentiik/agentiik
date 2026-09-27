-- A TOTP generator: the time step its code was last accepted at, so that no code is accepted twice,
-- and the rule that it exists beside a password alone. New in v0.3.0, and nothing to carry over at
-- the upgrade: no user held a TOTP before it.

-- A code is accepted one step either side of the API's clock, which makes each good for up to a
-- minute and a half; RFC 6238 §5.2 asks that a code accepted once is not accepted again, and a code
-- of a step before one accepted is refused with it. The step is kept rather than the code, since a
-- later code and an earlier one are told apart by their steps alone, and a sign-in writes it in the
-- statement that checks it, so that of two sign-ins with one code the second finds it spent. Absent
-- until a code has been accepted, and on every row that is not a TOTP's.
alter table credentials
  add column totp_step bigint,
  add constraint credentials_totp_step check (totp_step is null or (type = 'totp' and totp_step > 0));

-- "TOTP exists only alongside a password": a TOTP is refused to a user holding no password, and goes
-- with the password however the password goes, removed by its user or deleted with every other when
-- passwords are forbidden, so that an installation forbidding passwords holds no TOTP either. Held
-- here rather than by whoever removes a password, since every way a password goes is then covered,
-- those written later included.
--
-- The password's row is locked while a TOTP is written beside it, so that a password removed at the
-- same moment either waits for the TOTP and takes it with it, or is gone and the TOTP refused. A
-- login no user holds is left to the foreign key, which refuses it for what it is.
create function agentiik_totp_beside_a_password() returns trigger
  language plpgsql
  as $$
begin
  if tg_op = 'INSERT' then
    if exists (select from users where login = new.login) then
      perform from credentials where login = new.login and type = 'password' for share;
      if not found then
        raise exception 'a TOTP generator is enrolled beside a password, and % holds none', new.login
          using errcode = 'check_violation', constraint = 'credentials_totp_beside_a_password';
      end if;
    end if;
    return new;
  end if;
  delete from credentials where login = old.login and type = 'totp';
  return old;
end
$$;

create trigger credentials_totp_needs_a_password before insert on credentials
  for each row when (new.type = 'totp') execute function agentiik_totp_beside_a_password();

create trigger credentials_totp_goes_with_the_password after delete on credentials
  for each row when (old.type = 'password') execute function agentiik_totp_beside_a_password();
