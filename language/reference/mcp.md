# `mcp`

Publishes the workflow to model-driven clients as an MCP server at /mcp/{namespace}/{workflow}. The block is what makes the server exist: without it the endpoint answers 404, and with it but with an empty tool list it serves a server with nothing on it. Nothing is inferred from the graph, so exposing a workflow never leaks its internals, and publishing, renaming or withdrawing a tool is a commit like any other change.

## Keywords

- `/mcp`: Publishes the workflow to model-driven clients as an MCP server at /mcp/{namespace}/{workflow}. The block is what makes the server exist: without it the endpoint answers 404, and with it but with an empty tool list it serves a server with nothing on it. Nothing is inferred from the graph, so exposing a workflow never leaks its internals, and publishing, renaming or withdrawing a tool is a commit like any other change.
- `/mcp/name`: Server name shown to a client. Defaults to the workflow name, and exists so that a published interface need not follow an internal rename.
- `/mcp/description`: What the server is for, in one or two sentences. A model reads it before it reads any tool, so it is the sentence that decides whether the rest is ever looked at.
- `/mcp/tools`: The published tools. An empty list serves a server carrying nothing, which is not the same as declaring no mcp block at all.
- `/mcp/tools/*`: One tool on the workflow's MCP server. It is a view of the workflow's own boundary: an input is published as the tool's schema and an output as its result, and a call is a run like any other, attributed to the caller and subject to the namespace quota.
- `/mcp/tools/*/name`: Tool identifier, unique within the workflow and stable across commits because clients hold it. Renaming one is removing a tool and adding another.
- `/mcp/tools/*/title`: Label a client shows to a person in a list of tools, where the identifier would read as machinery.
- `/mcp/tools/*/description`: What the tool does, when to reach for it and what it refuses. This is the field a model actually acts on, and the one worth spending sentences on.
- `/mcp/tools/*/input`: The workflow input whose JSON Schema becomes the tool's inputSchema, used as it stands. There is no second schema to write and therefore no way for the two to drift apart, which also means the input it names has to carry a schema.
- `/mcp/tools/*/input/from`: Names the workflow input, never a step or a port, so that the graph stays free to change beneath a published tool.
- `/mcp/tools/*/input/from/input`: A workflow input declared in this file.
- `/mcp/tools/*/output`: The workflow output whose envelope becomes the result, carried as structuredContent with a text rendering beside it, and whose schema, when the output declares one, becomes the tool's outputSchema. Optional, because a tool may have an effect and return only that it succeeded, and an output carrying no schema is published with no outputSchema beside it.
- `/mcp/tools/*/output/from`: Names the workflow output, never a step or a port.
- `/mcp/tools/*/output/from/output`: A workflow output declared in this file.
- `/mcp/tools/*/mode`: Whether the call waits. sync holds the call until the run finishes and returns the output; async returns the run identifier at once, to be followed with the platform server's run.get.
- `/mcp/tools/*/timeout`: How long a sync call waits. It is capped at 120 seconds because clients time out long before a workflow gives up, and a workflow that cannot finish inside that has to be async. The ceiling is carried by the grammar of the value, so a timeout above it is refused here and not left to the validator; the companion rule, that an async tool carries no timeout at all, is stated on mode.
- `/mcp/tools/*/annotations`: The protocol's own hints, passed through to the client unchanged. They tell a client what to expect of a call, never what it is allowed to do: that is decided by the grants and not by a field in this file.
- `/mcp/tools/*/annotations/readOnlyHint`: Says the tool only reads, so a client may call it without asking anyone first.
- `/mcp/tools/*/annotations/idempotentHint`: Says calling twice with the same arguments has the same effect as calling once, which is what lets a client retry a call it is unsure about.
- `/mcp/tools/*/annotations/destructiveHint`: Says the tool can remove or overwrite something, so a client may want a confirmation it would not otherwise ask for.
- `/mcp/tools/*/annotations/openWorldHint`: Says the tool reaches systems outside this installation, so its result depends on something nobody here controls.
- `/mcp/tools/*/timeout`: How long a sync call waits, written as a duration and capped at 120 seconds. The ceiling sits in the grammar rather than in a sentence beside it, because a client times out long before a workflow gives up: 120s, 2m and 120000ms are the longest forms this accepts, and an hour or a day cannot be written at all. A workflow that cannot finish inside it is published as an async tool, which carries no timeout.

## Schema

`workflow.schema.json#/properties/mcp`:

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "name": {"$ref": "#/$defs/identifier"},
    "description": {"type": "string"},
    "tools": {"type": "array", "items": {"$ref": "#/$defs/mcpTool"}}
  }
}
```

`workflow.schema.json#/$defs/mcpTool`:

