# `mcp`

Publishes the workflow as one tool to model-driven clients: one block, one tool, whose arguments are the workflow's inputs and whose result is optionally one of its outputs. Whether a client is offered it is decided by whoever adds the workflow to a collection, so the block says what the tool is and never who may call it. Nothing is inferred from the graph, so publishing a workflow never leaks its internals, and publishing, renaming or withdrawing the tool is a commit like any other change.

## Keywords

- `/mcp`: Publishes the workflow as one tool to model-driven clients: one block, one tool, whose arguments are the workflow's inputs and whose result is optionally one of its outputs. Whether a client is offered it is decided by whoever adds the workflow to a collection, so the block says what the tool is and never who may call it. Nothing is inferred from the graph, so publishing a workflow never leaks its internals, and publishing, renaming or withdrawing the tool is a commit like any other change.
- `/mcp/name`: The tool's name, the workflow's name where none is written. It is stable across commits because clients hold it and send it back in every call: renaming it is withdrawing one tool and publishing another, in every collection that does not give the tool a name of its own.
- `/mcp/title`: Label a client shows to a person in a list of tools, where the name would read as machinery.
- `/mcp/description`: What the tool does, when to reach for it and what it refuses. This is the field a model actually acts on, and the one worth spending sentences on: a tool published without one is exactly the tool a model has no way to decide to call.
- `/mcp/output`: The name of one workflow output declared in this file, optional. Its envelope becomes the result, carried as structuredContent with a text rendering beside it, and its schema, where it declares one, the tool's outputSchema. Leaving it out publishes a tool that has an effect and returns only that it succeeded. It names an output and never a step or a port, so that the graph stays free to change beneath a published tool.
- `/mcp/mode`: Whether the call waits. sync holds the call until the run finishes and returns the output; async returns the run identifier at once, to be followed with run.get on the user's server.
- `/mcp/timeout`: How long a sync call waits. It is capped at 120 seconds because clients time out long before a workflow gives up, and a workflow that cannot finish inside that has to be async. The ceiling is carried by the grammar of the value, so a timeout above it is refused here and not left to the validator; the companion rule, that an async tool carries no timeout at all, is stated on mode.
- `/mcp/annotations`: The protocol's own hints, passed through to the client unchanged. They tell a client what to expect of a call, never what it is allowed to do: that is decided by the grants and not by a field in this file.
- `/mcp/annotations/readOnlyHint`: Says the tool only reads, so a client may call it without asking anyone first.
- `/mcp/annotations/idempotentHint`: Says calling twice with the same arguments has the same effect as calling once, which is what lets a client retry a call it is unsure about.
- `/mcp/annotations/destructiveHint`: Says the tool can remove or overwrite something, so a client may want a confirmation it would not otherwise ask for.
- `/mcp/annotations/openWorldHint`: Says the tool reaches systems outside this installation, so its result depends on something nobody here controls.
- `/mcp/timeout`: How long a sync call waits, written as a duration and capped at 120 seconds. The ceiling sits in the grammar rather than in a sentence beside it, because a client times out long before a workflow gives up: 120s, 2m and 120000ms are the longest forms this accepts, and an hour or a day cannot be written at all. A workflow that cannot finish inside it is published as an async tool, which carries no timeout.

## Schema

`workflow.schema.json#/properties/mcp`:

```json
{
  "type": "object",
  "required": ["description"],
  "additionalProperties": false,
  "dependentSchemas": {
    "mode": {
      "if": {"properties": {"mode": {"const": "async"}}, "required": ["mode"]},
      "then": {"not": {"required": ["timeout"]}}
    }
  },
  "properties": {
    "name": {"$ref": "#/$defs/identifier"},
    "title": {"type": "string"},
    "description": {"type": "string", "minLength": 1},
    "output": {"$ref": "#/$defs/portName"},
    "mode": {"enum": ["sync", "async"], "default": "sync"},
    "timeout": {"$ref": "#/$defs/mcpTimeout"},
    "annotations": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "readOnlyHint": {"type": "boolean"},
        "idempotentHint": {"type": "boolean"},
        "destructiveHint": {"type": "boolean"},
        "openWorldHint": {"type": "boolean"}
      }
    }
  }
}
```

`workflow.schema.json#/$defs/mcpTimeout`:

```json
{
  "type": "string",
  "pattern": "^(?:(?:[1-9][0-9]{0,4}|1[01][0-9]{4}|120000)ms|(?:[1-9][0-9]?|1[01][0-9]|120)s|[12]m)$"
}
```

`workflow.schema.json#/dependentSchemas/mcp`:

```json
{"properties": {"inputs": {"additionalProperties": {"required": ["schema"]}}}}
```

## Examples

`/mcp`:

```yaml
mcp:
  name: order_status
  title: Order status
  description: Return the state of one order and, once shipped, its tracking link. Takes an order number such as ORD-004211 and nothing else.
  output: status
  mode: sync
  timeout: 30s
  annotations:
    readOnlyHint: true
    idempotentHint: true
```

`/mcp`:

```yaml
mcp:
  description: 'Reconcile a whole month against the bank statements. Long running: returns a run to follow with run.get.'
  mode: async
```

`/mcp/name`:

```yaml
mcp:
  name: order_status
```

`/mcp/name`:

```yaml
mcp:
  name: create_invoices
```

`/mcp/title`:

```yaml
mcp:
  title: Order status
```

`/mcp/description`:

```yaml
mcp:
  description: Return the state of one order and, once shipped, its tracking link. Takes an order number such as ORD-004211 and nothing else.
```

`/mcp/output`:

```yaml
mcp:
  output: status
```

`/mcp/output`:

```yaml
mcp:
  output: invoices
```

`/mcp/mode`:

```yaml
mcp:
  mode: sync
```

`/mcp/mode`:

```yaml
mcp:
  mode: async
```

`/mcp/timeout`:

```yaml
mcp:
  timeout: 30s
```

`/mcp/timeout`:

```yaml
mcp:
  timeout: 120s
```

`/mcp/annotations`:

```yaml
mcp:
  annotations:
    readOnlyHint: false
    idempotentHint: false
```

`/mcp/annotations/readOnlyHint`:

```yaml
mcp:
  annotations:
    readOnlyHint: true
```

`/mcp/annotations/readOnlyHint`:

```yaml
mcp:
  annotations:
    readOnlyHint: false
```

`/mcp/annotations/idempotentHint`:

```yaml
mcp:
  annotations:
    idempotentHint: true
```

`/mcp/annotations/idempotentHint`:

```yaml
mcp:
  annotations:
    idempotentHint: false
```

`/mcp/annotations/destructiveHint`:

```yaml
mcp:
  annotations:
    destructiveHint: true
```

`/mcp/annotations/destructiveHint`:

```yaml
mcp:
  annotations:
    destructiveHint: false
```

`/mcp/annotations/openWorldHint`:

```yaml
mcp:
  annotations:
    openWorldHint: true
```

`/mcp/annotations/openWorldHint`:

```yaml
mcp:
  annotations:
    openWorldHint: false
```

`/mcp/timeout`:

```yaml
mcp:
  timeout: 500ms
```

`/mcp/timeout`:

```yaml
mcp:
  timeout: 60s
```

`/mcp/timeout`:

```yaml
mcp:
  timeout: 2m
```
