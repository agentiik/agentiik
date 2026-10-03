# `repository`

The entry point of a workflow repository, agentiik.yaml at the root. It is the source of truth for what a workflow does, validated before a push is accepted, and the commit that carries it is the version. It is not the source of truth for who may do what: access grants live in the platform and never in this file.

## Keywords

- ``: The entry point of a workflow repository, agentiik.yaml at the root. It is the source of truth for what a workflow does, validated before a push is accepted, and the commit that carries it is the version. It is not the source of truth for who may do what: access grants live in the platform and never in this file.
- `/apiVersion`: Language version this file is written against. It is read before anything else, because it decides how every other key in the file is interpreted.
- `/kind`: Which document of the language this is. The same apiVersion also covers the brick manifest, so a reader has to be told which of the two it is holding.
- `/metadata`: What the workflow is called, which namespace owns it, and the labels it is found by. Ownership is not only naming: a workflow reaches the secrets, the quotas and the runner pools of the namespace that owns it, and of no other.
- `/metadata/name`: Name of the workflow inside its namespace. With the namespace it forms the <namespace>/<name> identity that a run, a grant and a git remote are all addressed by.
- `/metadata/namespace`: Namespace that owns the workflow. It is the unit of ownership and the coarse scope for access grants, so it decides which secrets can be named here and which runner pools may be selected.
- `/metadata/labels`: Key and value pairs used to group workflows and to find them again. They carry no meaning to the engine: nothing about scheduling, isolation or access is decided by a label.
- `/vars`: Values written once and read anywhere an expression is allowed, through the vars context. They keep a currency or a threshold in a single place instead of repeated across steps. They are not secrets: they sit in the file and are readable by anyone who can read the workflow. On a server the vars context also holds the variables the owning namespace shows this workflow, set through the API rather than in a file, and a name written here takes this file's value, as a name an including file writes takes that file's; agk run --local reaches no namespace and reads these alone.
- `/concurrency`: What happens when a trigger fires while a run of this workflow is still going. Groups are scoped to the namespace, so one namespace can never block or cancel another's runs.
- `/concurrency/group`: Name that runs queue behind. Two runs sharing a group never overlap, which is how a workflow that writes to one ledger is kept from writing to it twice at once.
- `/concurrency/cancel_in_progress`: Whether the arriving run cancels the one already going instead of waiting behind it. The run it replaces ends in the cancelled state.
- `/timeout`: Maximum duration of the whole run. It is not the step keyword of the same name: that one bounds a single shard, this one bounds everything the run does, from the first task to the last. A run still going when it expires is stopped and ends in the timed_out state, which is what makes that state reachable from a file rather than only from an operator cancelling something. It is capped by the namespace quota max_run_duration, exactly as retain is capped by max_retention_days: a timeout above the quota is held to it rather than refused, and a workflow writing none is bounded by the quota.
- `/metadata/name`, `/metadata/namespace`, `/outputs/*/from/step`, `/outputs/*/from/port`, `/on/webhook/*/output`, `/on/event/*/namespace`, `/mcp/name`, `/mcp/output`, `/secrets/*`, `/defaults/secrets/*`, `/steps/*/needs/*`, `/steps/*/needs/*/step`, `/steps/*/needs/*/port`, `/steps/*/needs/*/as`, `/steps/*/outputs/*`, `/steps/*/secrets/*`, `/.*/needs/*`, `/.*/needs/*/step`, `/.*/needs/*/port`, `/.*/needs/*/as`, `/.*/outputs/*`, `/.*/secrets/*`: A name written in the file and used as a key elsewhere: a workflow, a namespace, an input, an output, a port, a variable, a secret or a tool. Kept to letters, digits, hyphens and underscores so that the same name survives a URL, an environment variable and a tool list unchanged, and to 255 characters because no filesystem holds a longer name: a step becomes a directory and a secret a file. A port and a workflow output are held to 250, $defs/portName.
- `/metadata/namespace`, `/on/event/*/namespace`: The name of a namespace, an identifier other than a reserved word, at most 255 characters, the most a directory holds a name to. The API's first path segment decides the route, so auth, me, users, groups, service-accounts, namespaces, runners, runner-pools, bus, tasks, bricks, runs, artifacts, stats cannot name a namespace, and are refused at namespace and login creation from v0.3.0. stats is reserved from v0.3.0 for GET /api/v1/stats/pools, which v0.6.0 serves at that path alone, so that no namespace created meanwhile takes it. Nothing renames a namespace, so one made before v0.3.0 under it keeps its name and the API still serves it, every route of it, with nothing to do; only its creation is refused, which is why the engine still reads a workflow naming it.
- `/outputs/*/retain`, `/outputs/*/retain/for`, `/on/schedule/*/jitter`, `/defaults/timeout`, `/defaults/retain`, `/defaults/retain/for`, `/defaults/retry/backoff/base`, `/defaults/retry/backoff/max`, `/timeout`, `/steps/*/retry/backoff/base`, `/steps/*/retry/backoff/max`, `/steps/*/timeout`, `/.*/retry/backoff/base`, `/.*/retry/backoff/max`, `/.*/timeout`: A length of time written as a number and a unit: ms, s, m, h or d. Written rather than counted in seconds so that a file says 90d where a reader would otherwise have to work out what 7776000 means.
- `/vars`: The vars block, as the entry point and an included file both write it: names and values, read by expressions through the vars context.

