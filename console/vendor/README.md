# What the console copies from the project's other repositories

Each file is copied as its repository publishes it and never edited here. A new copy is a commit that names where it came from, and at a release each is the copy of that release's tag, since every repository carries the same version.

| File | From | At |
| --- | --- | --- |
| `openapi.json`, `wire.schema.json` | `agentiik/schemas`, the document the console's client is generated from and the schema its records refer to, taken from its branch `mcp-pins-and-bulk`, which describes `resolve` on the images route, since v0.7.3 is not tagged there yet | `d582b58` |
| `envelope.schema.json` | `agentiik/schemas`, the schema the envelopes the console reads refer to, taken from the same commit | `d582b58` |
| `workflow.schema.json` | `agentiik/schemas`, the schema a workflow file is held to, which the console checks `agentiik.yaml` against before anything is sent (`src/lib/workflow-check.ts`), taken from the same commit | `d582b58` |
| `tokens.css` | `agentiik/design`, the tokens `tools/build.py` generates from `tokens.json` | `v0.7.0` |
| `tokens.json` | `agentiik/design`, the token file itself, which `agk console`'s palette is generated from (`cmd/agk/internal/console/palette.go`), so that the terminal and the browser are drawn from one copy at one version | `v0.7.0` |
| `icons/` | `agentiik/design`, the icon set and its index | `v0.7.0` |
| `../public/mark.svg` | `agentiik/design`, `mark/mark-square-light.svg`, the favicon | `v0.7.0` |
