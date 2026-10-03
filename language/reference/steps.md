# `steps`

The steps of the workflow, named. The order they appear in has no bearing on scheduling: a step becomes runnable once every declared input port is satisfied, and an edge is the only form of dependency there is. A name starting with a dot is a hidden block, never executed, there to be extended.

## Keywords

- `/steps`: The steps of the workflow, named. The order they appear in has no bearing on scheduling: a step becomes runnable once every declared input port is satisfied, and an edge is the only form of dependency there is. A name starting with a dot is a hidden block, never executed, there to be extended.
- `workflow.schema.json#/$defs/paramName`: Name of a step parameter. It is a key of /agk/params.json, and the same name is exported as AGK_PARAM_<NAME> where a step runs a script instead of a brick, so a parameter name reads as an identifier and carries no hyphen. It is the grammar the brick manifest keys its own params by, which is what lets a step be validated against a manifest at all.
- `/steps`: The steps block, as the entry point and an included file both write it: steps by name, with hidden blocks, whose names start with a dot, among them.
- `/steps/*`, `/.*`: One instance of a brick, a script or a sub-workflow inside the graph, with its edges, its parameters and its execution settings. A hidden block, whose name starts with a dot, has the same shape and is never executed.
- `/steps/*/image`, `/.*/image`: OCI image the step runs. A digest is required in production and a mutable tag is accepted in development, where it is resolved once for the whole run so that two shards of one step can never execute different code.
- `/steps/*/params`, `/.*/params`: Parameters handed to the brick, validated against its manifest schema once the expressions are resolved. A step carrying a script has no manifest to validate against, so each entry is written to /agk/params.json and exported as AGK_PARAM_<NAME>. Substitution belongs here rather than in the repository tree, because a file in the tree is bytes and never a template.
- `/steps/*/timeout`, `/.*/timeout`: How long one shard of this step may run before its container is stopped.
- `/steps/*/resources`, `/.*/resources`: What this step's container is given, for work that needs more or less than the rest of the graph.
- `/steps/*/network`, `/.*/network`: Whether this step can reach anything at all, and through what.
- `/steps/*/egress`, `/.*/egress`: The addresses this step is allowed to open a connection to.
- `/steps/*/runs_on`, `/.*/runs_on`: Where this step has to run, for work that any runner will not do.
- `/steps/*/cache`, `/.*/cache`: Whether an identical earlier execution of this step may stand in for running it again.
- `/steps/*/workflow`, `/.*/workflow`: Calls a sub-workflow instead of running an image. It is the only way to loop, since cycles in the graph are refused at validation, and it is bounded by a configurable maximum depth. A call into another namespace requires workflow:run on the workflow being called.
- `/steps/*/workflow/workflow`, `/.*/workflow/workflow`: The workflow to call, as <namespace>/<name>.
- `/steps/*/workflow/ref`, `/.*/workflow/ref`: Tag or commit the called workflow is read at, so that a change on its default branch does not change this run.
- `/defaults/timeout`, `/steps/*/timeout`, `/.*/timeout`: Maximum duration of one shard, not of the step and not of the run: the whole run is bounded by the timeout at the root. On expiry the container is sent SIGTERM and then SIGKILL after a grace period the runner policy sets, so a step with something to flush or to write gets the chance to do it.
- `/defaults/resources`, `/steps/*/resources`, `/.*/resources`: What the container is given and may not exceed. These are capped by the runner policy and by the namespace quota, so they can always ask for less than the fleet allows and never for more.
- `/defaults/resources/cpu`, `/steps/*/resources/cpu`, `/.*/resources/cpu`: CPU allowance, in cores, fractions included. It is written as a string so that half a core reads as 0.5 wherever it travels, in this file, in the brick manifest and in the task message alike, and so that cpu: 0.5 and cpu: "0.5" are not two different documents in YAML. It is what the runner sets NanoCPUs from, and a runner refuses a task whose demand would take it past its own declared capacity. Zero is not an allowance, and NanoCPUs 0 is how a container is given no limit at all, so the value starts above it.
- `/defaults/resources/memory`, `/steps/*/resources/memory`, `/.*/resources/memory`: Memory ceiling for the container, as a quantity with a binary suffix. The container is stopped when it goes past it, which is a failure of the step and not of the runner. A ceiling of zero would stop the container before it started, so the quantity begins at one.
- `/defaults/resources/pids`, `/steps/*/resources/pids`, `/.*/resources/pids`: Ceiling on the number of processes inside the container, 256 by default. It is the cheap defence against a fork bomb taking a runner with it.
- `/defaults/network`, `/steps/*/network`, `/.*/network`: The network the container is given. none is the default and leaves it with no network at all. egress attaches a network whose outbound traffic goes through the runner proxy that enforces the egress list. internal attaches a network with no route out and no address of the runner host in it, for a step that only talks to a sidecar service. Every task gets its own network, so two containers on one host never see each other.
- `/defaults/egress`, `/steps/*/egress`, `/.*/egress`: The addresses this step may reach. The runner proxy enforces it, so anything not listed is refused rather than merely discouraged. It only means something with network: egress, where it is the difference between one API and the whole internet.
- `/defaults/egress/allow`, `/steps/*/egress/allow`, `/.*/egress/allow`: The host and port pairs the step is allowed to open a connection to.
- `/defaults/runs_on`, `/steps/*/runs_on`, `/.*/runs_on`: Runner labels a task of this step has to match, for work that has to happen somewhere particular: a zone, an architecture, a host holding a licence. A namespace can only select the runner pools it is allowed to use, so this narrows a choice and never widens it. The task goes to the one pool whose labels include every one of these, because a pool is a queue and a task on two queues would run twice: none, or more than one, fails the step with exit code 125 on the infrastructure's account. A step that names none goes to the pool default.
- `/defaults/cache`, `/steps/*/cache`, `/.*/cache`: Reuses the result of an identical earlier execution instead of starting a container. The key combines the image digest, the resolved parameters and the digests of the input envelopes, prefixed by the namespace, so a hit never crosses a namespace boundary. A hit that would hand back an expired artifact is not a hit, because the point is to skip work whose result is still there.

