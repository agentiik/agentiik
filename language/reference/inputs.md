# `inputs`

Named values a run is started with, each validated against its schema before any step runs. They are the workflow's own boundary, which is what a trigger fills and what an MCP tool publishes; the graph behind them is free to change without callers noticing.

## Keywords

- `/inputs`: Named values a run is started with, each validated against its schema before any step runs. They are the workflow's own boundary, which is what a trigger fills and what an MCP tool publishes; the graph behind them is free to change without callers noticing.
- `/inputs/*/schema`, `/outputs/*/schema`: A JSON Schema 2020-12 document describing a value, either an object of keywords or a boolean. A $ref pointing into the repository tree keeps the schema beside the workflow and versioned with the same commit. The brick manifest calls the same thing by the same name and accepts the same two shapes, so a schema written on a port and a schema written on a workflow boundary are one kind of document.
- `/inputs/*`: One named input of the workflow: what it must look like, whether a run can start without it, and what stands in when it is absent.
- `/inputs/*/schema`: What the value has to look like for the run to start. An input without one is accepted as it comes and cannot be published as an MCP tool input, since a tool has to advertise an inputSchema.
- `/inputs/*/required`: Refuses a run that does not supply the input, rather than letting every step downstream discover the absence for itself.
- `/inputs/*/default`: Value used when the run supplies none. It is what makes an optional input ordinary: the graph reads one value and never tests for absence.

## Schema

`workflow.schema.json#/properties/inputs`:

```json
{
  "type": "object",
  "propertyNames": {"$ref": "#/$defs/identifier"},
  "additionalProperties": {"$ref": "#/$defs/workflowInput"}
}
```

`workflow.schema.json#/$defs/workflowInput`:

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "schema": {"$ref": "#/$defs/jsonSchema"},
    "required": {"type": "boolean", "default": false},
    "default": {}
  }
}
```

`workflow.schema.json#/$defs/jsonSchema`:

```json
{"type": ["object", "boolean"]}
```

## Examples

`/inputs`:

```yaml
inputs:
  orders:
    schema:
      $ref: ./schemas/order.json
    required: true
  customers:
    schema:
      $ref: ./schemas/customer.json
    default: []
```

`workflow.schema.json#/$defs/jsonSchema`:

```yaml
$ref: ./schemas/order.json
```

`workflow.schema.json#/$defs/jsonSchema`:

```yaml
type: object
required:
- customer_id
properties:
  customer_id:
    type: string
```

`workflow.schema.json#/$defs/jsonSchema`:

```yaml
true
```

`/inputs/*`:

```yaml
inputs:
  monthly-invoicing:
    schema:
      $ref: ./schemas/order.json
    required: true
```

`/inputs/*`:

```yaml
inputs:
  monthly-invoicing:
    schema:
      type: array
    default: []
```

`/inputs/*/schema`:

```yaml
inputs:
  monthly-invoicing:
    schema:
      $ref: ./schemas/order.json
```

`/inputs/*/required`:

```yaml
inputs:
  monthly-invoicing:
    required: true
```

`/inputs/*/required`:

```yaml
inputs:
  monthly-invoicing:
    required: false
```

`/inputs/*/default`:

```yaml
inputs:
  monthly-invoicing:
    default: []
```

`/inputs/*/default`:

```yaml
inputs:
  monthly-invoicing:
    default: 0
```

`/inputs/*/default`:

```yaml
inputs:
  monthly-invoicing:
    default: EUR
```
