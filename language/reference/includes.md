# `includes`

Files and repositories merged into this one before anything else is resolved, in declaration order, each read as an included file, $defs/fragment. Reuse applies to steps and defaults; the published surface is not included, so reading this file tells you everything the workflow exposes.

## Keywords

- `/include`: Files and repositories merged into this one before anything else is resolved, in declaration order, each read as an included file, $defs/fragment. Reuse applies to steps and defaults; the published surface is not included, so reading this file tells you everything the workflow exposes.
- `/defaults`: Settings every step of this workflow inherits unless it states its own, so that a decision taken once is not repeated in twenty places.
- `/.*`: A hidden block at the root of the file, whose name starts with a dot. It is never executed and exists to be named by a step's extends, which is how one set of settings is written once and inherited by many steps.
- ``: A file an include names: a fragment merged into the file that includes it, never an entry point, and validated against this definition rather than against the root of this document, which is the entry point's. It carries what reuse is for, hidden blocks at its root, steps, defaults, vars, secrets and includes of its own, and none of what makes a document an entry point: no apiVersion, kind or metadata, and none of the workflow's boundary, inputs, outputs, on, mcp, concurrency or timeout, since what a workflow is given, returns, starts on, publishes and is bounded by is declared by the workflow itself, so that reading one file tells you what that workflow is. It is closed like every block the language owns, so any of those keys, and any key it does not define, is refused rather than ignored. What it writes sits under what the file including it writes, keyword by keyword, and what it includes sits under it in turn. A repository whose root agentiik.yaml is written this way is a library: another workflow includes it at a tag or a commit, its own hook validates it as a fragment, and nothing runs it.
- `/include`: Files and repositories this file includes in turn, resolved before it in declaration order, so that what they carry sits under what this file writes. A path include here resolves against this file's own directory, and inside the repository this file came from where a workflow include brought it.
- `/vars`: Variables this file contributes, read through the vars context of the workflow including it. A name the including file writes too takes that file's value, since the including file is applied last.
- `/secrets`: Secrets the steps and blocks of this file name. Names add up rather than override: a name written here and by the including file is one secret, and it is the namespace owning the workflow that runs it that has to declare it, whichever repository the name was written in.
- `/defaults`: Settings this file contributes to the defaults of the workflow including it, keyword by keyword, under whatever the including file sets itself.
- `/steps`: Steps and hidden blocks this file contributes. A step written here carries its own values, above the defaults, and a step of the same name in the including file overrides it keyword by keyword; a hidden block of the same name is replaced whole.
- `/.*`: A hidden block at the root of the included file, never executed, there to be named by the extends of a step in this file or in any file including it.
- `/include`: The include block, as the entry point and an included file both write it: files and repositories in declaration order, each read as an included file, $defs/fragment, and merged under the file naming it.
- `/include/*`: One file or repository merged into this one, and read as an included file, $defs/fragment, never as an entry point. A path include resolves inside the same commit, so it can never be stale and nothing has to pin it. A workflow include resolves another repository at a tag or a commit, requires workflow:read on it, and records the commit it resolved to in the run.
- `/include/*/path`: A file of this repository, relative to the directory of the file naming it, which for the entry point is the root, or from the root where it starts with /. A path leaving the repository tree is refused. It travels with the commit, which is why it carries no version of its own and why a ref beside it is refused: a path include resolves inside the same commit, so it can never be stale and there is nothing for a ref to pin.
- `/include/*/workflow`: Another workflow repository, as <namespace>/<name>, whose root agentiik.yaml is read at ref. That file is written as an included file, $defs/fragment: a library repository, which its own hook validates as a fragment and nothing runs, since an entry point carries apiVersion and the rest of what a fragment refuses. Its own path includes resolve inside that repository, at that commit. Reading it requires workflow:read on it, so an include cannot widen what its author may see.
- `/include/*/ref`: Tag or commit the included repository is read at: a tag, by its name or in full, or a commit written whole that a branch or a tag of the library was pushed to, which is a version its hook judged. It is required because a moving ref would let another repository change what this workflow does without this commit changing, so a branch is refused. A commit abbreviated is refused too: an abbreviation is a name another commit of the library can come to share, and the file would then name two. The version records the commit it resolved to, and every run of it names that commit.
- `/include/*/workflow`, `/steps/*/workflow/workflow`, `/.*/workflow/workflow`: A workflow addressed as <namespace>/<name>, the same identity a grant and a git remote use. The namespace is never a reserved word, as $defs/namespace lists them, and each half is at most 255 characters, as $defs/identifier is: a maxLength would bound the whole string, so the pattern counts each half.
- `/defaults`: Settings applied to every step that does not state its own. Resolution is ordered: includes first, which bring hidden blocks and defaults of their own, then extends depth first, then these defaults, then the step's own values, so a default never overrides something a step said. A step written in an included file carries its own values, above these defaults, and the entry point overrides it keyword by keyword. Graph shape is not settable here: what a step runs and what it depends on belongs to the step.
- `/defaults/timeout`: How long a shard may run where the step sets no limit of its own.
- `/defaults/retain`: How long intermediate artifacts stay fetchable, and any workflow output that does not say otherwise.
- `/defaults/retry`: The policy a step inherits when it declares no retry of its own.
- `/defaults/resources`: What a container is given where the step asks for nothing in particular.
- `/defaults/network`: The network a step gets unless it asks for another, which is where a workflow says that reaching out is the exception.
- `/defaults/egress`: The addresses every step may reach, for a workflow whose steps all talk to the same few hosts.
- `/defaults/runs_on`: The runner labels every step selects, for a workflow that belongs on one kind of host.
- `/defaults/secrets`: Secrets mounted into every step, for a workflow whose steps all need the same credential.
- `/defaults/cache`: Whether memoisation is on for the steps that do not decide for themselves.
- `/defaults/continue_on_error`: Whether a failure leaves the run going, for the steps that do not say.
- `/defaults/idempotent`: Whether steps may be requeued after a lost task and cached, unless one of them says otherwise.
- `/defaults/files`: What every step receives from the repository tree, for a repository too large to hand over whole.
- `/defaults/shell`: The interpreter every script step is handed to unless it names another.
- `/defaults/before_script`: Commands every script step runs first, merged outwards in with those of the blocks it extends.
- `/defaults/after_script`: Commands every script step runs last, failure included.
- `/defaults/when`: The upstream states steps start on where they do not say, which stays [succeeded] unless a workflow decides otherwise.
- `/steps/*/extends`, `/.*/extends`: The hidden block this step inherits from. A hidden block's name starts with a dot and is never executed, which is how one set of settings is written once and shared by many steps. It is resolved after includes and before defaults, so the step's own values still win.

