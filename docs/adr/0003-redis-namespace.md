# ADR 0003 — Redis namespace split and key ownership

**Status:** accepted (Milestone 1)
**Implements:** handoff §3.2 (separate namespaces), §6.3 (stream name).

> The telemetry stream is published with a raw `XADD` so it lands on the literal key
> below. Laravel applies a per-connection key prefix by default (derived from
> `APP_NAME`), and the Go consumer has no notion of that prefix, so a normal command
> would write to an unreachable key and telemetry would silently disappear.

## Context

GoHorizon shares one Redis instance with the Laravel application. Three kinds of keys
coexist: the application's queue data, GoHorizon telemetry, and GoHorizon
coordination. The handoff suggests the prefix `gohorizon:{application}:{environment}:`
and the stream `gohorizon:{application}:{environment}:events`, without saying how the
three namespaces stay disjoint.

The Laravel application owns `queues:*` (with its own `options.prefix`) and
`illuminate:queue:restart`. GoHorizon must never read or write those.

## Decision

One scope prefix, then disjoint segments:

```
gohorizon:{application}:{environment}:events                  telemetry stream
gohorizon:{application}:{environment}:coord:lock:supervisor    ownership lock
gohorizon:{application}:{environment}:coord:supervisor:{id}    supervisor control
gohorizon:{application}:{environment}:metrics:*                aggregates (later)
```

- `events` matches handoff §6.3 verbatim.
- Everything coordination-related sits under `coord:`; everything aggregate-related
  under `metrics:`. No future key may be added outside these two segments, and the
  package and the Go service construct the stream key with the same function on both
  sides (`internal/redis/keys.go`, `src/Redis/Keys.php`), asserted by tests.
- `{application}` and `{environment}` are restricted to `[a-z0-9][a-z0-9-]*` so that a
  name can never contain `:`, whitespace or a cluster hash tag. Validation happens at
  config load and again in the key builder.
- Only the `redis` package builds key strings; nothing else receives the raw
  `redis.Client`. There is no generic `Do`/`Keys`/`Scan` escape hatch, so a query
  against an application key cannot be written accidentally.
- `SELECT`-able `db` is taken from configuration and must match the application's
  connection; GoHorizon does not copy the application's Redis prefix.

## Consequences

- A `KEYS *`-style scan is impossible by construction; there is no place to put it.
- Cluster mode is out of scope for v1 (handoff §18): the key builder is not hash-tagged,
  and the supported topology is documented as a single Redis node or a non-cluster
  proxy. Supporting cluster later means adding a hash tag decision here, in one file.
- Renaming an application or environment orphans the old namespace; that is deliberate
  (telemetry is not migrated, it expires by retention).
