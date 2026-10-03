# `merge`

How several edges arriving on the same input port are combined. wait_all waits for every upstream port and concatenates items in edge declaration order, which is the default when a port has more than one edge. zip pairs items by rank and fails validation on envelopes of differing lengths. join matches items on a JSON path and sends what it cannot match to the unmatched port, when the step declares one. first lifts the barrier as soon as one upstream port has produced, abandoning the other edges and cancelling their steps when nothing else needs them.

## Keywords

- `/steps/*/merge`, `/.*/merge`: How several edges arriving on the same input port are combined. wait_all waits for every upstream port and concatenates items in edge declaration order, which is the default when a port has more than one edge. zip pairs items by rank and fails validation on envelopes of differing lengths. join matches items on a JSON path and sends what it cannot match to the unmatched port, when the step declares one. first lifts the barrier as soon as one upstream port has produced, abandoning the other edges and cancelling their steps when nothing else needs them.
- `/steps/*/merge/join`, `/.*/merge/join`: The join and what it matches items on.
- `/steps/*/merge/join/on`, `/.*/merge/join/on`: JSON path to the value items are matched by. Items that match nothing leave on the unmatched port if the step declares one, rather than disappearing quietly.

## Schema

`workflow.schema.json#/$defs/step/properties/merge`:

```json
{
  "oneOf": [
    {"enum": ["wait_all", "zip", "first"]},
    {
      "type": "object",
      "required": ["join"],
      "additionalProperties": false,
      "properties": {
        "join": {
          "type": "object",
          "required": ["on"],
          "additionalProperties": false,
          "properties": {"on": {"type": "string", "minLength": 1}}
        }
      }
    }
  ],
  "default": "wait_all"
}
```

## Examples

`/steps/*/merge`:

```yaml
steps:
  normalize:
    merge: wait_all
```

`/steps/*/merge`:

```yaml
steps:
  normalize:
    merge: first
```

`/steps/*/merge`:

```yaml
steps:
  normalize:
    merge:
      join:
        on: $.data.customer_id
```
