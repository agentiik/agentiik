-- What a user says of themself beside their display name, and their photo: PATCH /api/v1/me and PUT
-- /api/v1/me/avatar, set by the user alone. New in v0.6.0, and nothing to carry over at the upgrade:
-- every account starts with an empty profile and no photo.
--
-- Each field is text that is never null, the empty string being a field left unsaid, so that
-- clearing one and never having set it are the same row and a reader needs no second spelling of
-- nothing. The bounds are the API's, held here as well so that a writer that is not the API cannot
-- store what the API would never answer: a name, a title and a location of 128 characters are a
-- line on a card, a time zone of 64 is twice the longest name the IANA database holds, and a bio of
-- 280 is a few sentences, the length a short post has, rather than a page.
--
-- The photo is the PNG the API re-encoded, at most 512 by 512, kept in the row rather than in the
-- object store: it is read by its owner and by administrators alone, and an object of its own would
-- be a second thing to remove with the user, which the row's own removal does now. Two mebibytes is
-- more than any PNG of 512 by 512 the encoder writes, eight bits a channel. avatar_updated_at is when
-- it was last set, null with it, which the console adds to the photo's address so that a cache
-- keeping the old one is never asked for it again.
alter table users
  add column given_name        text not null default '' check (length(given_name) <= 128),
  add column family_name       text not null default '' check (length(family_name) <= 128),
  add column title             text not null default '' check (length(title) <= 128),
  add column location          text not null default '' check (length(location) <= 128),
  add column timezone          text not null default '' check (length(timezone) <= 64),
  add column bio               text not null default '' check (length(bio) <= 280),
  add column avatar            bytea check (octet_length(avatar) between 1 and 2097152),
  add column avatar_updated_at timestamptz,
  add constraint users_avatar_updated_with_it check ((avatar is null) = (avatar_updated_at is null));