```json
{
  "type": "object",
  "required": ["name", "description", "input"],
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
    "input": {
      "type": "object",
      "required": ["from"],
      "additionalProperties": false,
      "properties": {
        "from": {
          "type": "object",
          "required": ["input"],
          "additionalProperties": false,
          "properties": {"input": {"$ref": "#/$defs/identifier"}}
        }
      }
    },
    "output": {
      "type": "object",
      "required": ["from"],
      "additionalProperties": false,
      "properties": {
        "from": {
          "type": "object",
          "required": ["output"],
          "additionalProperties": false,
          "properties": {"output": {"$ref": "#/$defs/portName"}}
        }
      }
    },
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

## Examples

`/mcp`:

```yaml
mcp:
  name: invoicing
  description: Issue invoices and check their status.
  tools:
  - name: create_invoice
    description: Issue one invoice for a customer.
    input:
      from:
        input: orders
    output:
      from:
        output: invoices
    mode: sync
```

`/mcp/name`:

```yaml
mcp:
  name: invoicing
```

`/mcp/description`:

```yaml
mcp:
  description: Issue invoices and check their status.
```

`/mcp/tools`:

```yaml
mcp:
  tools:
  - name: reconcile_month
    description: Reconcile a whole month. Long running; returns a run to follow.
    input:
      from:
        input: period
    mode: async
```

`/mcp/tools/*`:

```yaml
mcp:
  tools:
  - name: create_invoice
    title: Create an invoice
    description: Create one invoice for a customer and return its number and total. Rejects a customer whose VAT number does not validate.
    input:
      from:
        input: orders
    output:
      from:
        output: invoices
    mode: sync
    timeout: 60s
```

`/mcp/tools/*/name`:

```yaml
mcp:
  tools:
  - name: create_invoice
```

`/mcp/tools/*/name`:

```yaml
mcp:
  tools:
  - name: reconcile_month
```

`/mcp/tools/*/title`:

```yaml
mcp:
  tools:
  - title: Create an invoice
```

`/mcp/tools/*/description`:

```yaml
mcp:
  tools:
  - description: Create one invoice for a customer and return its number and total. Rejects a customer whose VAT number does not validate.
```

`/mcp/tools/*/input`:

```yaml
mcp:
  tools:
  - input:
      from:
        input: orders
```

`/mcp/tools/*/input/from/input`:

```yaml
mcp:
  tools:
  - input:
      from:
        input: period
```

`/mcp/tools/*/output`:

```yaml
mcp:
  tools:
  - output:
      from:
        output: invoices
```

`/mcp/tools/*/mode`:

```yaml
mcp:
  tools:
  - mode: sync
```

`/mcp/tools/*/mode`:

```yaml
mcp:
  tools:
  - mode: async
```

`/mcp/tools/*/timeout`:

```yaml
mcp:
  tools:
  - timeout: 60s
```

`/mcp/tools/*/timeout`:

```yaml
mcp:
  tools:
  - timeout: 120s
```

`/mcp/tools/*/annotations`:

```yaml
mcp:
  tools:
  - annotations:
      readOnlyHint: false
      idempotentHint: false
```

`/mcp/tools/*/annotations/readOnlyHint`:

```yaml
mcp:
  tools:
  - annotations:
      readOnlyHint: true
```

`/mcp/tools/*/annotations/readOnlyHint`:

```yaml
mcp:
  tools:
  - annotations:
      readOnlyHint: false
```

`/mcp/tools/*/annotations/idempotentHint`:

```yaml
mcp:
  tools:
  - annotations:
      idempotentHint: true
```

`/mcp/tools/*/annotations/idempotentHint`:

```yaml
mcp:
  tools:
  - annotations:
      idempotentHint: false
```

`/mcp/tools/*/annotations/destructiveHint`:

```yaml
mcp:
  tools:
  - annotations:
      destructiveHint: true
```

`/mcp/tools/*/annotations/destructiveHint`:

```yaml
mcp:
  tools:
  - annotations:
      destructiveHint: false
```

`/mcp/tools/*/annotations/openWorldHint`:

```yaml
mcp:
  tools:
  - annotations:
      openWorldHint: true
```

`/mcp/tools/*/annotations/openWorldHint`:

```yaml
mcp:
  tools:
  - annotations:
      openWorldHint: false
```

`/mcp/tools/*/timeout`:

```yaml
mcp:
  tools:
  - timeout: 500ms
```

`/mcp/tools/*/timeout`:

```yaml
mcp:
  tools:
  - timeout: 2m
```
