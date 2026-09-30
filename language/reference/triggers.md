# `triggers`

What starts a run. A run started by a schedule, a webhook or an event is attributed to the namespace service identity rather than to whoever last edited the file, so production traffic does not stop when an author leaves. Three of the seven kinds of trigger are declared here; the other four are declared elsewhere or by the caller, and are listed in the comment beside this block. A manual run needs no declaration: any principal holding workflow:run starts one, supplying the inputs the workflow declares under inputs.

## Keywords

- `/on`: What starts a run. A run started by a schedule, a webhook or an event is attributed to the namespace service identity rather than to whoever last edited the file, so production traffic does not stop when an author leaves. Three of the seven kinds of trigger are declared here; the other four are declared elsewhere or by the caller, and are listed in the comment beside this block. A manual run needs no declaration: any principal holding workflow:run starts one, supplying the inputs the workflow declares under inputs.
- `/on/schedule`: Clock triggers, each a cron expression read in a named timezone.
- `/on/webhook`: HTTP entry points, each served below the namespaced prefix /hooks/<namespace>/ and authenticating the caller rather than a user.
- `/on/event`: Subscriptions to CloudEvents 1.0 carried on the bus. A workflow only sees events published into its own namespace, or into a namespace that granted it read access.
- `/on/schedule/*`: Starts a run on the clock, on the default branch. The run is attributed to the namespace service identity, so a schedule keeps working after its author is gone.
- `/on/schedule/*/cron`: Five-field cron expression: minute, hour, day of the month, month and day of the week, read in the timezone given beside it. A field is *, a value, a range a-b, * or a range stepped by /n, or a list of these joined by commas; a month may be written JAN to DEC and a day of the week SUN to SAT, with 0 and 7 both Sunday. Where both day fields are restricted, neither beginning with *, a day matching either runs, as in every cron since Vixie's. A value stepped alone, 5/15, is refused, since crons disagree on whether it means 5-59/15. No sixth field and no @daily: a schedule is read the same way by everyone who reads it, and five fields is what everyone reads.
- `/on/schedule/*/timezone`: Name of the IANA time zone the expression is read in, so that a schedule written for a country keeps its local hour across a daylight saving change. UTC when absent, never the installation's own zone, so that one file fires at the same instants on every installation it is pushed to. An occurrence a change skips runs at the first instant after the gap, and one a change repeats runs once, at its first instance.
- `/on/schedule/*/jitter`: Random delay of at most this long added to each occurrence, so that many workflows sharing an hour do not all start in the same second.
- `/on/schedule/*/catch_up`: Whether the occurrences missed while no controller led are made up once one leads again, each exactly once and with the instant it was due as trigger.scheduled_for. False by default, because a burst of late runs is rarely what a schedule meant: a missed occurrence is then skipped, and the next one runs on time.
- `/on/webhook/*`: Starts a run from an HTTP request. Paths are namespaced as /hooks/<namespace>/<path>, and authentication proves which caller is speaking, never which user.
- `/on/webhook/*/path`: Path the trigger answers on, below the namespaced prefix. Two namespaces can therefore use the same word without colliding, and within one namespace a path and a method answer one trigger: a push arming a second is refused. Each segment starts with a letter, a digit, an underscore, a tilde or a hyphen, so that no segment is . or .. and no path climbs out of its namespace's prefix once a proxy normalises it.
- `/on/webhook/*/method`: HTTP method the trigger accepts. A request arriving with another one is refused before a run is created. POST when absent, the method a sender delivers an event with.
- `/on/webhook/*/auth`: How the caller is proved: hmac over the raw body with a per-trigger secret, bearer for a token bound to a service account, mtls for client certificates, or none where the endpoint is deliberately open. hmac when absent, so that an endpoint is open only where the file says none.
- `/on/webhook/*/response`: What the caller gets back. async answers 202 with the run id; sync holds the request until the run finishes and returns the workflow output that output names. async when absent.
- `/on/webhook/*/output`: The workflow output a sync response returns, one of those the file declares under outputs. Required with response: sync and refused without it.
- `/on/webhook/*/map`: Fills workflow inputs from the request, through expressions over the trigger context. Without it a body is received and nothing reads it.
- `/on/event/*`: Starts a run on a CloudEvents 1.0 event published into a namespace with POST /api/v1/{ns}/events. A workflow only hears events published into its own namespace, or into the one namespace names where that namespace granted it read access.
- `/on/event/*/type`: CloudEvents type consumed. It is the event's own name for what happened, so a workflow subscribes to a fact rather than to a publisher.
- `/on/event/*/source`: CloudEvents source the event has to come from, for a workflow that cares which system said it.
- `/on/event/*/filter`: CEL expression evaluated over the event's context attributes and over its data. Only an event it accepts starts a run, which keeps a busy topic from starting runs that would immediately decide they had nothing to do.
- `/on/event/*/namespace`: Namespace whose events the subscription hears, the workflow's own where it names none. Another namespace's events are heard only while that namespace grants this workflow's namespace read access, its built-in identity holding workflow:read there, so that both namespaces chose it.
- `/on/event/*/map`: Fills workflow inputs from the event, through expressions over it, each input held to its schema before the run exists. Without it an event starts a run with the inputs' defaults, and nothing reads its data.

