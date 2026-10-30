# `ports`

The output ports this step publishes. For a brick they must be a subset of the ports its manifest declares; a script has no manifest to read them from, so there they have to be written out. A declared port the container never writes publishes an empty envelope, which is not an error.

## Keywords

- `/steps/*/outputs`, `/.*/outputs`: The output ports this step publishes. For a brick they must be a subset of the ports its manifest declares; a script has no manifest to read them from, so there they have to be written out. A declared port the container never writes publishes an empty envelope, which is not an error.
- `/outputs/*/from/port`, `/on/webhook/*/output`, `/mcp/output`, `/steps/*/needs/*/port`, `/steps/*/needs/*/as`, `/steps/*/outputs/*`, `/.*/needs/*/port`, `/.*/needs/*/as`, `/.*/outputs/*`: The name of a port, or of a workflow output: an identifier of at most 250 characters, since each becomes the file <name>.json, which a filesystem holds to 255 with its suffix. It is the grammar and the bound the brick manifest declares its ports on, so that every port a manifest declares can be named here.
- `/steps/*/inputs`, `/.*/inputs`: Feeds an input port from a workflow input or from an expression, creating no dependency on another step. It is how a step reads what the run was started with rather than what another step produced. A port carries a batch of items, so what feeds it is an object, which is one item, or a list of objects, one item each. A number, a string or a boolean is no item: it is written under a key of an object, count: { value: "${{ workflow.inputs.count }}" }, since the key an item's data holds it under is the author's to choose, and validation refuses a literal that is no batch and a workflow input whose schema declares a scalar type. On a step that calls a workflow, it writes the called workflow's inputs instead, which take a value of any type.

## Schema

`workflow.schema.json#/$defs/step/properties/inputs`:

```json
{"type": "object", "propertyNames": {"$ref": "#/$defs/portName"}, "additionalProperties": {}}
```

`workflow.schema.json#/$defs/step/properties/outputs`:

```json
{"type": "array", "minItems": 1, "uniqueItems": true, "items": {"$ref": "#/$defs/portName"}}
```

`workflow.schema.json#/$defs/portName`:

```json
{"$ref": "#/$defs/identifier", "maxLength": 250}
```

## Examples

`/steps/*/outputs`:

```yaml
steps:
  normalize:
    outputs:
    - ok
    - rejected
```

`/steps/*/outputs`:

```yaml
steps:
  normalize:
    outputs:
    - out
    - error
```

`workflow.schema.json#/$defs/portName`:

```yaml
out
```

`workflow.schema.json#/$defs/portName`:

```yaml
error
```

`workflow.schema.json#/$defs/portName`:

```yaml
rejected
```

`workflow.schema.json#/$defs/portName`:

```yaml
invoices
```

`/steps/*/inputs`:

```yaml
steps:
  normalize:
    inputs:
      orders: ${{ workflow.inputs.orders }}
      customers: ${{ workflow.inputs.customers }}
```

`/steps/*/inputs`:

```yaml
steps:
  normalize:
    inputs:
      count:
        value: ${{ workflow.inputs.count }}
```
