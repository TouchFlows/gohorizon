# Laravel 13 recon — verified queue behaviour

Status: recon for Milestone 1. Every claim below was checked against Laravel 13.x
framework source or the 13.x documentation, not assumed from Horizon's behaviour.

Sources:

- `laravel/framework` branch `13.x`:
  - `src/Illuminate/Queue/Worker.php`
  - `src/Illuminate/Queue/RedisQueue.php`
  - `src/Illuminate/Queue/WorkerStopReason.php`
  - `src/Illuminate/Queue/Console/WorkCommand.php`
  - `src/Illuminate/Queue/Events/WorkerStopping.php`
  - `src/Illuminate/Contracts/Queue/Queue.php`
- `laravel/framework` branch `12.x` (`Worker.php`, for the diff)
- https://laravel.com/docs/13.x/releases
- https://laravel.com/docs/13.x/upgrade
- https://laravel.com/docs/13.x/queues

## Release facts

| Fact | Value |
|---|---|
| Released | 17 March 2026 |
| Minimum PHP | 8.3 (supported range 8.3 – 8.5) |
| Queue transport assumption | unchanged: Redis driver, `queues:<name>` LIST + `:delayed`/`:reserved` ZSET + `:notify` LIST |
| Worker model | unchanged: long-lived `queue:work` daemon, `pcntl` async signals |

## Worker lifecycle (Worker.php, 13.x)

**Signals.** `listenForSignals()` registers, via `pcntl_async_signals`:

| Signal | Effect |
|---|---|
| `SIGTERM`, `SIGINT`, `SIGQUIT` | `shouldQuit = true`, dispatch `WorkerInterrupted`, notify an `Interruptible` job via `interrupted($signal)` |
| `SIGUSR2` | `paused = true`, dispatch `WorkerPausing` |
| `SIGCONT` | `paused = false`, dispatch `WorkerResuming` |
| `SIGALRM` | job timeout handler |

`shouldQuit` is only read in `stopIfNecessary()`, which runs **after** the current job
finishes (or after the sleep/blocking-pop returns). So SIGTERM is graceful for the
job in flight, but the exit is not instantaneous: worst case is one job duration, or
one `--sleep` interval while idle.

**Exit codes.**

| Code | Meaning |
|---|---|
| `0` | success: interrupted, lost connection, restart signal, queue empty, `--max-jobs`, `--max-time` |
| `1` | error, including job timeout (unless overridden by `Worker::$timedOutExitCode`) |
| `12` | memory limit exceeded (unless overridden by `Worker::$memoryExceededExitCode`) |

`WorkerStopReason` (enum, string) is the machine-readable reason, but it is only
available **inside the process** through `WorkerStopping`; it is not encoded in the
exit code. A supervisor watching only the exit status cannot distinguish
"recycled after `--max-jobs`" from "exited because someone ran `queue:restart`" from
"queue empty".

**Timeout is not graceful.** On `SIGALRM` the worker marks the job failed/attempted,
dispatches `JobTimedOut`, then `kill()` calls `posix_kill(getmypid(), SIGKILL)` and
`exit()`. The worker is killed by its own SIGKILL. `--timeout` requires the `pcntl`
extension; without it the option silently does nothing.

**Restart / pause channels.** `queue:restart` writes a timestamp to the cache key
`illuminate:queue:restart`; each worker compares it every loop and exits with reason
`restart_signal`, exit code 0. `queue:pause`-style pause state is read from the cache
through `QueueManager::getPausedQueues()`. Laravel 13 also honours `SIGUSR2`/`SIGCONT`
for pause/resume without stopping the process.

**`queue:work` CLI (13.x signature).**

```
queue:work {connection?}
  --name=default   --queue=            --once      --stop-when-empty
  --stop-when-empty-for=0               --backoff=0 --max-jobs=0
  --max-time=0     --memory=128        --sleep=3    --rest=0
  --timeout=60     --tries=1           --force      --json
```

