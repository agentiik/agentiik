# `fan_out`

How the step is split into shards and how many of them run at once. Shards of one step have their envelopes concatenated port by port before publication, so a fan-out is invisible to whatever reads the result.

## Keywords

- `/steps/*/strategy`, `/.*/strategy`: How the step is split into shards and how many of them run at once. Shards of one step have their envelopes concatenated port by port before publication, so a fan-out is invisible to whatever reads the result.
- `/steps/*/strategy/fan_out`, `/.*/strategy/fan_out`: Whether one container takes the whole envelope or the work is divided. none gives a single container everything and is the default. item gives one container per item, each receiving a single-item envelope, and is the only place an expression can read item. batch(n) gives one container per n items. matrix gives one container per combination of the matrix variables.
- `/steps/*/strategy/max_parallel`, `/.*/strategy/max_parallel`: Ceiling on how many shards of this step run at the same time. It is what keeps a fan-out of ten thousand items from taking the whole namespace's share of the fleet, before the namespace quota has to.
- `/steps/*/strategy/matrix`, `/.*/strategy/matrix`: Variable lists whose cartesian product becomes the shards. Every combination runs as its own shard with its variables injected into params, which is how one step covers three regions and two profiles without being written six times.
- `/steps/*/strategy/fail_fast`, `/.*/strategy/fail_fast`: Whether the first shard to fail stops the shards still running, rather than letting every shard finish before the step is judged.

## Schema

`workflow.schema.json#/$defs/step/properties/strategy`:

```json
{
  "type": "object",
  "minProperties": 1,
  "additionalProperties": false,
  "properties": {
    "fan_out": {
      "anyOf": [
        {"enum": ["none", "item", "matrix"]},
        {"type": "string", "pattern": "^batch\\([1-9][0-9]*\\)$"}
      ],
      "default": "none"
    },
    "max_parallel": {"type": "integer", "minimum": 1},
    "matrix": {
      "type": "object",
      "minProperties": 1,
      "propertyNames": {"$ref": "#/$defs/identifier"},
      "additionalProperties": {"type": "array", "minItems": 1}
    },
    "fail_fast": {"type": "boolean"}
  }
}
```

## Examples

`/steps/*/strategy`:

```yaml
steps:
  normalize:
    strategy:
      fan_out: item
      max_parallel: 8
```

`/steps/*/strategy`:

```yaml
steps:
  normalize:
    strategy:
      matrix:
        region:
        - fr
        - be
        - ch
        profile:
        - standard
        - reduced
      max_parallel: 3
```

`/steps/*/strategy/fan_out`:

```yaml
steps:
  normalize:
    strategy:
      fan_out: none
```

`/steps/*/strategy/fan_out`:

```yaml
steps:
  normalize:
    strategy:
      fan_out: item
```

`/steps/*/strategy/fan_out`:

```yaml
steps:
  normalize:
    strategy:
      fan_out: batch(50)
```

`/steps/*/strategy/fan_out`:

```yaml
steps:
  normalize:
    strategy:
      fan_out: matrix
```

`/steps/*/strategy/max_parallel`:

```yaml
steps:
  normalize:
    strategy:
      max_parallel: 3
```

`/steps/*/strategy/max_parallel`:

```yaml
steps:
  normalize:
    strategy:
      max_parallel: 8
```

`/steps/*/strategy/matrix`:

```yaml
steps:
  normalize:
    strategy:
      matrix:
        region:
        - fr
        - be
        - ch
        profile:
        - standard
        - reduced
```

`/steps/*/strategy/fail_fast`:

```yaml
steps:
  normalize:
    strategy:
      fail_fast: true
```

`/steps/*/strategy/fail_fast`:

```yaml
steps:
  normalize:
    strategy:
      fail_fast: false
```
