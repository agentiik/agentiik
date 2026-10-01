# What the console copies from the project's other repositories

Each file is copied as its repository publishes it and never edited here. A new copy is a commit that names where it came from, and at a release each is the copy of that release's tag, since every repository carries the same version.

| File | From | At |
| --- | --- | --- |
| `openapi.json`, `wire.schema.json`, `envelope.schema.json` | `agentiik/schemas`, the document the console's client is generated from and the schemas its records and envelopes refer to | `e4222bb`, on the way to v0.6.0 |
| `workflow.schema.json` | `agentiik/schemas`, the schema a workflow file is held to, which the console checks `agentiik.yaml` against before anything is sent (`src/lib/workflow-check.ts`) | `e4222bb`, on the way to v0.6.0 |
| `tokens.css` | `agentiik/design`, the tokens `tools/build.py` generates from `tokens.json` | `a2e4225`, on the way to v0.6.0 |
| `icons/` | `agentiik/design`, the icon set and its index | `a2e4225`, on the way to v0.6.0 |
| `../public/mark.svg` | `agentiik/design`, `mark/mark-square-light.svg`, the favicon | `a2e4225`, on the way to v0.6.0 |
