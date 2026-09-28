-- What a namespace knows of the images its workflows name: the digest each tag was pinned to, and
-- the brick manifest each image carries.
--
-- A git push is judged by the one validation agk push makes, and two things that validation needs
-- are read out of images: the digest a tag names, which its registry answers, and the manifest a
-- brick step is held to, which is a file inside the image. "A git push reaches no registry": the
-- installation pulls no image and asks no registry anything, so what agk push and agk validate read
-- through the daemon on the machine they run on is recorded here, by PUT /api/v1/{ns}/images, and
-- the pre-receive hook reads it back. A tag or a digest this holds nothing for is refused at the
-- push, naming agk push, which is what records it.
--
-- Per namespace rather than per workflow, since that is what a workflow's other stores are, its
-- secrets and its quotas among them, and two workflows of a namespace naming one image are held to
-- one manifest of it. And never across namespaces: what one namespace's pushers pinned a tag to is
-- no reason for another's to run it.

-- The repository a reference names, less its tag and its digest, as agk.ImageRepository reads it:
-- only a colon after the last slash begins a tag, since registry.example:5000/brick names a port.
create function image_repository(ref text) returns text
  language sql immutable
  as $$ select regexp_replace(split_part(ref, '@', 1), ':[^/:]*$', '') $$;

-- A reference as a registry can serve one: no whitespace, and at most 512 bytes. The distribution
-- specification holds a tag to 128 characters, and its clients a repository's name, its registry's
-- host included, to 255, so no reference a registry serves is longer, and the bound keeps each one
-- a key an index holds.
create function image_reference(ref text) returns boolean
  language sql immutable
  as $$ select ref ~ '^[^\s@]+(@sha256:[0-9a-f]{64})?$' and octet_length(ref) <= 512 $$;

-- One row per tag a workflow of the namespace names, with the digest it was last pinned to, which
-- is the digest a push naming the tag runs: "images by digest on a server: a tag is a mutable
-- pointer, and a version must determine what ran". The reference is written as the workflow
-- writes it, since that is what a step is resolved by; the digest is of the tag's own repository,
-- spelt as the tag spells it, as agk push has always recorded one in a version.
--
-- Pinned again to the same digest, the row is left as it was, so that who pinned it and when say
-- when the tag last moved and who moved it. A version keeps the digests its push ran for good, so a
-- pin moved later changes no version already recorded.
create table image_pins (
  namespace text not null references namespaces (name) on delete cascade,
  reference text not null constraint image_pins_reference check (image_reference(reference) and reference !~ '@'),
  pinned    text not null constraint image_pins_pinned check (
    image_reference(pinned) and pinned ~ '@' and split_part(pinned, '@', 1) = image_repository(reference)),
  pinned_by text not null check (pinned_by <> ''),
  pinned_at timestamptz not null default now(),
  primary key (namespace, reference)
);

-- One row per image a brick step of the namespace runs, named by its repository at its digest, with
-- the manifest read out of it, as the JSON document brick.Manifest keeps. A digest names one image
-- for good, and so one manifest: a tag written beside the digest is not part of the name, since a
-- pull does not read it. Recorded again with other bytes, which an agk writing the document
-- differently would send, the row takes them and who sent them.
create table brick_manifests (
  namespace   text not null references namespaces (name) on delete cascade,
  image       text not null constraint brick_manifests_image check (
    image_reference(image) and image ~ '@' and split_part(image, '@', 1) = image_repository(image)),
  manifest    bytea not null check (octet_length(manifest) > 0),
  recorded_by text not null check (recorded_by <> ''),
  recorded_at timestamptz not null default now(),
  primary key (namespace, image)
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

-- Filled from every version recorded before this, so that the images of the workflows v0.2 and v0.3
-- pushed as trees are known when each is first pushed with git: nothing is asked of an installation
-- at the upgrade. Each version keeps, in graph, the digest every tag it names was pinned to and the
-- manifest of every image a brick step runs, by the reference the step wrote; a tag is taken at the
-- digest the newest version naming it pinned, and a manifest from the newest version holding one,
-- each recorded by that version's author when it was pushed. A manifest kept by a tag no digest was
-- recorded for, which a version from before digests were kept holds, names no image and is left
-- out, as is anything a version holds that is not what these tables keep: an upgrade is never
-- refused over a row it could not copy. Written across every namespace, which is what the
-- installation scope is bound for.
select set_config('agentiik.scope', 'installation', true);

insert into image_pins (namespace, reference, pinned, pinned_by, pinned_at)
select distinct on (v.namespace, i.key) v.namespace, i.key, i.value, v.author, v.created_at
  from workflow_versions v
 cross join lateral jsonb_each_text(
         case when jsonb_typeof(v.graph -> 'images') = 'object' then v.graph -> 'images' else '{}' end) i
 where image_reference(i.key) and i.key !~ '@'
   and image_reference(i.value) and i.value ~ '@' and split_part(i.value, '@', 1) = image_repository(i.key)
   and v.author <> ''
 order by v.namespace, i.key, v.created_at desc, v.commit desc;

insert into brick_manifests (namespace, image, manifest, recorded_by, recorded_at)
select distinct on (namespace, image) namespace, image, decode(body, 'base64'), author, created_at
  from (select v.namespace, v.commit, v.author, v.created_at, m.value as body,
               case when m.key ~ '@' then m.key else v.graph -> 'images' ->> m.key end as named
          from workflow_versions v
         cross join lateral jsonb_each_text(
                 case when jsonb_typeof(v.graph -> 'manifests') = 'object' then v.graph -> 'manifests' else '{}' end) m) held
 cross join lateral (select image_repository(named) || '@' || split_part(named, '@', 2) as image) keyed
 where image_reference(named) and named ~ '@'
   and body ~ '^([A-Za-z0-9+/]{4})*([A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$' and body <> ''
   and author <> ''
 order by namespace, image, created_at desc, commit desc;
