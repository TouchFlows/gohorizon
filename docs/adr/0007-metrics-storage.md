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
   row cost of one per queue per busy minute; coarser rollups and retention windows from
   handoff §9 are deliberately not implemented yet.
3. **Exactly-once by event id.** `recorded_events(id)` is inserted in the same
   transaction as the bucket update, and a duplicate insert aborts the update. This makes
   the sink idempotent across restarts, which is what allows a safe retry. The table
   carries no timestamp yet: nothing prunes it, and the column arrives with the retention
   windows that would use it.
4. **The sink failure policy differs from the ack policy.** A failed write counts as a
   telemetry error and leaves the batch unacknowledged, so the drain path retries it
   instead of dropping the metric. Ingestion backs off; no worker is affected. The event
   is marked seen only after the write succeeds, so the retry is not filtered out by the
   in-memory dedupe window.
5. **Storage failure is not a startup failure.** If the database cannot be opened, the
   service logs a warning and supervises without history. A metrics outage must not take
   healthy workers down (contrast the capabilities preflight, which protects job
   correctness and therefore does refuse to start).
6. **No dashboard coupling.** Nothing in this package formats or serves the data; the
   read API and UI are later slices. `Buckets` exists so history can be read back and
   tested.

## Consequences

- History survives a restart, including the dedupe record, so a replayed stream entry is
  not counted twice.
- Per-queue aggregates are the unit the handoff's balancing and dashboard work will read;
  no schema change is needed for a per-queue view, only for new rollup granularities.
- `recorded_events` grows one row per consumed event. Pruning with the retention windows
  is deferred with them; it needs the event timestamp added back when it lands.
- A single connection (`SetMaxOpenConns(1)`) serialises writes; that is enough for one
  consumer and is revisited when the read API arrives.
