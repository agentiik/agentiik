# The round-trip corpus

Every workflow file `agentiik/schemas` pins what its schemas accept and refuse with, copied as it publishes them under `fixtures/` at `e4222bb`, on the way to v0.6.0, and named after where they come from: `workflow-valid-minimal.yaml` is `fixtures/workflow/valid/minimal.yaml`. `index.json` is that repository's `fixtures/index.json`, which says of each refused file whether the schema refuses it or a rule over the whole graph does.

`tests/yaml-tree.test.ts` reads each of them, edits it and fails the build on any byte that changes beyond the edit, and holds the console's check against `workflow.schema.json` to the refusals the index gives the schema. A new copy is a commit that names the commit it was copied from, as `vendor/README.md` asks of the vendored files.