## Schema

`workflow.schema.json#/properties/on`:

```json
{
  "type": "object",
  "minProperties": 1,
  "additionalProperties": false,
  "properties": {
    "schedule": {"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/scheduleTrigger"}},
    "webhook": {"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/webhookTrigger"}},
    "event": {"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/eventTrigger"}}
  }
}
```

`workflow.schema.json#/$defs/scheduleTrigger`:

```json
{
  "type": "object",
  "required": ["cron"],
  "additionalProperties": false,
  "properties": {
    "cron": {
      "type": "string",
      "pattern": "^(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?)(?:,(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?))*\\s+(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?)(?:,(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?))*\\s+(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?)(?:,(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?))*\\s+(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?)(?:,(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?))*\\s+(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?)(?:,(?:\\*(?:/[0-9]+)?|[0-9A-Za-z]+(?:-[0-9A-Za-z]+(?:/[0-9]+)?)?))*$"
    },
    "timezone": {
      "type": "string",
      "pattern": "^[A-Za-z][A-Za-z0-9_+-]*(?:/[A-Za-z0-9_+-]+)*$",
      "not": {"const": "Local"},
      "maxLength": 64,
      "default": "UTC"
    },
    "jitter": {"$ref": "#/$defs/duration"},
    "catch_up": {"type": "boolean", "default": false}
  }
}
```

`workflow.schema.json#/$defs/webhookTrigger`:

```json
{
  "type": "object",
  "required": ["path"],
  "additionalProperties": false,
  "allOf": [
    {
      "if": {"required": ["response"], "properties": {"response": {"const": "sync"}}},
      "then": {"required": ["output"]}
    },
    {
      "if": {"required": ["output"]},
      "then": {"required": ["response"], "properties": {"response": {"const": "sync"}}}
    }
  ],
  "properties": {
    "path": {"type": "string", "pattern": "^(?:/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)+$", "maxLength": 255},
    "method": {"type": "string", "pattern": "^[A-Z]+$", "default": "POST"},
    "auth": {"enum": ["hmac", "bearer", "mtls", "none"], "default": "hmac"},
    "response": {"enum": ["async", "sync"], "default": "async"},
    "output": {"$ref": "#/$defs/portName"},
    "map": {"type": "object", "propertyNames": {"$ref": "#/$defs/identifier"}, "additionalProperties": {}}
  }
}
```

`workflow.schema.json#/$defs/eventTrigger`:

```json
{
  "type": "object",
  "anyOf": [{"required": ["type"]}, {"required": ["source"]}, {"required": ["filter"]}],
  "additionalProperties": false,
  "properties": {
    "type": {"type": "string", "minLength": 1},
    "source": {"type": "string", "minLength": 1},
    "filter": {"$ref": "#/$defs/expression"},
    "namespace": {"$ref": "#/$defs/namespace"},
    "map": {"type": "object", "propertyNames": {"$ref": "#/$defs/identifier"}, "additionalProperties": {}}
  }
}
```

