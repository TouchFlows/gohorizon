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
3. **A renewal error degrades, it does not stop.** An unreachable Redis may mean a
   stale lock, or it may only mean telemetry cannot be written. It is not proof that
   this instance lost the pool, and stopping healthy workers would turn a telemetry
   outage into a job outage. Renewal errors are logged and reported through
   `/api/v1/health` as `degraded` with `lock.degraded` and `lock.last_error`; workers
   keep running and no job is touched until Redis answers again.
4. **A definitively lost lock fences the supervisor.** Redis answering that the key is
   gone or held by another token means another instance may supervise the same pool
   now. The service stops every worker through the normal graceful path (an in-flight
   job still finishes and is accounted for), stops ingestion after its final pass, and
   exits non-zero with `ErrLockLost`. It does not wait for an operator: the pool is
   released within one renewal interval, and the startup rule in (1) applies again on
   the next start. The release script compares the owner token, so a fenced instance
   never deletes the lock the new owner holds.
5. **Fencing is not failover.** A fenced instance exits instead of becoming a standby;
   the pool is free, not moved. Restarting it is what makes it supervise again, and a
   supervisor that can reach Redis at startup takes the lock as usual.
6. All lock timings are constants in one place; there is no configuration knob for
   them in v1 (a knob invites two instances with mismatched TTLs).
7. **The health status follows the lock and nothing else.** `/api/v1/health` answers
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
  refuses the second one, and a loser releases the pool by exiting.
- A fenced process exits non-zero within one renewal interval (10s), so its `degraded`
  `/api/v1/health` answers exist only during that short window; `503` still means lock
  loss only, so a process manager may restart on it. Telemetry health
  (`telemetry.failing`, `last_error`, counters) belongs in dashboards and alerts, not in
  restart decisions.
- A stop caused by fencing takes as long as a normal graceful stop: the shutdown grace of
  each supervisor, plus the bounded telemetry drain.
