# `outputs`

The results a run is read by, each one a view of a single step port. Naming the boundary here is what lets a step be renamed, split or replaced without anything that consumes the workflow having to change.

## Keywords

- `/outputs`: The results a run is read by, each one a view of a single step port. Naming the boundary here is what lets a step be renamed, split or replaced without anything that consumes the workflow having to change.
- `/outputs/*`: One named result of the workflow: the step port it is taken from, how long it stays fetchable, and optionally what it carries.
- `/outputs/*/from`: The single step port whose envelope becomes this output. One port, because an output that concatenated several would hide which step actually produced what.
- `/outputs/*/from/step`: Step the envelope is taken from.
- `/outputs/*/from/port`: Output port of that step. An error port raised to a workflow output is how a run reports what it could not process without failing.
- `/outputs/*/retain`: How long this output stays fetchable, and how many times it may be collected.
- `/outputs/*/schema`: What the items of this output carry, written as a JSON Schema 2020-12 document. It is what a tool published from this output advertises as its outputSchema, so a client is told the shape of a result and not only its text. It is the mirror of a tool input, which has to have a schema because a tool has to advertise an inputSchema; here it stays optional, and an output declaring none is published as a tool with no outputSchema beside it.
- `/outputs/*/retain`: How long the artifacts of this output stay fetchable, and how many times they may be fetched. A duration answers how long it may be fetched; the long form also answers whether it may be fetched more than once. Retention is capped by the namespace quota: a workflow may always ask for less, never for more.
- `/outputs/*/retain/for`: How long the artifacts stay fetchable when nobody comes for them. A one-shot output still needs one, because it is what expires an artifact that is never collected.
- `/outputs/*/retain/fetches`: How many completed fetches the artifacts survive. The count is per artifact and not per principal, which is why it is a number and not a flag: two consumers of one output are fetches: 2. An interrupted, refused or ranged transfer does not count.
- `/defaults/retain`: How long artifacts stay fetchable. Only a workflow output may be one-shot, so no fetch count belongs here: an artifact travelling between two steps is read once per shard and again by a replay, and consuming it would make a graph that runs correctly the first time and not the second.
- `/defaults/retain/for`: How long the artifacts stay fetchable.

## Schema

`workflow.schema.json#/properties/outputs`:

```json
{
  "type": "object",
  "propertyNames": {"$ref": "#/$defs/portName"},
  "additionalProperties": {"$ref": "#/$defs/workflowOutput"}
}
```

`workflow.schema.json#/$defs/workflowOutput`:

```json
{
  "type": "object",
  "required": ["from"],
  "additionalProperties": false,
  "properties": {
    "from": {
      "type": "object",
      "required": ["step", "port"],
      "additionalProperties": false,
      "properties": {"step": {"$ref": "#/$defs/identifier"}, "port": {"$ref": "#/$defs/portName"}}
    },
    "retain": {"$ref": "#/$defs/retainOutput"},
    "schema": {"$ref": "#/$defs/jsonSchema"}
  }
}
```

`workflow.schema.json#/$defs/retainOutput`:

```json
{
  "oneOf": [
    {"$ref": "#/$defs/duration"},
    {
      "type": "object",
      "required": ["for"],
      "additionalProperties": false,
      "properties": {"for": {"$ref": "#/$defs/duration"}, "fetches": {"type": "integer", "minimum": 1}}
    }
  ]
}
```

`workflow.schema.json#/$defs/retain`:

```json
{
  "oneOf": [
    {"$ref": "#/$defs/duration"},
    {
      "type": "object",
      "required": ["for"],
      "additionalProperties": false,
      "properties": {"for": {"$ref": "#/$defs/duration"}}
    }
  ]
}
```

## Examples

`/outputs`:

```yaml
outputs:
  invoices:
    from:
      step: archive
      port: out
    retain: 90d
  errors:
    from:
      step: invoice
      port: error
```

`/outputs/*`:

```yaml
outputs:
  out:
    from:
      step: archive
      port: out
    retain: 90d
```

`/outputs/*`:

```yaml
outputs:
  out:
    from:
      step: render
      port: out
    retain:
      for: 24h
      fetches: 1
```

`/outputs/*/from`:

```yaml
outputs:
  out:
    from:
      step: archive
      port: out
```

`/outputs/*/from/step`:

```yaml
outputs:
  out:
    from:
      step: archive
```

`/outputs/*/from/step`:

```yaml
outputs:
  out:
    from:
      step: invoice
```

`/outputs/*/from/port`:

```yaml
outputs:
  out:
    from:
      port: out
```

`/outputs/*/from/port`:

```yaml
outputs:
  out:
    from:
      port: error
```

`/outputs/*/retain`:

```yaml
outputs:
  out:
    retain: 90d
```

`/outputs/*/retain`:

```yaml
outputs:
  out:
    retain:
      for: 24h
      fetches: 1
```

`/outputs/*/schema`:

```yaml
outputs:
  out:
    schema:
      $ref: ./schemas/invoice.json
```

`/outputs/*/schema`:

```yaml
outputs:
  out:
    schema:
      type: object
      properties:
        total:
          type: number
```

`/outputs/*/retain/for`:

```yaml
outputs:
  out:
    retain:
      for: 24h
```

`/outputs/*/retain/for`:

```yaml
outputs:
  out:
    retain:
      for: 7d
```

`/outputs/*/retain/fetches`:

```yaml
outputs:
  out:
    retain:
      fetches: 1
```

`/outputs/*/retain/fetches`:

```yaml
outputs:
  out:
    retain:
      fetches: 2
```

`/defaults/retain`:

```yaml
defaults:
  retain: 7d
```

`/defaults/retain`:

```yaml
defaults:
  retain:
    for: 7d
```
