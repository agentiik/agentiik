# `expressions`

A CEL expression inside ${{ }}, evaluated by the controller. An expression that fills the whole value keeps its type; one embedded in a longer string is converted to text. CEL is used because it terminates, has no side effects and cannot be made to diverge by the file that carries it. It reads a fixed set of roots, each only where it means something: workflow (name, namespace, version, inputs), run (id, started_at, attempt, trigger_kind, triggered_by) and vars everywhere; trigger (body, headers, query, scheduled_for) in the on block and step parameters; event (the CloudEvents 1.0 attributes and data) in an event trigger's filter and map; inputs (the current step's input ports: count, empty, bytes) and steps (steps.<id>.status, steps.<id>.outputs.<port>.count) in a step; item under fan_out: item and matrix under fan_out: matrix, in a shard; and secrets, opaque references, in params and secrets alone. It never reads what an item holds, only a port's metadata such as inputs.in.count, except the current item under fan_out: item, so the controller serving every namespace never loads one namespace's business data.

## Keywords

- `/on/event/*/filter`, `/steps/*/if`, `/.*/if`: A CEL expression inside ${{ }}, evaluated by the controller. An expression that fills the whole value keeps its type; one embedded in a longer string is converted to text. CEL is used because it terminates, has no side effects and cannot be made to diverge by the file that carries it. It reads a fixed set of roots, each only where it means something: workflow (name, namespace, version, inputs), run (id, started_at, attempt, trigger_kind, triggered_by) and vars everywhere; trigger (body, headers, query, scheduled_for) in the on block and step parameters; event (the CloudEvents 1.0 attributes and data) in an event trigger's filter and map; inputs (the current step's input ports: count, empty, bytes) and steps (steps.<id>.status, steps.<id>.outputs.<port>.count) in a step; item under fan_out: item and matrix under fan_out: matrix, in a shard; and secrets, opaque references, in params and secrets alone. It never reads what an item holds, only a port's metadata such as inputs.in.count, except the current item under fan_out: item, so the controller serving every namespace never loads one namespace's business data.

## Schema

`workflow.schema.json#/$defs/expression`:

```json
{"type": "string"}
```

## Examples

`workflow.schema.json#/$defs/expression`:

```yaml
${{ workflow.inputs.orders }}
```

`workflow.schema.json#/$defs/expression`:

```yaml
${{ inputs.in.count > 0 }}
```

`workflow.schema.json#/$defs/expression`:

```yaml
${{ vars.currency }}
```

`workflow.schema.json#/$defs/expression`:

```yaml
${{ trigger.body.orders }}
```
