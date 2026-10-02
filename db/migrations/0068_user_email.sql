-- The email address an administrator gives a user, at POST /api/v1/users and PATCH
-- /api/v1/users/{login}, and the display name made of the given and family names rather than
-- written. New in v0.6.0.
--
-- email is text that is never null, the empty string where no address was given, as each field of
-- a profile is, so that a reader needs no second spelling of nothing. It is at most 254 characters,
-- the longest address a mail path carries once its two angle brackets are counted, held here as
-- well as by the API so that a writer that is not the API cannot store what the API would never
-- answer. The rest of what the API holds an address to, one @ with something on each side and no
-- space or control character, is the API's alone: an address is held loosely, and a table refusing
-- what a later release comes to accept would be a migration to undo. Nothing sends anything to it;
-- it is there for people to reach the user by.
--
-- display_name is read and written by nothing from this release on: a user's display name is their
-- given name and their family name, or their login where they say neither, made as the user is read,
-- so that the name everybody reads and the names the user writes never say two things. The column
-- is kept, and no longer required, so that nothing an administrator wrote is lost at the upgrade; a
-- release to come may drop it. A name an administrator gave that is not the login is carried into the
-- given name, cut to its 128 characters, where the user says neither name, which is every user an
-- installation of v0.5.0 holds, so that everybody is still shown by the name they were: the user
-- splits it into their given and family names when they next write their profile.
alter table users
  add column email text not null default '' check (length(email) <= 254),
  alter column display_name drop not null;

update users
   set given_name = left(display_name, 128)
 where given_name = '' and family_name = '' and display_name <> login;
