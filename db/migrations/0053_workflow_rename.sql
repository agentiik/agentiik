-- A workflow renamed: "A rename answers 200 and carries everything at once: versions, refs, runs,
-- grants and triggers answer at the new name from the answer on, the old name is free."
--
-- Every row naming a workflow names it by its namespace and its name, through a foreign key onto
-- workflows, or onto workflow_versions for a run; each key is made to carry an update of the name
-- through, so that a rename is one statement on the workflow's row, in one transaction, under the
-- lock every writer of the repository takes, and no row is left naming the old name. The packs name
-- their repository by its key, which a rename leaves alone, and are not among them.
--
-- The keys are found by what they reference rather than written out by name, since the names
-- PostgreSQL gave them are its own, and each is dropped and made again with ON UPDATE CASCADE beside
-- whatever it already carried.
do $$
declare k record;
begin
  for k in
    select c.conrelid::regclass as tbl, c.conname, pg_get_constraintdef(c.oid) as def
    from pg_constraint c
    where c.contype = 'f'
      and c.confrelid in ('workflows'::regclass, 'workflow_versions'::regclass)
      and c.conrelid in ('workflow_versions'::regclass, 'runs'::regclass, 'grants'::regclass,
                         'workflow_refs'::regclass, 'image_pins'::regclass, 'brick_manifests'::regclass)
      and pg_get_constraintdef(c.oid) not like '%ON UPDATE%'
  loop
    execute format('alter table %s drop constraint %I', k.tbl, k.conname);
    execute format('alter table %s add constraint %I %s ON UPDATE CASCADE', k.tbl, k.conname, k.def);
  end loop;
end
$$;
