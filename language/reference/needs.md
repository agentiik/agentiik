# `needs`

The inbound edges of the step, which are the only form of dependency there is. Each edge names the step and the output port an envelope comes from and the input port it arrives on. A step becomes runnable once every declared port is satisfied, so this is a barrier and not a stream.

## Keywords

- `/steps/*/needs`, `/.*/needs`: The inbound edges of the step, which are the only form of dependency there is. Each edge names the step and the output port an envelope comes from and the input port it arrives on. A step becomes runnable once every declared port is satisfied, so this is a barrier and not a stream.
- `/steps/*/if`, `/.*/if`: Decides whether the step runs at all. When it is false the step is skipped and publishes empty envelopes on every port, so the steps below it still get to decide their own fate through when. Item contents are not readable here: a test reads port metadata such as inputs.in.count, which is what keeps the controller from loading a namespace's data to schedule it.
- `/steps/*/when`, `/.*/when`: Which upstream outcomes let this step start, so that a step cleaning up after a failure runs where an ordinary one would not.
- `/steps/*/needs/*`, `/.*/needs/*`: One inbound edge: the output port of another step, and the input port it lands on here. The short form names the step alone, which is the same as taking its out port onto in.
- `/steps/*/needs/*/step`, `/.*/needs/*/step`: The step the envelope comes from.
- `/steps/*/needs/*/port`, `/.*/needs/*/port`: Which of that step's output ports is taken. This is what lets an item rejected by one step bypass the next and still reach the last.
- `/steps/*/needs/*/as`, `/.*/needs/*/as`: The input port the envelope arrives on here. Naming it is what lets one step take two different envelopes and still tell them apart.
- `/defaults/when`, `/steps/*/when`, `/.*/when`: The upstream states this step is willing to start on. The default is [succeeded], which is why a step that cleans up after a failure, or that runs whatever happened, has to say so. A step reached through continue_on_error sees the failed state here.

## Schema

`workflow.schema.json#/$defs/step/properties/needs`:

```json
{"type": "array", "minItems": 1, "uniqueItems": true, "items": {"$ref": "#/$defs/edge"}}
```

`workflow.schema.json#/$defs/edge`:

```json
{
  "oneOf": [
    {"$ref": "#/$defs/identifier"},
    {
      "type": "object",
      "required": ["step"],
      "additionalProperties": false,
      "properties": {
        "step": {"$ref": "#/$defs/identifier"},
        "port": {"$ref": "#/$defs/portName", "default": "out"},
        "as": {"$ref": "#/$defs/portName", "default": "in"}
      }
    }
  ]
}
```

`workflow.schema.json#/$defs/step/properties/when`:

```json
{"$ref": "#/$defs/when"}
```

`workflow.schema.json#/$defs/when`:

```json
{
  "type": "array",
  "minItems": 1,
  "uniqueItems": true,
  "items": {"enum": ["succeeded", "failed", "skipped", "always"]},
  "default": ["succeeded"]
}
```

`workflow.schema.json#/$defs/step/properties/if`:

```json
{"$ref": "#/$defs/expression"}
```

## Examples

`/steps/*/needs`:

```yaml
steps:
  normalize:
    needs:
    - normalize
```

`/steps/*/needs`:

```yaml
steps:
  normalize:
    needs:
    - step: invoice
      port: out
      as: invoices
    - step: normalize
      port: rejected
      as: rejected
```

`/steps/*/if`:

```yaml
steps:
  normalize:
    if: ${{ inputs.in.count > 0 }}
```

`/steps/*/when`:

```yaml
steps:
  normalize:
    when:
    - succeeded
    - skipped
```

`workflow.schema.json#/$defs/edge`:

```yaml
normalize
```

`workflow.schema.json#/$defs/edge`:

```yaml
step: normalize
port: ok
as: in
```

`/steps/*/needs/*/step`:

```yaml
steps:
  normalize:
    needs:
    - step: normalize
```

`/steps/*/needs/*/port`:

```yaml
steps:
  normalize:
    needs:
    - port: ok
```

`/steps/*/needs/*/port`:

```yaml
steps:
  normalize:
    needs:
    - port: rejected
```

`/steps/*/needs/*/port`:

```yaml
steps:
  normalize:
    needs:
    - port: error
```

`/steps/*/needs/*/as`:

```yaml
steps:
  normalize:
    needs:
    - as: in
```

`/steps/*/needs/*/as`:

```yaml
steps:
  normalize:
    needs:
    - as: invoices
```

`/steps/*/needs/*/as`:

```yaml
steps:
  normalize:
    needs:
    - as: rejected
```

`workflow.schema.json#/$defs/when`:

```yaml
- succeeded
- skipped
```

`workflow.schema.json#/$defs/when`:

```yaml
- always
```