Notable in 13: `--memory` accepts a percentage of PHP's `memory_limit` (e.g. `60%`);
defaults are `--tries=1` and `--memory=128`, not the values in the handoff example;
`--name` exists, which gives a supervisor a way to stamp process identity onto argv;
`--json` emits one JSON object per lifecycle event on stdout.

**Multi-queue pop.** `getNextJob()` splits `--queue` on commas and pops in order,
passing the queue index to `pop($queue, $index)`. `RedisQueue::pop()` only performs a
blocking `BLPOP` on the notify list when `$index === 0`; a job found on a secondary
queue sets `secondaryQueueHadJob`, which suppresses blocking on the next iteration.
Queues remain strictly priority-ordered; there is no round-robin and no per-queue
fair share.

## Queue inspection (RedisQueue.php, 13.x)

Laravel 13 adds real inspection to the `Queue` contract:

```php
public function pendingSize($queue = null);                       // LLEN queues:<q>
public function delayedSize($queue = null);                       // ZCARD queues:<q>:delayed
public function reservedSize($queue = null);                      // ZCARD queues:<q>:reserved
public function creationTimeOfOldestPendingJob($queue = null);    // payload createdAt of LINDEX 0
```

`RedisQueue` also ships `size()`, `totalSize()`, `pendingJobs()`, `delayedJobs()`,
`reservedJobs()`, `allPendingJobs()`, `allDelayedJobs()`, `allReservedJobs()`, and an
`Illuminate\Queue\Jobs\InspectedJob` DTO built from a payload. On a cluster connection
keys become `queues:{name}` (hash tag), and `allQueueNames()` discovers them with
`KEYS queues:*` (single node) or `SCAN` (cluster).

Redis key layout, from the same file: `queues:<name>` (list), `:notify` (list used
for blocking pop), `:delayed` (zset), `:reserved` (zset). Laravel's Redis
`options.prefix` prefixes all of them. `retryAfter` moves expired reservations back
onto the main list; Go must not participate in that migration.

## Event payload changes that matter

- `JobAttempted` in 13 carries the exception **object** (`$exception`, null on success).
  In 12 it carried the boolean `$exceptionOccurred`. Code written against 12 breaks.
- `QueueBusy::$connection` was renamed `$connectionName`.
- `JobProcessed` is **not** proof of success. `Worker::process()` calls
  `raiseAfterJobEvent()` also when the job was deleted because it already exceeded its
  attempts, and a job can be released or failed during `handle()`. Listeners must check
  `$job->hasFailed()`, `$job->isReleased()`, `$job->isDeleted()`.
- `JobReleasedAfterException` exists (12.x and 13.x) with `$connectionName`, `$job`,
  `$backoff` — the handoff's "if available" hedge is satisfied.
- `JobProcessing`, `JobProcessed`, `JobExceptionOccurred`, `JobFailed` keep their
  `($connectionName, $job[, $exception])` shape.
- 13 adds `WorkerInterrupted`, `WorkerIdle`, `WorkerPausing`, `WorkerResuming`,
  `WorkerQueuePaused`, `WorkerQueueResumed`.
- `WorkerStopping` in 13: `($status, $workerOptions, $reason, $jobsProcessed,
  $lastJobProcessedAt, $memoryUsage, $connectionName, $queue)`.

Failed jobs are **not** stored in Redis by default. The default failer is
`database-uuids`, writing to the application's `failed_jobs` table; retry/forget go
through `queue:retry` / `queue:forget` and therefore through the application's
database, not through the queue transport.

## Where the handoff contradicts the framework

