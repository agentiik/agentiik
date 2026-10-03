# `script`

Shell commands run inside image instead of treating it as a brick, under the same contract every brick honours: the same mounts, the same environment, the same isolation, the same meaning for the exit code. No manifest is read, so nothing about the ports is inferred and outputs has to be declared. A script that writes nothing and exits 0 publishes one item on out carrying its standard output and whatever it left in /agk/out/files/, which is what makes a two-line script worth writing.

## Keywords

- `/steps/*/script`, `/.*/script`: Shell commands run inside image instead of treating it as a brick, under the same contract every brick honours: the same mounts, the same environment, the same isolation, the same meaning for the exit code. No manifest is read, so nothing about the ports is inferred and outputs has to be declared. A script that writes nothing and exits 0 publishes one item on out carrying its standard output and whatever it left in /agk/out/files/, which is what makes a two-line script worth writing.
- `/steps/*/before_script`, `/.*/before_script`: What this step runs before its script, usually the one tool the image does not carry.
- `/steps/*/after_script`, `/.*/after_script`: What this step runs after its script, whether the script worked or not.
- `/steps/*/shell`, `/.*/shell`: The interpreter this step's commands are handed to, and the flags it starts with.
- `/defaults/before_script`, `/defaults/after_script`, `/steps/*/script`, `/steps/*/before_script`, `/steps/*/after_script`, `/.*/script`, `/.*/before_script`, `/.*/after_script`: A list of shell commands, run in order inside the container. The first one to exit non-zero ends the step with that code, which the exit code table then reads as an application, transient or infrastructure failure.
- `/defaults/before_script`, `/steps/*/before_script`, `/.*/before_script`: Commands prepended to script, merged from defaults and from extends outwards in. They run in the same container, which is where a step installs the one tool it needs before doing its work.
- `/defaults/after_script`, `/steps/*/after_script`, `/.*/after_script`: Commands appended to script, run in the same container even when script failed, so that a diagnostic dump survives a failure. Their own exit code does not change the step's verdict.
- `/defaults/shell`, `/steps/*/shell`, `/.*/shell`: The interpreter the commands are handed to, and the flags it starts with. It defaults to ["/bin/sh", "-e"], so the first command that fails ends the step rather than letting the rest run on a broken assumption.

## Schema

`workflow.schema.json#/$defs/step/properties/script`:

```json
{"$ref": "#/$defs/commands"}
```

`workflow.schema.json#/$defs/step/properties/before_script`:

```json
{"$ref": "#/$defs/beforeScript"}
```

`workflow.schema.json#/$defs/step/properties/after_script`:

```json
{"$ref": "#/$defs/afterScript"}
```

`workflow.schema.json#/$defs/step/properties/shell`:

```json
{"$ref": "#/$defs/shell"}
```

`workflow.schema.json#/$defs/commands`:

```json
{"type": "array", "minItems": 1, "items": {"type": "string", "minLength": 1}}
```

`workflow.schema.json#/$defs/beforeScript`:

```json
{"$ref": "#/$defs/commands"}
```

`workflow.schema.json#/$defs/afterScript`:

```json
{"$ref": "#/$defs/commands"}
```

`workflow.schema.json#/$defs/shell`:

```json
{
  "type": "array",
  "minItems": 1,
  "items": {"type": "string", "minLength": 1},
  "default": ["/bin/sh", "-e"]
}
```

## Examples

`/steps/*/script`:

```yaml
steps:
  normalize:
    script:
    - python /agk/repo/scripts/normalize.py
```

`/steps/*/script`:

```yaml
steps:
  normalize:
    script:
    - agk items | jq -r '.data.vat_number' > /tmp/numbers
```

`/steps/*/before_script`:

```yaml
steps:
  normalize:
    before_script:
    - apk add --no-cache jq curl
```

`/steps/*/after_script`:

```yaml
steps:
  normalize:
    after_script:
    - cp /tmp/result.json /agk/out/files/ 2>/dev/null || true
```

`/steps/*/shell`:

```yaml
steps:
  normalize:
    shell:
    - /bin/sh
    - -eu
    - -o
    - pipefail
```

`workflow.schema.json#/$defs/commands`:

```yaml
- psql -f /agk/repo/sql/orders.sql
```

`workflow.schema.json#/$defs/beforeScript`:

```yaml
- apk add --no-cache jq curl
```

`workflow.schema.json#/$defs/afterScript`:

```yaml
- cp /tmp/result.json /agk/out/files/ 2>/dev/null || true
```

`workflow.schema.json#/$defs/shell`:

```yaml
- /bin/sh
- -e
```

`workflow.schema.json#/$defs/shell`:

```yaml
- /bin/sh
- -eu
- -o
- pipefail
```