## Examples

`/on`:

```yaml
on:
  schedule:
  - cron: 0 6 1 * *
    timezone: Europe/Paris
```

`/on`:

```yaml
on:
  webhook:
  - path: /invoicing
    method: POST
    auth: hmac
```

`/on/event`:

```yaml
on:
  event:
  - type: com.example.order.approved
```

`/on/schedule/*`:

```yaml
on:
  schedule:
  - cron: 0 6 1 * *
    timezone: Europe/Paris
    jitter: 5m
    catch_up: false
```

`/on/schedule/*/cron`:

```yaml
on:
  schedule:
  - cron: 0 6 1 * *
```

`/on/schedule/*/cron`:

```yaml
on:
  schedule:
  - cron: '*/15 * * * *'
```

`/on/schedule/*/cron`:

```yaml
on:
  schedule:
  - cron: 30 7 * * MON-FRI
```

`/on/schedule/*/cron`:

```yaml
on:
  schedule:
  - cron: 0 0 1,15 * *
```

`/on/schedule/*/timezone`:

```yaml
on:
  schedule:
  - timezone: Europe/Paris
```

`/on/schedule/*/timezone`:

```yaml
on:
  schedule:
  - timezone: UTC
```

`/on/schedule/*/timezone`:

```yaml
on:
  schedule:
  - timezone: America/Argentina/Buenos_Aires
```

`/on/schedule/*/jitter`:

```yaml
on:
  schedule:
  - jitter: 30s
```

`/on/schedule/*/jitter`:

```yaml
on:
  schedule:
  - jitter: 5m
```

`/on/schedule/*/catch_up`:

```yaml
on:
  schedule:
  - catch_up: false
```

`/on/schedule/*/catch_up`:

```yaml
on:
  schedule:
  - catch_up: true
```

`/on/webhook/*`:

```yaml
on:
  webhook:
  - path: /invoicing
    method: POST
    auth: hmac
    map:
      orders: ${{ trigger.body.orders }}
```

`/on/webhook/*`:

```yaml
on:
  webhook:
  - path: /quotes
    auth: bearer
    response: sync
    output: quote
```

`/on/webhook/*/path`:

```yaml
on:
  webhook:
  - path: /invoicing
```

`/on/webhook/*/path`:

```yaml
on:
  webhook:
  - path: /orders/approved
```

`/on/webhook/*/method`:

```yaml
on:
  webhook:
  - method: POST
```

`/on/webhook/*/method`:

```yaml
on:
  webhook:
  - method: PUT
```

`/on/webhook/*/auth`:

```yaml
on:
  webhook:
  - auth: hmac
```

`/on/webhook/*/auth`:

```yaml
on:
  webhook:
  - auth: bearer
```

`/on/webhook/*/response`:

```yaml
on:
  webhook:
  - response: async
```

`/on/webhook/*/response`:

```yaml
on:
  webhook:
  - response: sync
```

`/on/webhook/*/output`:

```yaml
on:
  webhook:
  - output: quote
```

`/on/webhook/*/output`:

```yaml
on:
  webhook:
  - output: invoices
```

`/on/webhook/*/map`:

```yaml
on:
  webhook:
  - map:
      orders: ${{ trigger.body.orders }}
```

`/on/event/*`:

```yaml
on:
  event:
  - type: com.example.order.approved
    filter: ${{ event.data.amount > 0 }}
```

`/on/event/*`:

```yaml
on:
  event:
  - type: com.example.order.approved
    source: /erp/orders
    namespace: sales
    map:
      orders: ${{ event.data.orders }}
```

`/on/event/*/source`:

```yaml
on:
  event:
  - source: /erp/orders
```

`/on/event/*/filter`:

```yaml
on:
  event:
  - filter: ${{ event.data.amount > 0 }}
```

`/on/event/*/namespace`:

```yaml
on:
  event:
  - namespace: sales
```

`/on/event/*/map`:

```yaml
on:
  event:
  - map:
      orders: ${{ event.data.orders }}
```
