# `files`

Narrows what the step receives from the repository tree, which it otherwise gets whole under /agk/repo/. It is an optimisation for a large repository and never a permission boundary: everything in the tree is readable by anyone who can read the workflow. Each entry is a path or a glob relative to the root of the repository. A path names a file, or a directory with everything under it. A glob is matched against the path of every file of the tree, segment by segment: * matches any run of characters inside one segment, ? one character, [abc] one character of a set or a range, and ** written as a segment of its own matches zero or more whole segments, so ./sql/** is every file at any depth under sql/ and ./sql/**/*.sql every .sql file at any depth there, sql/orders.sql among them. What an entry selects is laid out where it is under /agk/repo/, unless the long form relocates it to wherever a tool insists on finding it.

## Keywords

- `/defaults/files`, `/steps/*/files`, `/.*/files`: Narrows what the step receives from the repository tree, which it otherwise gets whole under /agk/repo/. It is an optimisation for a large repository and never a permission boundary: everything in the tree is readable by anyone who can read the workflow. Each entry is a path or a glob relative to the root of the repository. A path names a file, or a directory with everything under it. A glob is matched against the path of every file of the tree, segment by segment: * matches any run of characters inside one segment, ? one character, [abc] one character of a set or a range, and ** written as a segment of its own matches zero or more whole segments, so ./sql/** is every file at any depth under sql/ and ./sql/**/*.sql every .sql file at any depth there, sql/orders.sql among them. What an entry selects is laid out where it is under /agk/repo/, unless the long form relocates it to wherever a tool insists on finding it.
- `/steps/*/files`, `/.*/files`: What this step receives from the repository tree, and where each path is placed.
- `/defaults/files/*/from`, `/steps/*/files/*/from`, `/.*/files/*/from`: The path or glob in the repository tree, relative to its root and read as the short form is: a file, a directory with everything under it, or every file a glob matches.
- `/defaults/files/*/to`, `/steps/*/files/*/to`, `/.*/files/*/to`: Where it is placed in the container, for a tool that will only look in one place, as an absolute path. For a path naming a file, the path the file takes. For a directory or a glob, the directory what it selects is placed under, each file keeping its path below the directory named, or below the glob's fixed prefix, the segments before the first one holding a wildcard: { from: ./sql/**/*.sql, to: /docker-entrypoint-initdb.d } places sql/orders.sql at /docker-entrypoint-initdb.d/orders.sql and sql/2026/q1.sql at /docker-entrypoint-initdb.d/2026/q1.sql, so that no two files one glob selects land on one path. Absolute because a relative path has nothing inside a container to be relative to, which is why the runner refuses one and the task message holds it to the same grammar.
- `/defaults/files/*/mode`, `/steps/*/files/*/mode`, `/.*/files/*/mode`: The permissions it is given, as an octal string, every file a directory or a glob selects taking the same. It decides whether the file is executable, and never who may read it: every file a step is given is laid out readable by anybody and writable by nobody, 0444 or 0555, since the container reads it through a read-only bind as the image's user, in a remapped range the runner does not know, and a narrower mode would keep the file from the step that asked for it. What must not be readable by everything in the container is a secret, which has its own store and its own mount.

## Schema

`workflow.schema.json#/$defs/step/properties/files`:

```json
{"$ref": "#/$defs/files"}
```

`workflow.schema.json#/$defs/files`:

```json
{
  "type": "array",
  "minItems": 1,
  "items": {
    "oneOf": [
      {"type": "string", "minLength": 1},
      {
        "type": "object",
        "required": ["from"],
        "additionalProperties": false,
        "properties": {
          "from": {"type": "string", "minLength": 1},
          "to": {"type": "string", "pattern": "^/", "minLength": 1},
          "mode": {"type": "string", "pattern": "^0?[0-7]{3}$"}
        }
      }
    ]
  }
}
```

## Examples

`workflow.schema.json#/$defs/files`:

```yaml
- ./sql/**
- from: ./certs/internal-ca.pem
  to: /etc/ssl/certs/internal-ca.pem
  mode: '0444'
```

`workflow.schema.json#/$defs/files`:

```yaml
- from: ./sql/**/*.sql
  to: /docker-entrypoint-initdb.d
  mode: '0444'
```

`/steps/*/files`:

```yaml
steps:
  normalize:
    files:
    - ./sql/**
```

`/defaults/files/*/from`:

```yaml
defaults:
  files:
  - from: ./certs/internal-ca.pem
```

`/defaults/files/*/from`:

```yaml
defaults:
  files:
  - from: ./sql/**
```

`/defaults/files/*/from`:

```yaml
defaults:
  files:
  - from: ./sql/**/*.sql
```

`/defaults/files/*/to`:

```yaml
defaults:
  files:
  - to: /etc/ssl/certs/internal-ca.pem
```

`/defaults/files/*/to`:

```yaml
defaults:
  files:
  - to: /docker-entrypoint-initdb.d
```

`/defaults/files/*/mode`:

```yaml
defaults:
  files:
  - mode: '0444'
```

`/defaults/files/*/mode`:

```yaml
defaults:
  files:
  - mode: '0600'
```
