# gohorizon

GoHorizon is an independent Go control plane for existing Laravel queue infrastructure.
Laravel keeps executing jobs, Redis keeps transporting them, and GoHorizon supervises
processes, observes lifecycle events and reports operational state.

The specification lives in [`handoff.md`](handoff.md); the verified Laravel 13 behaviour
that adjusts it is in [`docs/laravel-13-recon.md`](docs/laravel-13-recon.md).

## Status

Milestone 1 (proof of concept) in progress. Implemented:

- Configuration loading, environment expansion and full validation (`gohorizon validate`).
- Redis namespace split with a typed client; the ownership lock with renewal and
  fencing: an instance whose lock is definitively gone or taken over, or whose renewals
  have gone unconfirmed for longer than the lock's TTL, stops its workers gracefully (an
  in-flight job still finishes) and exits non-zero, so the pool is never supervised by two
  services.
- Capabilities preflight: startup refuses a `worker.timeout` that PHP cannot enforce
  (no `pcntl`) or that is not below the queue connection's `retry_after`.
- Process supervisor: explicit state machine, per-queue fixed allocation, crash backoff,
  graceful SIGTERM with bounded SIGKILL escalation, adoption of processes left by a
  previous supervisor, exit-code classification (0/12 recycle vs crash).
- Telemetry ingestion: a consumer group on the events stream that deduplicates by
  event id, acknowledges what it handles and retries Redis failures without touching
  the workers. Its counters and live failure state are on `GET /api/v1/health`.
- Historical metrics: every consumed `job.finished` event is aggregated into per-queue
  minute buckets in SQLite at `storage.path`, deduplicated by event id, so history
  survives a restart. Each pass (startup and hourly) folds closed minutes into hourly
  buckets and closed hours into daily ones before pruning event ids, minutes, hours and
  days past their windows. The windows follow handoff §9 and are configurable under
  `storage.retention` (defaults: raw 72 h, minutes 7 d, hours 30 d, days 365 d).
  `GET /api/v1/metrics?resolution=…` reads any of the three levels back.
- Queue depth: every 15 seconds the service runs `gohorizon:inspect` for the queues
  its supervisors launch and reports each queue's `pending`, `reserved` and `delayed`
  counts on `GET /api/v1/health`. Counts a driver cannot report are `null`, and a
  missing package or failing command only fills in `queues.error` — the status stays
  `ok`, so a health check cannot restart healthy workers over a failed inspection.
- Read-only `GET /api/v1/health`.
- `gohorizon/laravel` package: configuration, job lifecycle telemetry to a Redis Stream,
  `gohorizon:capabilities`, `gohorizon:inspect`.

`GET /api/v1/metrics?queue=default[&resolution=minute|hourly|daily][&from=…&to=…]`
returns one queue's persisted buckets as `{queue, resolution, from, to, buckets: [{start,
processed, released, failed, timed_out, duration_ms}]}`, oldest first.
`resolution` defaults to `minute`; the range is `[from, to)` in RFC 3339 and defaults to the last
hour, 24 hours or 30 days respectively. A missing
queue, an unknown resolution or a malformed range is a 400, unavailable history is a
503, and a store failure is a 500 that keeps the database path out of the response.

Not implemented yet: the React dashboard, authentication, balancing, failed-job
administration, Prometheus exposition.

## Layout

```
cmd/gohorizon          CLI and service entrypoint
internal/config        YAML loading, env expansion, validation
internal/redis         key ownership and the typed Redis client
internal/process       worker state machine, launch specs, process manager
internal/adapter       gohorizon:capabilities probe, queue inspection, launch preflight
internal/telemetry     events stream consumer, dedupe and counters
internal/metrics       SQLite per-queue aggregates (historical metrics)
internal/supervisor    lock + managers + health report
internal/api           HTTP routes
packages/laravel       Composer package (gohorizon/laravel)
docs/adr               architecture decision records
```

## Running locally

```sh
go build ./...
go test -race ./...

GOHORIZON_REDIS_PASSWORD=secret go run ./cmd/gohorizon validate -config examples/gohorizon.yaml
GOHORIZON_REDIS_PASSWORD=secret go run ./cmd/gohorizon run -config examples/gohorizon.yaml
curl -s localhost:8090/api/v1/health | jq
```

`examples/gohorizon.yaml` points at `/var/www/application`; adjust the application path
and PHP binary before running the service.

## Acceptance run

`tests/integration` drives a real service against a disposable Laravel application and a
real Redis: a scheduled worker runs jobs end to end, telemetry reaches `/api/v1/health`
and `/api/v1/metrics`, a job in flight finishes exactly once and is recorded before a
graceful stop completes, a hard-killed worker's job is retried by Laravel, a killed
worker is replaced, a service restart adopts the running worker instead of launching
a second one, an instance whose lock was taken over releases its worker instead of
sharing the pool, jobs keep running while the control plane's Redis is cut while health
reports 503 and recovers without restarting the worker, an instance whose Redis stays
silent past the lock's TTL stops supervising instead of holding an expired lock, and the
`released` and `timed_out` outcomes of a real job reach the persisted history. The tests are skipped
unless `GOHORIZON_E2E=1`, so `go test ./...` stays fast and needs no PHP.

The fixture is `tests/fixtures/laravel-app`: a Laravel 13 app with the package installed
by path. Its `composer.lock` is tracked so everyone runs the same dependency set, and
`vendor/` is ignored, so install it once. The suite
expects Redis at `127.0.0.1:6379`, works in database 15 and deletes only the keys it
created. The outage test puts a loopback TCP proxy in front of that Redis and pauses the
proxy, so it never stops the shared server; the fixture's Laravel workers keep their own
direct connection, which is what makes the outage GoHorizon's alone:

```sh
cd tests/fixtures/laravel-app && composer install && cd ../../..
GOHORIZON_E2E=1 go test -count=1 -v ./tests/integration
```

## Safety rules

These are enforced by tests, not just documented:

- Go never reads or writes a Laravel queue key, and has no Redis API that could.
- A worker is only restarted once its process is confirmed gone; a stale pid is never
  signalled and never adopted on pid alone (kernel start time and `--name` must match).
- One worker serves one queue, so queue-level allocation is real.
- A graceful stop waits `shutdown_grace`, then escalates to SIGKILL on the process group
  and records the escalation.
