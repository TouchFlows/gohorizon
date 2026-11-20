# ADR 0001 — Queue inspection and failed jobs are served by the Laravel adapter

**Status:** accepted (Milestone 1)
**Supersedes:** handoff §3.2 (Redis adapter "queue depth sampling"), §6.5 (Redis queue
inspection), and the implication that Go can enumerate queues and failed jobs itself.

## Context

The handoff assigns GoHorizon a Redis adapter responsible for "queue depth sampling"
and queue inspection, with Redis-specific inspection isolated in a version-tested
driver adapter. It also lists failed jobs under Redis inspection.

Laravel 13 changed the facts:

- The `Illuminate\Contracts\Queue\Queue` contract now exposes `pendingSize()`,
  `delayedSize()`, `reservedSize()` and `creationTimeOfOldestPendingJob()`; `RedisQueue`
  implements them, plus `pendingJobs()`, `delayedJobs()`, `reservedJobs()` and the
  `InspectedJob` DTO (`docs/laravel-13-recon.md`).
- Failed jobs are not in Redis at all under the default `database-uuids` failer; they
  live in the application's `failed_jobs` table.
- Redis queue keys are prefixed by the application's `redis.options.prefix`, and on a
  cluster they become `queues:{name}`. A Go-side reader would need the app's prefix,
  cluster mode, and `retry_after` semantics to interpret anything.

Reading those keys from Go would mean reimplementing a framework detail the framework
already exposes, and would make GoHorizon break on a Laravel change the framework
itself considers internal.

## Decision

1. Go never issues a command against a Laravel queue key, the failed-job store, or any
   key outside the GoHorizon namespace. There is no "queue adapter" in Go.
2. Queue counts and queue metadata come from the package commands
   `php artisan gohorizon:inspect --json` (contract methods only) and
   `php artisan gohorizon:capabilities --json` (drivers, `retry_after`, `pcntl`,
   versions, event availability).
3. Failed-job listing, retry and delete are adapter operations
   (`gohorizon:failed:list|retry|forget` to be added in the dashboards/administration
   slice). They delegate to Laravel's failer and `queue:retry`/`queue:forget`
   semantics, never to raw SQL written by Go.
4. Go schedules inspection by executing the adapter command in the application
   directory with the configured PHP binary. A package that is not installed degrades
   the feature (counts unavailable, "adapter_missing" capability) instead of breaking
   supervision.

## Consequences

- GoHorizon works with any queue driver the framework supports, not just Redis.
- Two documented, testable seams (stdout JSON from two artisan commands) instead of
  one undocumented Redis-key seam.
- Queue counts are as fresh as the inspection interval; they are observations, never
  an execution ledger (handoff §14.7 is preserved).
- GoHorizon cannot monitor an application that refuses to install the package; worker
  supervision still works, and the capability report explains the gap.