## Schema

`workflow.schema.json#/properties/apiVersion`:

```json
{"const": "agentiik.dev/v1"}
```

`workflow.schema.json#/properties/kind`:

```json
{"const": "Workflow"}
```

`workflow.schema.json#/properties/metadata`:

```json
{
  "type": "object",
  "required": ["name"],
  "additionalProperties": false,
  "properties": {
    "name": {"$ref": "#/$defs/identifier"},
    "namespace": {"$ref": "#/$defs/namespace"},
    "labels": {
      "type": "object",
      "propertyNames": {"$ref": "#/$defs/identifier"},
      "additionalProperties": {"type": "string"}
    }
  }
}
```

`workflow.schema.json#/properties/concurrency`:

```json
{
  "type": "object",
  "minProperties": 1,
  "additionalProperties": false,
  "properties": {"group": {"type": "string", "minLength": 1}, "cancel_in_progress": {"type": "boolean"}}
}
```

`workflow.schema.json#/properties/timeout`:

```json
{"$ref": "#/$defs/duration"}
```

`workflow.schema.json#/properties/vars`:

```json
{"$ref": "#/$defs/vars"}
```

`workflow.schema.json#/$defs/vars`:

```json
{"type": "object", "propertyNames": {"$ref": "#/$defs/identifier"}, "additionalProperties": {}}
```

`workflow.schema.json#/$defs/identifier`:

```json
{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_-]*$", "maxLength": 255}
```

`workflow.schema.json#/$defs/namespace`:

```json
{
  "$ref": "#/$defs/identifier",
  "maxLength": 255,
  "not": {
    "pattern": "^(auth|me|users|groups|service-accounts|namespaces|runners|runner-pools|bus|tasks|bricks|runs|artifacts|stats)$"
  }
}
```

`workflow.schema.json#/$defs/duration`:

```json
{"type": "string", "pattern": "^[0-9]+(ms|s|m|h|d)$"}
```

## Examples

`agentiik.yaml`:

```yaml
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: first-run
  namespace: demo
outputs:
  greeting:
    from:
      step: greet
      port: out
steps:
  greet:
    image: alpine:3.21@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507
    script:
    - echo "hello from ${AGK_STEP}"
    outputs:
    - out
```

`agentiik.yaml`:

```yaml
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: monthly-invoicing
  namespace: finance
steps:
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d4a77b0c3e51d8a6f2b4c9e0a13d5f7b82c6e04a9d31b7f5c28e6a0b7e0
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs:
    - ok
    - rejected
```

`/apiVersion`:

```yaml
apiVersion: agentiik.dev/v1
```

`/kind`:

```yaml
kind: Workflow
```

`/metadata`:

```yaml
metadata:
  name: monthly-invoicing
  namespace: finance
  labels:
    domain: billing
```

`/metadata/name`:

```yaml
metadata:
  name: monthly-invoicing
```

`/metadata/name`:

```yaml
metadata:
  name: vat-reconciliation
```

`/metadata/namespace`:

```yaml
metadata:
  namespace: finance
```

`/metadata/namespace`:

```yaml
metadata:
  namespace: platform
```

`/metadata/labels`:

```yaml
metadata:
  labels:
    domain: billing
```

`/metadata/labels`:

```yaml
metadata:
  labels:
    domain: billing
    tier: critical
```

`/vars`:

```yaml
vars:
  currency: EUR
  dunning_days: 30
```

`/concurrency`:

```yaml
concurrency:
  group: monthly-invoicing
  cancel_in_progress: false
```

`/concurrency/group`:

```yaml
concurrency:
  group: monthly-invoicing
```

`/concurrency/group`:

```yaml
concurrency:
  group: invoicing-fr
```

`/concurrency/cancel_in_progress`:

```yaml
concurrency:
  cancel_in_progress: true
```

`/concurrency/cancel_in_progress`:

```yaml
concurrency:
  cancel_in_progress: false
```

`/timeout`:

```yaml
timeout: 30m
```

`/timeout`:

```yaml
timeout: 6h
```

`/timeout`:

```yaml
timeout: 24h
```

`workflow.schema.json#/$defs/identifier`:

```yaml
monthly-invoicing
```

`workflow.schema.json#/$defs/identifier`:

```yaml
create_invoice
```

`workflow.schema.json#/$defs/identifier`:

```yaml
orders
```

`workflow.schema.json#/$defs/namespace`:

```yaml
finance
```

`workflow.schema.json#/$defs/namespace`:

```yaml
platform
```

`workflow.schema.json#/$defs/duration`:

```yaml
500ms
```

`workflow.schema.json#/$defs/duration`:

```yaml
30s
```

`workflow.schema.json#/$defs/duration`:

```yaml
10m
```

`workflow.schema.json#/$defs/duration`:

```yaml
24h
```

`workflow.schema.json#/$defs/duration`:

```yaml
90d
```
