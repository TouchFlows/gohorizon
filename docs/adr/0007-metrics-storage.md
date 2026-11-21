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
2. **Aggregates plus one row per attempt.** `job.finished` events are folded into
   `queue_buckets(queue, bucket_start, processed, released, failed, timed_out,
   duration_ms)`, one row per queue per minute (UTC). The job count is the sum of the
   outcome counters, so it is not stored twice. Minutes keep recent history useful at a
   row cost of one per queue per busy minute; `Rollup` folds them into the hourly and
   daily levels (point 7).

   The same write also stores the attempt itself in
   `jobs(id, job_id, uuid, class, queue, connection, outcome, attempts, duration_ms,
   finished_at, exception_class, exception_message)`, keyed by the event id so a replay
   cannot duplicate it. `job_id` is the job's identity — its uuid, or `id:` plus the queue
   job id when Laravel assigned none, which is the fallback the recorder itself uses — so
   the attempts of one job can be read together. Indexes serve the questions job history
   asks: `(finished_at DESC, id)`, `(job_id, finished_at DESC)`, `(queue, finished_at
   DESC, id)` and `(outcome, finished_at DESC, id)`. The event id is the tiebreak in each:
   jobs finish in the same second, and a keyset page is then read in index order instead of
   being sorted. These rows are observations, not a ledger: they are bounded by the raw
   window, and a missing row never affects a worker.
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
   serves one queue's `Buckets` over `[from, to)` at the requested `resolution`
   (minute by default), read-only. `GET /api/v1/jobs` serves one page of `Job` rows newest
   first, filtered by queue and outcome and paged by the `(finished_at, id)` keyset, and
   `GET /api/v1/jobs/{id}` serves the newest attempt of one identity. The dashboard remains a
   later slice.
7. **Bounded growth follows handoff §9.** `Rollup` folds closed minute buckets into
   hourly buckets and closed hourly buckets into daily ones; `Prune` then deletes event
   ids, job rows and each aggregate level past its window. Both run at startup and then hourly,
   rollup first: pruning minutes before rolling them up would leave holes in the hours
   and days built from them, so a failed rollup skips that prune pass. Windows come from
   `storage.retention` (defaults 72 h raw — the upper end of the handoff's window, so an
   outage still recognises replays — then 7 d minutes, 30 d hours, 365 d days), and each
   level must outlive the level it is built from.
8. **Rollups recompute, they do not increment.** Every pass groups the source table for
   closed periods and writes absolute sums with an upsert. Running a pass twice cannot
   double count, and a late event inside an already closed hour is absorbed by the next
   pass. The hour in progress is excluded; the day in progress collects its completed
   hours and is recomputed as more arrive.

## Consequences

- History survives a restart, including the dedupe record, so a replayed stream entry is
  not counted twice.
- Per-queue aggregates exist at minute, hour and day resolution for the handoff's
  balancing and dashboard work; `GET /api/v1/metrics?resolution=` serves all three.
- The database is bounded: ids and job rows live 72 hours, minute buckets 7 days, so
  job history is a recent window, not an archive, and a failed job older than it is gone.
  Volume is the cost: a busy queue writes one row per attempt, which dwarfs the aggregates,
  and the prune pass is the only bound. An event
  redelivered after its id was pruned would be counted again, but the stream's own
  `telemetry.retention` window is far shorter than 72 hours in practice.
- Schema changes before the first release are applied by recreating the database;
  golang-migrate (handoff §5) arrives with the first upgrade that must preserve data.
- A single connection (`SetMaxOpenConns(1)`) serialises writes and the read API's
  queries; that is enough for one consumer and a bounded range per request, and is
  revisited if read latency starts delaying ingestion.

## Amendment (Milestone 2)

The job rows are also what the dashboard reads, through `GET /api/v1/jobs`. A page is the
`limit + 1` rows newest first from the cursor: the extra row decides whether another page
exists, and `next_before` carries the last returned row's `(finished_at, id)` pair as an
opaque cursor. Keyset paging is what keeps a page cheap at any depth and what stops an attempt
recorded in the same second as the cursor from being repeated or skipped. The indexes named in
point 2 are exactly the four reads that page serves — all rows, one queue, one outcome and one
queue's outcome — each ending in `(finished_at DESC, id)`. Reads change nothing, are bounded
by the raw window, and are exercised through a page only by the browser smoke in
`tests/browser`; the rest is covered against the store directly.
