# `retry`

When an attempt is made again, and how long the engine waits first. Retrying is deliberate rather than automatic: an application failure is not retried unless this says it should be, and invalid input, exit code 120, is never retried whatever this says.

## Keywords

- `/defaults/retry`, `/steps/*/retry`, `/.*/retry`: When an attempt is made again, and how long the engine waits first. Retrying is deliberate rather than automatic: an application failure is not retried unless this says it should be, and invalid input, exit code 120, is never retried whatever this says.
- `/steps/*/retry`, `/.*/retry`: Whether a failed attempt at this step is made again, and how long the engine waits first.
- `/steps/*/continue_on_error`, `/.*/continue_on_error`: Whether the run survives this step failing.
- `/steps/*/idempotent`, `/.*/idempotent`: Whether this step can safely be run twice, which is what decides if a lost task is requeued.
- `/defaults/retry/max`, `/steps/*/retry/max`, `/.*/retry/max`: How many further attempts a failure that this policy accepts is given. Beyond it the step is failed and the graph carries on as when says.
- `/defaults/retry/on`, `/steps/*/retry/on`, `/.*/retry/on`: The failures worth another attempt. transient is the 100 to 119 exit range a brick uses to say it was unlucky. failed is the application range, 1 to 99, retried only where the author knows it is safe. lost is a task whose runner stopped reporting, which is requeued only for an idempotent step. timeout is an attempt stopped at its deadline.
- `/defaults/retry/backoff`, `/steps/*/retry/backoff`, `/.*/retry/backoff`: How long to wait between attempts, so that a dependency which is briefly unwell is not hammered while it recovers.
- `/defaults/retry/backoff/type`, `/steps/*/retry/backoff/type`, `/.*/retry/backoff/type`: How the wait grows. exponential lengthens it after each failure, starting at base and never going past max.
- `/defaults/retry/backoff/base`, `/steps/*/retry/backoff/base`, `/.*/retry/backoff/base`: The wait before the first further attempt, and the value the growth starts from.
- `/defaults/retry/backoff/max`, `/steps/*/retry/backoff/max`, `/.*/retry/backoff/max`: Ceiling on the wait between two attempts, however many have already failed.
- `/defaults/continue_on_error`, `/steps/*/continue_on_error`, `/.*/continue_on_error`: Lets the run carry on when this step fails. The step is still failed and still says so: downstream steps see that state through when, which is how an error path is written without the whole run being marked failed.
- `/defaults/idempotent`, `/steps/*/idempotent`, `/.*/idempotent`: Whether running the step again with the same inputs is safe. Only an idempotent step is requeued after a task is lost, because a lost task may well have finished without the result coming back, and only an idempotent step can be cached. The default is true, which puts the burden on the brick author.

## Schema

`workflow.schema.json#/$defs/step/properties/retry`:

```json
{"$ref": "#/$defs/retry"}
```

`workflow.schema.json#/$defs/retry`:

```json
{
  "type": "object",
  "minProperties": 1,
  "additionalProperties": false,
  "properties": {
    "max": {"type": "integer", "minimum": 0},
    "on": {
      "type": "array",
      "minItems": 1,
      "uniqueItems": true,
      "items": {"enum": ["transient", "failed", "lost", "timeout"]}
    },
    "backoff": {
      "type": "object",
      "minProperties": 1,
      "additionalProperties": false,
      "properties": {
        "type": {"enum": ["exponential"]},
        "base": {"$ref": "#/$defs/duration"},
        "max": {"$ref": "#/$defs/duration"}
      }
    }
  }
}
```

`workflow.schema.json#/$defs/step/properties/continue_on_error`:

```json
{"$ref": "#/$defs/continueOnError"}
```

`workflow.schema.json#/$defs/continueOnError`:

```json
{"type": "boolean"}
```

`workflow.schema.json#/$defs/step/properties/idempotent`:

```json
{"$ref": "#/$defs/idempotent"}
```

`workflow.schema.json#/$defs/idempotent`:

```json
{"type": "boolean", "default": true}
```

## Examples

`workflow.schema.json#/$defs/retry`:

```yaml
max: 4
on:
- transient
```

`workflow.schema.json#/$defs/retry`:

```yaml
max: 2
on:
- transient
backoff:
  type: exponential
  base: 2s
  max: 60s
```

`/steps/*/retry`:

```yaml
steps:
  normalize:
    retry:
      max: 4
      on:
      - transient
```

`/steps/*/continue_on_error`:

```yaml
steps:
  normalize:
    continue_on_error: true
```

`/steps/*/idempotent`:

```yaml
steps:
  normalize:
    idempotent: false
```

`/defaults/retry/max`:

```yaml
defaults:
  retry:
    max: 2
```

`/defaults/retry/max`:

```yaml
defaults:
  retry:
    max: 4
```

`/defaults/retry/on`:

```yaml
defaults:
  retry:
    on:
    - transient
```

`/defaults/retry/on`:

```yaml
defaults:
  retry:
    on:
    - transient
    - lost
```

`/defaults/retry/backoff`:

```yaml
defaults:
  retry:
    backoff:
      type: exponential
      base: 2s
      max: 60s
```

`/defaults/retry/backoff/type`:

```yaml
defaults:
  retry:
    backoff:
      type: exponential
```

`/defaults/retry/backoff/base`:

```yaml
defaults:
  retry:
    backoff:
      base: 2s
```

`/defaults/retry/backoff/max`:

```yaml
defaults:
  retry:
    backoff:
      max: 60s
```

`workflow.schema.json#/$defs/continueOnError`:

```yaml
true
```

`workflow.schema.json#/$defs/continueOnError`:

```yaml
false
```
