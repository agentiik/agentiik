# `secrets`

The secrets of the owning namespace this workflow uses, by name and by nothing else, resolved by the API at the last moment. Where a value lives, its provider and its path, is declared on the namespace through the API or agentiik_secret in Terraform, by a principal holding secret:write, and a namespace is confined to its own paths: a path written in this file would let anyone able to push the workflow aim it at whatever the store holds. Only secrets the owning namespace declares can be named, so moving a workflow elsewhere breaks the reference at validation instead of quietly carrying access along. A step mounts one of them by the same name.

## Keywords

- `/secrets`: The secrets of the owning namespace this workflow uses, by name and by nothing else, resolved by the API at the last moment. Where a value lives, its provider and its path, is declared on the namespace through the API or agentiik_secret in Terraform, by a principal holding secret:write, and a namespace is confined to its own paths: a path written in this file would let anyone able to push the workflow aim it at whatever the store holds. Only secrets the owning namespace declares can be named, so moving a workflow elsewhere breaks the reference at validation instead of quietly carrying access along. A step mounts one of them by the same name.
- `/secrets`: The secrets block, as the entry point and an included file both write it: names the owning namespace declares, each once, and nothing about where a value lives.
- `/steps/*/secrets`, `/.*/secrets`: Which of the workflow's secrets this step is given, kept to the few it actually uses.
- `/defaults/secrets`, `/steps/*/secrets`, `/.*/secrets`: Which of the secrets the workflow names in its secrets block this step gets, by the same name. They arrive as files on tmpfs under /agk/secrets/, at the mount the brick manifest gives the name or at /agk/secrets/<name> where it gives none, rather than as environment variables, because a process environment is readable by its children and ends up in diagnostic dumps. A secret is never reachable from a condition.

## Schema

`workflow.schema.json#/properties/secrets`:

```json
{"$ref": "#/$defs/secrets"}
```

`workflow.schema.json#/$defs/secrets`:

```json
{"type": "array", "minItems": 1, "uniqueItems": true, "items": {"$ref": "#/$defs/identifier"}}
```

`workflow.schema.json#/$defs/step/properties/secrets`:

```json
{"$ref": "#/$defs/stepSecrets"}
```

`workflow.schema.json#/$defs/stepSecrets`:

```json
{"type": "array", "minItems": 1, "uniqueItems": true, "items": {"$ref": "#/$defs/identifier"}}
```

## Examples

`/secrets`:

```yaml
secrets:
- billing
```

`/secrets`:

```yaml
secrets:
- billing
- ledger
```

`/steps/*/secrets`:

```yaml
steps:
  normalize:
    secrets:
    - billing
```

`workflow.schema.json#/$defs/stepSecrets`:

```yaml
- billing
```