## Schema

`workflow.schema.json#/properties/steps`:

```json
{"$ref": "#/$defs/steps"}
```

`workflow.schema.json#/$defs/steps`:

```json
{
  "type": "object",
  "propertyNames": {"anyOf": [{"$ref": "#/$defs/identifier"}, {"pattern": "^\\.[A-Za-z0-9][A-Za-z0-9_-]*$"}]},
  "additionalProperties": {"$ref": "#/$defs/step"}
}
```

`workflow.schema.json#/$defs/step`:

```json
{
  "type": "object",
  "additionalProperties": false,
  "allOf": [
    {"not": {"required": ["image", "workflow"]}},
    {
      "if": {"required": ["workflow"]},
      "then": {
        "not": {
          "anyOf": [
            {"required": ["script"]},
            {"required": ["before_script"]},
            {"required": ["after_script"]},
            {"required": ["shell"]}
          ]
        }
      }
    }
  ],
  "properties": {
    "image": {"type": "string", "minLength": 1},
    "needs": {"$comment": "taught by the needs topic"},
    "inputs": {"$comment": "taught by the ports topic"},
    "outputs": {"$comment": "taught by the ports topic"},
    "params": {"type": "object", "propertyNames": {"$ref": "#/$defs/paramName"}, "additionalProperties": {}},
    "if": {"$comment": "taught by the needs topic"},
    "when": {"$comment": "taught by the needs topic"},
    "merge": {"$comment": "taught by the merge topic"},
    "strategy": {"$comment": "taught by the fan_out topic"},
    "retry": {"$comment": "taught by the retry topic"},
    "timeout": {"$ref": "#/$defs/timeout"},
    "continue_on_error": {"$comment": "taught by the retry topic"},
    "resources": {"$ref": "#/$defs/resources"},
    "network": {"$ref": "#/$defs/network"},
    "egress": {"$ref": "#/$defs/egress"},
    "runs_on": {"$ref": "#/$defs/runsOn"},
    "secrets": {"$comment": "taught by the secrets topic"},
    "cache": {"$ref": "#/$defs/cache"},
    "idempotent": {"$comment": "taught by the retry topic"},
    "files": {"$comment": "taught by the files topic"},
    "script": {"$comment": "taught by the script topic"},
    "before_script": {"$comment": "taught by the script topic"},
    "after_script": {"$comment": "taught by the script topic"},
    "shell": {"$comment": "taught by the script topic"},
    "extends": {"$comment": "taught by the includes topic"},
    "workflow": {
      "oneOf": [
        {
          "type": "string",
          "pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{0,254}/[A-Za-z0-9][A-Za-z0-9_-]{0,254}(@\\S+)?$",
          "not": {
            "pattern": "^(auth|me|users|groups|service-accounts|namespaces|runners|runner-pools|bus|tasks|bricks|runs|artifacts|stats)/"
          }
        },
        {
          "type": "object",
          "required": ["workflow"],
          "additionalProperties": false,
          "properties": {"workflow": {"$ref": "#/$defs/workflowPath"}, "ref": {"type": "string", "minLength": 1}}
        }
      ]
    }
  }
}
```

