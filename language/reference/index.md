# Agentiik workflow

The entry point of a workflow repository, agentiik.yaml at the root. It is the source of truth for what a workflow does, validated before a push is accepted, and the commit that carries it is the version. It is not the source of truth for who may do what: access grants live in the platform and never in this file.

## A complete minimal workflow

```yaml
apiVersion: agentiik.dev/v1
kind: Workflow
metadata:
  name: first-run
  namespace: demo
outputs:
  greeting:
    from:
      step: greet
      port: out
steps:
  greet:
    image: alpine:3.21
    script:
    - echo "hello from ${AGK_STEP}"
    outputs:
    - out
```

## Topics

- `repository`: The entry point of a workflow repository, agentiik.yaml at the root.
- `inputs`: Named values a run is started with, each validated against its schema before any step runs.
- `outputs`: The results a run is read by, each one a view of a single step port.
- `triggers`: What starts a run.
- `steps`: The steps of the workflow, named.
- `ports`: The output ports this step publishes.
- `needs`: The inbound edges of the step, which are the only form of dependency there is.
- `merge`: How several edges arriving on the same input port are combined.
- `fan_out`: How the step is split into shards and how many of them run at once.
- `retry`: When an attempt is made again, and how long the engine waits first.
- `expressions`: A CEL expression inside ${{ }}, evaluated by the controller.
- `secrets`: The secrets of the owning namespace this workflow uses, by name and by nothing else, resolved by the API at the last moment.
- `files`: Narrows what the step receives from the repository tree, which it otherwise gets whole under /agk/repo/.
- `script`: Shell commands run inside image instead of treating it as a brick, under the same contract every brick honours: the same mounts, the same environment, the same isolation, the same meaning for the exit code.
- `includes`: Files and repositories merged into this one before anything else is resolved, in declaration order, each read as an included file, $defs/fragment.
- `mcp`: Publishes the workflow to model-driven clients as an MCP server at /mcp/{namespace}/{workflow}.

## Schema parts

- `workflow`, Agentiik workflow: The entry point of a workflow repository, agentiik.yaml at the root. `https://schemas.agentiik.dev/workflow.schema.json`
- `brick`, Brick manifest: The manifest embedded in a brick image at /agk/brick.yaml. `https://schemas.agentiik.dev/brick.schema.json`
- `envelope`, Envelope: The JSON object { meta, items } that travels along a port. `https://schemas.agentiik.dev/envelope.schema.json`
