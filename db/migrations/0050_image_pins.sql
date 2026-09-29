-- What a workflow repository's pushes are judged against beyond their own tree: the digest each
-- image tag its steps name is pinned to, and the brick manifest of each image a brick step runs.
--
-- A git push carries neither, and the hook reaches no registry to find them: agk push resolves each
-- tag where the image is, on the pusher's own Docker daemon, reads each brick's /agk/brick.yaml
-- there, and records both here before it pushes with git, as the tree push records those of each
-- version it makes. A plain git push then passes where every tag it names is pinned and every brick
-- image it runs has a manifest here, and is refused where one is missing.
--
-- Kept per repository rather than per namespace. A pin decides which bytes the next version of a
-- workflow runs, with that workflow's secrets. Held per namespace, an editor granted workflow:write
-- on one workflow alone could re-pin a tag for all of them, and the next push of another workflow,
-- by its own editor, would run that image with the other workflow's secrets: a widening across
-- workflows that a grant on one workflow exists to prevent. Per repository, writing a pin takes
-- what registering a version of that workflow takes, and reaches nothing else. A manifest the same:
-- whoever records it can only be believed for the workflow they may push to.

-- A reference is at most 384 bytes: the 255 characters clients hold a registry's host and a
-- repository's name to together, a colon, and a tag of at most 128 characters, both from the OCI
-- distribution specification. A reference by digest, 7 characters of sha256: and 64 of hexadecimal
-- after the repository and an @, is shorter. Both are printable ASCII, as the OCI grammar of a name
-- and a tag is, so that a log line or a terminal reads nothing as something else and a bound in
-- bytes is the same bound in characters. An image by digest is kept less any tag written beside the
-- digest, which the code writing a row strips: the digest is what is pulled.
create function image_reference(reference text) returns boolean
  language sql immutable
  as $$
    select reference ~ '^[\x21-\x3f\x41-\x7e]+$' and octet_length(reference) <= 384
  $$;

create function image_digest(image text) returns boolean
  language sql immutable
  as $$
    select image ~ '^[\x21-\x3f\x41-\x7e]+@sha256:[0-9a-f]{64}$' and octet_length(image) <= 384
  $$;

-- One row per tag a step of the repository names, and the reference by digest it is pinned to, of
-- the tag's own repository, which the code that writes a row checks. A pin moves: a tag is a
-- mutable pointer, and pinning it again is what an agk push does once the tag has moved.
create table image_pins (
  namespace text not null,
  workflow  identifier not null,
  reference text not null constraint image_pins_reference check (image_reference(reference)),
  image     text not null constraint image_pins_image check (image_digest(image)),
  pinned_by text not null check (pinned_by <> ''),
  pinned_at timestamptz not null,
  primary key (namespace, workflow, reference),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade
);

-- One row per image a brick step of the repository runs, by digest, with the manifest read out of
-- it: the file the image holds at /agk/brick.yaml, byte for byte, rather than the document a version
-- keeps of it, which lifts a parameter's boolean required out and so could not hold a step to the
-- parameters it must supply. Recorded again with other bytes, it is replaced: the runner reads the
-- manifest out of the image itself when a step runs, so what is kept here is only what a push is
-- judged against, and one recorded wrong is put right by recording it again. At most 256 KiB, sixty
-- times the largest manifest of the standard catalog, so that one with a large parameter schema fits
-- and a row stays a row.
create table brick_manifests (
  namespace   text not null,
  workflow    identifier not null,
  image       text not null constraint brick_manifests_image check (image_digest(image)),
  manifest    bytea not null check (octet_length(manifest) between 1 and 262144),
  recorded_by text not null check (recorded_by <> ''),
  recorded_at timestamptz not null,
  primary key (namespace, workflow, image),
  foreign key (namespace, workflow) references workflows (namespace, name) on delete cascade
);

do $$
declare t text;
begin
  foreach t in array array['image_pins', 'brick_manifests']
  loop
    execute format('alter table %I enable row level security', t);
    execute format('alter table %I force row level security', t);
    execute format($f$
      create policy %I_by_namespace on %I
        using (namespace = agentiik_namespace() or agentiik_installation())
        with check (namespace = agentiik_namespace() or agentiik_installation())
    $f$, t, t);
  end loop;
end
$$;