## Schema

`workflow.schema.json#/properties/include`:

```json
{"$ref": "#/$defs/includes"}
```

`workflow.schema.json#/$defs/includes`:

```json
{"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/include"}}
```

`workflow.schema.json#/$defs/include`:

```json
{
  "type": "object",
  "oneOf": [
    {"required": ["path"], "not": {"anyOf": [{"required": ["workflow"]}, {"required": ["ref"]}]}},
    {"required": ["workflow", "ref"], "not": {"required": ["path"]}}
  ],
  "additionalProperties": false,
  "properties": {
    "path": {"type": "string", "minLength": 1},
    "workflow": {"$ref": "#/$defs/workflowPath"},
    "ref": {"type": "string", "minLength": 1}
  }
}
```

`workflow.schema.json#/$defs/workflowPath`:

```json
{
  "type": "string",
  "pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{0,254}/[A-Za-z0-9][A-Za-z0-9_-]{0,254}$",
  "not": {
    "pattern": "^(auth|me|users|groups|service-accounts|namespaces|runners|runner-pools|bus|tasks|bricks|runs|artifacts|stats)/"
  }
}
```

`workflow.schema.json#/$defs/fragment`:

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "include": {"$ref": "#/$defs/includes"},
    "vars": {"$ref": "#/$defs/vars"},
    "secrets": {"$ref": "#/$defs/secrets"},
    "defaults": {"$ref": "#/$defs/defaults"},
    "steps": {"$ref": "#/$defs/steps"}
  },
  "patternProperties": {"^\\.[A-Za-z0-9][A-Za-z0-9_-]*$": {"$ref": "#/$defs/step"}}
}
```

`workflow.schema.json#/properties/defaults`:

```json
{"$ref": "#/$defs/defaults"}
```

`workflow.schema.json#/$defs/defaults`:

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "timeout": {"$ref": "#/$defs/timeout"},
    "retain": {"$ref": "#/$defs/retain"},
    "retry": {"$ref": "#/$defs/retry"},
    "resources": {"$ref": "#/$defs/resources"},
    "network": {"$ref": "#/$defs/network"},
    "egress": {"$ref": "#/$defs/egress"},
    "runs_on": {"$ref": "#/$defs/runsOn"},
    "secrets": {"$ref": "#/$defs/stepSecrets"},
    "cache": {"$ref": "#/$defs/cache"},
    "continue_on_error": {"$ref": "#/$defs/continueOnError"},
    "idempotent": {"$ref": "#/$defs/idempotent"},
    "files": {"$ref": "#/$defs/files"},
    "shell": {"$ref": "#/$defs/shell"},
    "before_script": {"$ref": "#/$defs/beforeScript"},
    "after_script": {"$ref": "#/$defs/afterScript"},
    "when": {"$ref": "#/$defs/when"}
  }
}
```

`workflow.schema.json#/$defs/step/properties/extends`:

```json
{"type": "string", "pattern": "^\\.[A-Za-z0-9][A-Za-z0-9_-]*$"}
```

