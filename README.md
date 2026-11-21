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
- Process supervisor: explicit state machine, per-queue fixed allocation (`workers.min`
  is the pool size and validation requires `workers.max` to match it until balancing
  arrives; a pool shorter than its queue list logs one startup warning naming the
  queues that get no worker), crash backoff,
  graceful SIGTERM with bounded SIGKILL escalation, adoption of processes left by a
  previous supervisor, exit-code classification (0/12 recycle vs crash). A worker that
  keeps crashing before it can do any work is flagged `crash_looping` in the health body,
  with one escalation log per streak; the status stays `ok`, because the pool is already
  replacing it and a flapping health check would only restart the supervisor.
- Telemetry ingestion: a consumer group on the events stream that deduplicates by
  event id, validates the job identity each record carries (id, or the uuid when Laravel
  assigned one, plus its class, attempts and an exception's class when there is one),
  acknowledges what it handles and retries Redis failures without touching the workers. Its counters and live failure state are on `GET /api/v1/health`.
- Historical metrics: every consumed `job.finished` event is stored as one `jobs` row
  (identity, class, queue, attempts, duration and exception) and aggregated into per-queue
  minute buckets in SQLite at `storage.path`, deduplicated by event id, so history
  survives a restart. Job rows share the raw retention window and are pruned with it.
  Each pass (startup and hourly) folds closed minutes into hourly buckets and closed
  hours into daily ones before pruning event ids, job rows, minutes, hours and days past
  their windows. The windows follow handoff §9 and are configurable under
  `storage.retention` (defaults: raw 72 h, minutes 7 d, hours 30 d, days 365 d).
  `GET /api/v1/metrics?resolution=…` reads any of the three levels back, and
  `GET /api/v1/jobs` / `GET /api/v1/jobs/{id}` read the attempts themselves.
- Queue depth: every 15 seconds the service runs `gohorizon:inspect` for the queues
  its supervisors launch and reports each queue's `pending`, `reserved` and `delayed`
  counts on `GET /api/v1/health`, plus `oldest_pending_at` (the unix time of the oldest
  waiting job, so a stalled queue is visible even when its depth looks small) and
  `oldest_pending_error` when the driver could not answer that. Counts a driver cannot
  report are `null`, and a missing package or failing command only fills in
  `queues.error` — the status stays `ok`, so a health check cannot restart healthy
  workers over a failed inspection.
- Read-only `GET /api/v1/health`, and `GET /metrics` for a Prometheus scrape of the same
  state: `gohorizon_up`, `gohorizon_lock_held`, telemetry counters by result, queue depth
  by state, and workers by state and crash loops per supervisor.
- Dashboard served at `/`: overview, queues, workers and metrics screens. The first three
  read `/api/v1/health` every 5 s, where a 503 is shown as a degraded service rather than an
  error page and a count the driver could not report reads `unknown`, not `0`; metrics charts
  the service's own minute, hourly and daily aggregates from `/api/v1/metrics`, drawn as
  inline SVG with no chart dependency and a window with no rows shown as "no data in this
  window". The Vite bundle under `web/` is embedded in the binary, hashed assets are
  cached forever, `index.html` is not cached, and a client route falls back to it
  (docs/adr/0008).
- `gohorizon/laravel` package: configuration, job lifecycle telemetry to a Redis Stream,
  `gohorizon:capabilities`, `gohorizon:inspect`.

`GET /api/v1/metrics?queue=default[&resolution=minute|hourly|daily][&from=…&to=…]`
returns one queue's persisted buckets as `{queue, resolution, from, to, buckets: [{start,
processed, released, failed, timed_out, duration_ms}]}`, oldest first.
`resolution` defaults to `minute`; the range is `[from, to)` in RFC 3339 and defaults to the last
hour, 24 hours or 30 days respectively. A missing
queue, an unknown resolution or a malformed range is a 400, unavailable history is a
503, and a store failure is a 500 that keeps the database path out of the response.

`GET /api/v1/jobs?[queue=…][outcome=…][limit=…][before=…]` returns one page of finished
attempts, newest first, as `{jobs: [{id, job_id, uuid, class, queue, connection, outcome,
attempts, duration_ms, finished_at, exception: {class, message} | null}], next_before}`.
`limit` defaults to 50 and may not exceed 200, `outcome` must be one of `processed`,
`released`, `failed` or `timed_out`, and `before` is the `next_before` of the previous page —
empty on the last page, so paging never needs a count. `GET /api/v1/jobs/{id}` returns one
job's newest attempt by identity: the uuid when Laravel published one, otherwise `id:` plus
the queue job id (the same `id:` fallback the recorder uses). A malformed query is a 400, an
unknown identity a 404, unavailable history a 503, and a store failure a 500 without the
database path.

`GET /metrics` serves the state `/api/v1/health` reports in the Prometheus text format
(`text/plain; version=0.0.4; charset=utf-8`), without a client library:
`gohorizon_up`, `gohorizon_lock_held`,
`gohorizon_telemetry_events_total{result="event|duplicate|invalid|error"}`,
`gohorizon_queue_jobs{queue,state="pending|reserved|delayed"}`,
`gohorizon_workers{supervisor,state}` and `gohorizon_crash_looping_workers{supervisor}`.
The `error` result counts Redis failures that were retried, not failed jobs. A count the
driver could not report has no sample rather than a zero, and label values are escaped as
the format requires. Both `/metrics` and `/api/v1/health` are unauthenticated
like the rest of the API today: keep them behind the reverse proxy until Milestone 3 adds
authentication.

Not implemented yet: the dashboard's screens, authentication, balancing, failed-job
administration.

## Layout

```
cmd/gohorizon          CLI and service entrypoint
internal/config        YAML loading, env expansion, validation
internal/redis         key ownership and the typed Redis client
internal/process       worker state machine, launch specs, process manager
internal/adapter       gohorizon:capabilities probe, queue inspection, launch preflight
internal/telemetry     events stream consumer, dedupe and counters
internal/metrics       SQLite job rows and per-queue aggregates (historical metrics)
internal/supervisor    lock + managers + health report
internal/api           HTTP routes and the embedded dashboard
web                    Vite + React dashboard, built into web/dist
packages/laravel       Composer package (gohorizon/laravel)
docs/adr               architecture decision records
```

## Dashboard

The dashboard lives in `web/` (React, TypeScript, Vite) and the service serves it at `/`.
Its built bundle is committed under `web/dist` and embedded with `go:embed`, so `go build`
needs no Node and a release serves the bytes a reviewer saw (docs/adr/0008).

Change the sources, then rebuild and commit the output with them:

```
cd web
npm ci
npm run build
```

CI runs the same build and fails when `web/dist` does not match the sources. While working
on the UI, the Vite dev server is quicker:

```
cd web
npm run dev
```

The dev server proxies `/api` to `http://127.0.0.1:8090` — the address in
`examples/gohorizon.yaml` — so components call the same paths in development and in the
binary; set `GOHORIZON_API` to point it at a service somewhere else. `npm test` runs the
vitest suite over the pure functions: report to screen, age formatting, unreported counts
staying `unknown`, the bucket-to-column transform (including gaps and an all-zero window),
and the history query the range picker builds.

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
real Redis: a scheduled worker runs jobs end to end, telemetry reaches `/api/v1/health`,
`/api/v1/metrics` and `/api/v1/jobs`, a job in flight finishes exactly once and is
recorded before a graceful stop completes, a hard-killed worker's job is retried by
Laravel, a killed worker is replaced, a service restart adopts the running worker instead
of launching a second one, an instance whose lock was taken over releases its worker
instead of sharing the pool, jobs keep running while the control plane's Redis is cut
while health reports 503 and recovers without restarting the worker, an instance whose
Redis stays silent past the lock's TTL stops supervising instead of holding an expired
lock, a supervisor over two queues runs one worker per queue with each queue's job
recorded in its own history, and the `released` and `timed_out` outcomes of a real job
reach the persisted history. The tests are skipped unless `GOHORIZON_E2E=1`, so
`go test ./...` stays fast and needs no PHP.

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