| # | Handoff says | Laravel 13 reality | Resolution |
|---|---|---|---|
| 1 | §3.2/§6.5: failed-job monitoring and Redis queue inspection are GoHorizon's job, with a "version-tested Redis driver adapter". | Failed jobs live in the app DB (default `database-uuids`). Queue counts are exposed by the 13 `Queue` contract; `RedisQueue` also exposes inspected-job collections. | Inspection and failed jobs go through the Laravel package (ADR 0001). Go never reads `queues:*` or `failed_jobs`. |
| 2 | §2.5: never assume undocumented Redis key structures. | The key layout is documented only implicitly by `RedisQueue`; `allQueueNames()` itself uses `KEYS`, which is unsafe on large keyspaces. | Same as 1: Go has no Redis queue access at all, so the assumption disappears instead of being defended. |
| 3 | §7.3 states `Crashed` as the failure state, §7.4 assumes signal-only escalation. | Exit `0` can mean `restart_signal`/`max_jobs`/`max_time`/`queue_empty`; exit `12` is a memory recycle; exit `1` can be a timeout where the worker SIGKILLs itself. | Exit status is classified (stable/crash) and `WorkerStopReason` is preferred when the adapter reports it (ADR 0002). |
| 4 | §7.4: graceful shutdown with a grace period, escalate per policy. | Graceful SIGTERM can take up to one job duration or one `--sleep`; a timed-out worker dies by SIGKILL immediately, and `--timeout` is inert without `pcntl`. | Grace is bounded and escalation is logged; the capabilities preflight reports `pcntl` and startup refuses a timeout when it is missing. |
| 5 | §6.1/§8: `worker.memory` is an integer number of megabytes. | 13 accepts an integer **or** a percentage (`60%`) of PHP's `memory_limit`. | Config accepts both and passes the literal value through to the CLI. |
| 6 | §9: "do not launch every worker against an identical multi-queue list". | `--queue=a,b,c` is strictly priority-ordered, and only index 0 blocks; secondary queues never get a fair share. | Fixed allocation binds each worker to exactly one queue; multi-queue workers are not generated (ADR 0005). |
| 7 | §6.2 lists five lifecycle events as the telemetry set. | `JobProcessed` fires for deleted/over-limit jobs too, so status cannot come from the event name alone. | Telemetry derives status from job flags and emits a single terminal event per attempt (ADR 0006). |
| 8 | §12/§11: pause/resume a supervisor as an administrative action. | Laravel 13 supports `SIGUSR2` pause and `SIGCONT` resume, plus cache-based queue pause, without terminating workers. | Supervisor pause maps to signals, not to stop/start (deferred to the API milestone; the supervisor exposes the primitive). |
| 9 | §6.3: telemetry stream named `gohorizon:{application}:{environment}:events`. | Compatible as-is. | Kept verbatim; coordination gets a disjoint `coord:` segment (ADR 0003). |
| 10 | §5/§6.5: worker launch hard-codes `php artisan queue:work redis`. | `queue:work {connection?}`, and the app's default connection may not be Redis; timeouts must stay below the connection's `retry_after`, which only the app knows. | Launch uses the configured connection name, and `gohorizon:capabilities` reports each connection's driver and `retry_after` so Go can validate before spawning. |
| 11 | §6.2: capture job tags. | Laravel core has no job tag concept. The only source is Horizon's `tags()` method convention on the job class. | Tags are read from an optional `tags()` method and are otherwise an empty list; the capability report says so (ADR 0006). |

## Open items for the next slice

- Go-side telemetry ingestion (consumer group, dedupe, idempotency) — the package
  publishes, nothing consumes yet.
- Adapter-side `queue:work --json` ingestion as a fallback for applications that will
  not install the package.
- `WorkerStopping` reason overriding exit-code inference: the reason is not consumed
  until Go ingestion exists, so exit `1` is still reported as `error` (ADR 0002
  item 5, pending).

Wired in this slice: `retry_after` and `pcntl` are validated against `--timeout`
through `gohorizon:capabilities --json` before the supervisor spawns anything, and an
incompatible configuration refuses to start (ADR 0002 item 6).