`workflow.schema.json#/patternProperties`:

```json
{
  "^\\.[A-Za-z0-9][A-Za-z0-9_-]*$": {
    "description": "A hidden block at the root of the file, whose name starts with a dot. It is never executed and exists to be named by a step's extends, which is how one set of settings is written once and inherited by many steps.",
    "$ref": "#/$defs/step",
    "examples": [{"timeout": "2m", "network": "egress", "retry": {"max": 3, "on": ["transient"]}}]
  }
}
```

## Examples

`/include`:

```yaml
include:
- path: ./common-bricks.yaml
- workflow: finance/common
  ref: v2.1.0
```

`/defaults`:

```yaml
defaults:
  timeout: 10m
  network: none
  retain: 7d
```

`/.*`:

```yaml
.base:
  timeout: 2m
  network: egress
  retry:
    max: 3
    on:
    - transient
```

`workflow.schema.json#/$defs/fragment`:

```yaml
.api-brick:
  timeout: 2m
  retry:
    max: 3
    on:
    - transient
  network: egress
  resources:
    cpu: '0.5'
    memory: 256Mi
.region-matrix:
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

`workflow.schema.json#/$defs/fragment`:

```yaml
include:
- path: ./retry-policy.yaml
vars:
  currency: EUR
secrets:
- billing
defaults:
  timeout: 10m
  network: none
steps:
  .billing-api:
    network: egress
    egress:
      allow:
      - api.billing.example.com:443
    secrets:
    - billing
```

`/include`:

```yaml
include:
- path: ./retry-policy.yaml
```

`/vars`:

```yaml
vars:
  currency: EUR
```

`/secrets`:

```yaml
secrets:
- billing
```

`/defaults`:

```yaml
defaults:
  timeout: 10m
  network: none
```

`/steps`:

```yaml
steps:
  .billing-api:
    network: egress
    egress:
      allow:
      - api.billing.example.com:443
    secrets:
    - billing
```

`/include/*`:

```yaml
include:
- path: ./common-bricks.yaml
```

`/include/*`:

```yaml
include:
- workflow: finance/common
  ref: v2.1.0
```

`/include/*/path`:

```yaml
include:
- path: ./includes/defaults.yaml
```

`/include/*/path`:

```yaml
include:
- path: /includes/defaults.yaml
```

`/include/*/workflow`:

```yaml
include:
- workflow: finance/common
```

`/include/*/ref`:

```yaml
include:
- ref: v2.1.0
```

`/include/*/ref`:

```yaml
include:
- ref: c41d9e2a7b3f5e8d1c0a9b6e4f2d8c7a5b3e1f09
```

`workflow.schema.json#/$defs/workflowPath`:

```yaml
finance/common
```

`workflow.schema.json#/$defs/workflowPath`:

```yaml
finance/monthly-invoicing
```

`/defaults`:

```yaml
defaults:
  timeout: 10m
  retain: 7d
  retry:
    max: 2
    on:
    - transient
    backoff:
      type: exponential
      base: 2s
      max: 60s
  resources:
    cpu: '1'
    memory: 512Mi
  network: none
  runs_on:
  - arch=amd64
```

`/defaults/timeout`:

```yaml
defaults:
  timeout: 10m
```

`/defaults/retain`:

```yaml
defaults:
  retain: 7d
```

`/defaults/retry`:

```yaml
defaults:
  retry:
    max: 2
    on:
    - transient
```

`/defaults/resources`:

```yaml
defaults:
  resources:
    cpu: '1'
    memory: 512Mi
```

`/defaults/network`:

```yaml
defaults:
  network: none
```

`/defaults/egress`:

```yaml
defaults:
  egress:
    allow:
    - api.billing.example.com:443
```

`/defaults/runs_on`:

```yaml
defaults:
  runs_on:
  - arch=amd64
```

`/defaults/secrets`:

```yaml
defaults:
  secrets:
  - billing
```

`/defaults/cache`:

```yaml
defaults:
  cache: true
```

`/defaults/continue_on_error`:

```yaml
defaults:
  continue_on_error: false
```

`/defaults/idempotent`:

```yaml
defaults:
  idempotent: true
```

`/defaults/files`:

```yaml
defaults:
  files:
  - ./sql/**
```

`/defaults/shell`:

```yaml
defaults:
  shell:
  - /bin/sh
  - -e
```

`/defaults/before_script`:

```yaml
defaults:
  before_script:
  - apk add --no-cache jq curl
```

`/defaults/after_script`:

```yaml
defaults:
  after_script:
  - cp /tmp/result.json /agk/out/files/ 2>/dev/null || true
```

`/defaults/when`:

```yaml
defaults:
  when:
  - succeeded
```

`/steps/*/extends`:

```yaml
steps:
  normalize:
    extends: .api-brick
```

`/steps/*/extends`:

```yaml
steps:
  normalize:
    extends: .region-matrix
```