`workflow.schema.json#/$defs/paramName`:

```json
{"type": "string", "pattern": "^[A-Za-z_][A-Za-z0-9_]*$"}
```

`workflow.schema.json#/$defs/timeout`:

```json
{"$ref": "#/$defs/duration"}
```

`workflow.schema.json#/$defs/resources`:

```json
{
  "type": "object",
  "minProperties": 1,
  "additionalProperties": false,
  "properties": {
    "cpu": {
      "type": "string",
      "pattern": "^(?:[0-9]*[1-9][0-9]*(?:\\.[0-9]+)?|[0-9]+\\.[0-9]*[1-9][0-9]*)$"
    },
    "memory": {"type": "string", "pattern": "^[1-9][0-9]*(Ki|Mi|Gi|Ti)$"},
    "pids": {"type": "integer", "minimum": 1, "default": 256}
  }
}
```

`workflow.schema.json#/$defs/network`:

```json
{"enum": ["none", "egress", "internal"], "default": "none"}
```

`workflow.schema.json#/$defs/egress`:

```json
{
  "type": "object",
  "required": ["allow"],
  "additionalProperties": false,
  "properties": {
    "allow": {
      "type": "array",
      "minItems": 1,
      "uniqueItems": true,
      "items": {
        "type": "string",
        "pattern": "^[^\\s/:]+:(?:6553[0-5]|655[0-2][0-9]|65[0-4][0-9]{2}|6[0-4][0-9]{3}|[1-5][0-9]{4}|[1-9][0-9]{0,3})$"
      }
    }
  }
}
```

`workflow.schema.json#/$defs/runsOn`:

```json
{
  "type": "array",
  "minItems": 1,
  "uniqueItems": true,
  "items": {"type": "string", "pattern": "^[^\\s=]+=[^\\s=]+$"}
}
```

`workflow.schema.json#/$defs/cache`:

```json
{"type": "boolean"}
```

## Examples

`/steps`:

```yaml
steps:
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d4a77b0c3e51d8a6f2b4c9e0a13d5f7b82c6e04a9d31b7f5c28e6a0b7e0
    inputs:
      orders: ${{ workflow.inputs.orders }}
    outputs:
    - ok
    - rejected
  archive:
    image: ghcr.io/acme/agk-archive@sha256:44de9033c0a15bb27e8d41f6a920cd73b58e1f0427cad96b3e8175d20f41c115
    needs:
    - step: normalize
      port: ok
      as: in
    merge: wait_all
    outputs:
    - out
```

`workflow.schema.json#/$defs/paramName`:

```yaml
url
```

`workflow.schema.json#/$defs/paramName`:

```yaml
method
```

`workflow.schema.json#/$defs/paramName`:

```yaml
endpoint
```

`workflow.schema.json#/$defs/paramName`:

```yaml
currency
```

`/steps`:

```yaml
steps:
  normalize:
    image: ghcr.io/acme/agk-normalize@sha256:9f2c1d4a77b0c3e51d8a6f2b4c9e0a13d5f7b82c6e04a9d31b7f5c28e6a0b7e0
    outputs:
    - ok
    - rejected
  .shared-egress:
    network: egress
    egress:
      allow:
      - vat.example.com:443
```

`workflow.schema.json#/$defs/step`:

```yaml
image: ghcr.io/acme/agk-invoice@sha256:1ab74e5c9d0b3f6182a47ce05d9b3f21708cd4a6e2b95f30c71d8ae64b2c39cc
needs:
- step: normalize
  port: ok
  as: in
if: ${{ inputs.in.count > 0 }}
strategy:
  fan_out: item
  max_parallel: 8
secrets:
- billing
network: egress
egress:
  allow:
  - api.billing.example.com:443
retry:
  max: 4
  on:
  - transient
outputs:
- out
- error
```

