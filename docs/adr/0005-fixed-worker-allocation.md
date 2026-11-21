# ADR 0005 — Fixed allocation binds one worker to one queue

**Status:** accepted (Milestone 1)
**Implements:** handoff §9 phase 1, and its warning about identical multi-queue pools.

## Context

The handoff's worker launch example uses a single `--queue=default` and then asks for
dynamic balancing across several queues. It explicitly warns against launching every
worker with the same multi-queue list and calling it balancing.

Laravel 13 confirms why: `Worker::getNextJob()` walks the comma-separated list in
order and `RedisQueue::pop()` only blocks on index 0. A worker with
`--queue=emails,notifications` drains `emails` first, always; `notifications` is served
only when `emails` is empty. Ten identical workers are therefore one pool with a
priority order, and per-queue scaling is impossible.

## Decision

1. In Milestone 1 (`balance.strategy: fixed`) each worker slot is assigned exactly one
   queue: `queues[slot % len(queues)]`. Worker 0 → `default`, worker 1 → `emails`, and
   so on; `workers.min` is the total pool size.
2. `--queue` is always rendered with a single queue name from Go. A multi-queue worker
   is never generated, so the "identical list" anti-pattern cannot be introduced by
   configuration.
3. `simple`/`auto`/`scaling_strategy` are rejected by configuration validation in v1
   with an error that names Milestone 4. Accepted-but-ignored strategies would make the
   dashboard lie about allocation.
4. Growth beyond `min` is never automatic, so `workers.max` must equal `workers.min`
   under `fixed`: validation rejects a larger max with an error naming the balancing
   milestone, rather than accepting a pool size the supervisor cannot reach.
5. Queue-level metrics (pending, reserved, delayed) come from the adapter's inspection
   command, keyed by the same queue names used for allocation, so a later balancing
   decision reads the same names it would act on.

## Consequences

- "Scale `emails` workers" is a real, expressible operation from the first slice.
- The default pool for a single-queue application is unchanged: N workers on that queue.
- An application with three queues and one worker still serves one queue only; the
  configuration must set `workers.min` to at least the number of queues it wants served
  concurrently. Nothing detects an undersized pool: validation rejects pool sizes that
  are non-positive, above the v1 ceiling, or unequal under `fixed`, and reports every
  problem as an error, so it has no warning channel for this. The mapping is visible
  instead in the `queues[slot % len(queues)]` assignment, which shows exactly which
  queues the configured pool serves.
