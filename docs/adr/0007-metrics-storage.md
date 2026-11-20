# ADR 0007 — Durable per-queue metrics in SQLite

**Status:** accepted (Milestone 1)
**Implements:** handoff §3.2 (metrics aggregation), §9 (historical metrics), §5 (SQLite).

## Context

The consumer reads, deduplicates and acknowledges telemetry, but it only kept counters in
memory: after a restart, all history was gone. Handoff §9 asks for historical metrics in
SQLite with minute/hour/day retention and configurable windows, and explicitly keeps the
dashboard separate from collection.

Two constraints shape the storage:

- The stream is at-least-once. An entry can be redelivered after an ack failure or a
  crash, and the in-memory dedupe window does not survive a restart.
- Telemetry must never stop jobs (handoff §2.11). A storage failure may cost metrics, but
  it may not block workers or stop ingestion for good.

## Decision

1. **SQLite at `storage.path`**, opened by `internal/metrics` (driver: `modernc.org/sqlite`,
   the pure-Go build chosen in handoff §5). The directory is created if missing.
2. **Aggregates, not an event log.** `job.finished` events are folded into
   `queue_buckets(queue, bucket_start, processed, released, failed, timed_out,
   duration_ms)`, one row per queue per minute (UTC). The job count is the sum of the
   outcome counters, so it is not stored twice. Minutes keep recent history useful at a
   row cost of one per queue per busy minute; the coarser hourly and daily rollups from
   handoff §9 are not implemented yet.
3. **Exactly-once by event id.** `recorded_events(id, recorded_at)` is inserted in the
   same transaction as the bucket update, and a duplicate insert aborts the update. This
   makes the sink idempotent across restarts, which is what allows a safe retry.
4. **The sink failure policy differs from the ack policy.** A failed write counts as a
   telemetry error and leaves the batch unacknowledged, so the drain path retries it
   instead of dropping the metric. Ingestion backs off; no worker is affected. The event
   is marked seen only after the write succeeds, so the retry is not filtered out by the
   in-memory dedupe window.
5. **Storage failure is not a startup failure.** If the database cannot be opened, the
   service logs a warning and supervises without history. A metrics outage must not take
   healthy workers down (contrast the capabilities preflight, which protects job
   correctness and therefore does refuse to start).
6. **No dashboard coupling.** This package formats nothing. `GET /api/v1/metrics`
   serves one queue's `Buckets` over `[from, to)`, read-only; the dashboard remains a
   later slice.
7. **Bounded growth follows handoff §9.** `Prune` deletes event ids older than 72 hours
   (the handoff's 24–72 hour raw window, at its upper bound, so an outage still
   recognises replays) and minute buckets older than 7 days. It runs at startup and then
   hourly, which bounds the overshoot to one interval. Hourly (30 days) and daily (12
   months) rollups do not exist yet, so there is nothing to prune for them. The windows
   are constants for now; they become configuration when the rollups that need their own
   windows arrive.

## Consequences

- History survives a restart, including the dedupe record, so a replayed stream entry is
  not counted twice.
- Per-queue aggregates are the unit the handoff's balancing and dashboard work will read;
  no schema change is needed for a per-queue view, only for new rollup granularities.
- The database is bounded: ids live 72 hours, minute buckets 7 days. An event
  redelivered after its id was pruned would be counted again, but the stream's own
  `telemetry.retention` window is far shorter than 72 hours in practice.
- Schema changes before the first release are applied by recreating the database;
  golang-migrate (handoff §5) arrives with the first upgrade that must preserve data.
- A single connection (`SetMaxOpenConns(1)`) serialises writes and the read API's
  queries; that is enough for one consumer and a bounded range per request, and is
  revisited if read latency starts delaying ingestion.