`/steps/*/image`:

```yaml
steps:
  normalize:
    image: ghcr.io/acme/agk-invoice@sha256:1ab74e5c9d0b3f6182a47ce05d9b3f21708cd4a6e2b95f30c71d8ae64b2c39cc
```

`/steps/*/image`:

```yaml
steps:
  normalize:
    image: python:3.13-slim@sha256:1c4d5e8f2a70b91c36d0ae47f5b82091c6d3a41e7059bfc2183d6a04e7b19a02
```

`/steps/*/params`:

```yaml
steps:
  normalize:
    params:
      currency: ${{ vars.currency }}
```

`/steps/*/params`:

```yaml
steps:
  normalize:
    params:
      endpoint: https://vat.example.com/v2
```

`/steps/*/timeout`:

```yaml
steps:
  normalize:
    timeout: 2m
```

`/steps/*/timeout`:

```yaml
steps:
  normalize:
    timeout: 10m
```

`/steps/*/resources`:

```yaml
steps:
  normalize:
    resources:
      cpu: '0.5'
      memory: 256Mi
```

`/steps/*/network`:

```yaml
steps:
  normalize:
    network: egress
```

`/steps/*/egress`:

```yaml
steps:
  normalize:
    egress:
      allow:
      - api.billing.example.com:443
```

`/steps/*/runs_on`:

```yaml
steps:
  normalize:
    runs_on:
    - zone=dmz
    - arch=arm64
```

`/steps/*/cache`:

```yaml
steps:
  normalize:
    cache: true
```

`/steps/*/workflow`:

```yaml
steps:
  normalize:
    workflow: finance/common
```

`/steps/*/workflow`:

```yaml
steps:
  normalize:
    workflow: finance/common@v2.1.0
```

`/steps/*/workflow`:

```yaml
steps:
  normalize:
    workflow:
      workflow: finance/common
      ref: v2.1.0
```

`/steps/*/workflow/workflow`:

```yaml
steps:
  normalize:
    workflow:
      workflow: finance/common
```

`/steps/*/workflow/ref`:

```yaml
steps:
  normalize:
    workflow:
      ref: v2.1.0
```

`/steps/*/workflow/ref`:

```yaml
steps:
  normalize:
    workflow:
      ref: a3f9c1e
```

`workflow.schema.json#/$defs/timeout`:

```yaml
2m
```

`workflow.schema.json#/$defs/timeout`:

```yaml
10m
```

`workflow.schema.json#/$defs/resources`:

```yaml
cpu: '1'
memory: 512Mi
```

`workflow.schema.json#/$defs/resources`:

```yaml
cpu: '0.5'
memory: 256Mi
pids: 128
```

`/defaults/resources/cpu`:

```yaml
defaults:
  resources:
    cpu: '1'
```

`/defaults/resources/cpu`:

```yaml
defaults:
  resources:
    cpu: '0.5'
```

`/defaults/resources/memory`:

```yaml
defaults:
  resources:
    memory: 256Mi
```

`/defaults/resources/memory`:

```yaml
defaults:
  resources:
    memory: 512Mi
```

`/defaults/resources/memory`:

```yaml
defaults:
  resources:
    memory: 2Gi
```

`/defaults/resources/pids`:

```yaml
defaults:
  resources:
    pids: 128
```

`/defaults/resources/pids`:

```yaml
defaults:
  resources:
    pids: 256
```

`workflow.schema.json#/$defs/network`:

```yaml
none
```

`workflow.schema.json#/$defs/network`:

```yaml
egress
```

`workflow.schema.json#/$defs/network`:

```yaml
internal
```

`workflow.schema.json#/$defs/egress`:

```yaml
allow:
- api.billing.example.com:443
```

`/defaults/egress/allow`:

```yaml
defaults:
  egress:
    allow:
    - api.billing.example.com:443
    - vat.example.com:443
```

`workflow.schema.json#/$defs/runsOn`:

```yaml
- arch=amd64
```

`workflow.schema.json#/$defs/runsOn`:

```yaml
- zone=dmz
- arch=arm64
```

`workflow.schema.json#/$defs/cache`:

```yaml
true
```

`workflow.schema.json#/$defs/cache`:

```yaml
false
```
