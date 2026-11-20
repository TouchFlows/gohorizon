# ADR 0004 — Single-owner lock, and what happens when Redis is down

**Status:** accepted (Milestone 1)
**Implements:** handoff §2.9, §14 (single-instance ownership lock), §14 recovery rules.

## Context

Two supervisors must never manage the same worker pool, and telemetry must never stop
jobs from executing. Those two requirements pull in opposite directions at startup:
the ownership lock needs Redis, and worker execution does not.

## Decision

1. **Startup requires the lock.** Before spawning any worker, the service performs
   `SET <coord:lock:supervisor> <owner> NX PX <ttl>`. If the key exists, the service
   exits with a distinct error ("another GoHorizon instance holds the supervisor lock")
   and never touches the worker pool. If Redis is unreachable at startup, the service
   exits too: starting workers without a lock could double-manage a pool, and that
   violates the invariant the lock exists to protect.
2. **Lock ownership is observable.** TTL is 30s; the service renews every 10s with a
   compare-and-pexpire Lua script, so a stalled instance cannot renew someone else's
   lock. The owner token is a random UUID per process.
3. **Losing Redis after startup degrades, it does not stop.** Supervision is local; a
   Redis outage means no telemetry ingestion and a stale lock, not stopped workers.
   Renewal failures are logged and reported through `/api/v1/health` as `degraded`
   with `lock.degraded` and `lock.last_error`. Workers keep running, restarts keep
   happening, and no job is touched.
4. **A lost lock is not a reason to kill workers.** If the key disappears or changes
   owner, the running instance keeps supervising (killing a healthy pool is worse) but
   reports `lock.held=false` as a critical condition. Recovery is deliberate: the
   operator restarts the service, and the startup rule in (1) applies again.
5. All lock timings are constants in one place; there is no configuration knob for
   them in v1 (a knob invites two instances with mismatched TTLs).
6. **The health status follows the lock and nothing else.** `/api/v1/health` answers
   `ok`/200 while the lock is held and `degraded`/503 once it is lost; no other
   condition participates. Telemetry ingestion failures never change `status` or the
   HTTP code: they are body-only, as the `telemetry.events`, `telemetry.duplicates`,
   `telemetry.invalid` and `telemetry.errors` counters plus `telemetry.failing` and
   `telemetry.last_error`. A 503 from a transient stream-read failure would make an
   automated health check restart a supervisor whose workers are healthy — a larger
   outage than the telemetry gap it reacted to. `failing` clears on the next
   successful read, so recovery never needs a restart.

## Consequences

- `systemd` should use `Restart=on-failure`: a lock conflict exits non-zero and does not
  loop forever spawning workers.
- Redis is a startup dependency but not a runtime single point of failure for job
  execution, which is what "the Laravel application must remain usable if GoHorizon is
  unavailable" (handoff §15) requires.
- Failover between two GoHorizon instances is explicitly not implemented; the lock only
  refuses the second one.
- `503` means lock loss only, so a process manager may restart on it. Telemetry health
  (`telemetry.failing`, `last_error`, counters) belongs in dashboards and alerts, not in
  restart decisions.
