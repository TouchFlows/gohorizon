# GoHorizon

Laravel queue supervision, powered by Go.

GoHorizon is an independent, lightweight control plane for existing Laravel queue infrastructure. It supervises Laravel queue workers, monitors Redis-backed queues, collects job lifecycle telemetry, and provides historical metrics through a modern web dashboard.

Laravel continues to execute jobs. Redis remains the queue transport. GoHorizon provides the operational visibility and process supervision around them.

[![Sponsor](https://img.shields.io/badge/Sponsor-GitHub-ea4aaa?logo=github)](https://github.com/sponsors/marktamis)
[![Buy Me a Coffee](https://img.shields.io/badge/Buy_Me_a_Coffee-Support-FFDD00?logo=buy-me-a-coffee&logoColor=000)](https://buymeacoffee.com/marktamis)

The specification lives in [`handoff.md`](handoff.md); the verified Laravel 13 behaviour
that adjusts it is in [`docs/laravel-13-recon.md`](docs/laravel-13-recon.md).

## Status

Milestones 1 and 2 implemented:

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
- Failed jobs: `php artisan gohorizon:failed:list --json` reads the application's own
  failed store through its configured failer, and `GET /api/v1/failed` pages it newest first,
  so the screen and the retry/delete actions name the same records (docs/adr/0013). The job
  payload is never returned; `exception` is the redacted first line of the stored trace.
- Read-only `GET /api/v1/health`, and `GET /metrics` for a Prometheus scrape of the same
  state: `gohorizon_up`, `gohorizon_lock_held`, telemetry counters by result, queue depth
  by state, and workers by state and crash loops per supervisor.
- Dashboard served at `/`: overview, queues, workers, jobs, failed jobs and metrics screens.
  The first three read `/api/v1/health` every 5 s, where a 503 is shown as a degraded service
  rather than an error page and a count the driver could not report reads `unknown`, not `0`;
  metrics charts the service's own minute, hourly and daily aggregates from
  `/api/v1/metrics`, drawn as inline SVG with no chart dependency and a window with no rows
  shown as "no data in this window", with the average duration of an attempt drawn under
  the outcome columns from the same bucket; jobs reads `/api/v1/jobs` with queue and outcome filters,
  cursor paging and a detail panel, and failed jobs reads the application's own store from
  `/api/v1/failed` with the same cursor paging, the redacted exception in the row itself and
  `Retry` and `Delete` in the record's detail panel. The two actions go through the failed-job
  endpoints below, echoing the CSRF token the page reads from its own cookie; a worker row's
  `Restart` posts the restart endpoint below and the supervisor recycles the process in its
  slot, so the job in flight finishes instead of being killed. Without `server.auth.users`
  there are no administrative routes to call, so the failed screen and the worker rows stay
  read-only. The Vite bundle under `web/` is embedded in the binary, hashed assets are
  cached forever, `index.html` is not cached, and a client route falls back to it
  (docs/adr/0008).
- `gohorizon/laravel` package: configuration, job lifecycle telemetry to a Redis Stream,
  `gohorizon:capabilities`, `gohorizon:inspect`, `gohorizon:failed:list`.
- Sign-in (Milestone 3, docs/adr/0009): `server.auth.users` turns authentication on, with
  PBKDF2 password hashes minted by `gohorizon passwd` and a session cookie signed with the
  key named by `server.auth.session_key_env`. `/api/v1/*` then requires a session (the
  session routes and the Prometheus scrape do not), sign-out revokes the token, a session
  naming a user the configuration no longer has is refused, and five failed sign-ins lock
  an address for a minute. With the proxy named in `server.trusted_proxies`, the cookie is
  `Secure` when its `X-Forwarded-Proto` says the browser is on HTTPS and the limiter counts
  the client it forwards for rather than the proxy itself; one list decides both, so a
  forwarded header is never trusted more than its sender. The dashboard opens on a sign-in
  screen and offers a sign-out; without users it stays open, as before. A user carries a
  role (`viewer`, `operator`, `administrator`; the default is `viewer`) which is read from
  the configuration on every request, so changing it takes effect at the operator's next
  request rather than when their session expires, and every session action — sign-in
  allowed, refused or limited, and sign-out — is written to the service log as a
  `msg=audit` record naming the actor, their role and the client address
  (docs/adr/0010). A role also decides what may be done: restarting a worker needs `operator`
  or above, and every administrative call needs the CSRF token (docs/adr/0011).

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

`POST /api/v1/workers/{worker}/restart` asks one worker to stop gracefully, so its supervisor
starts a replacement in the same slot and the job in flight finishes instead of being killed;
the answer is 202 with `{worker, supervisor}`. The id is one `GET /api/v1/health` reports — the
API never takes a command, a path or a PID. It needs a session with the `operator` role or
above, and the `X-CSRF-Token` header repeating the value of the `gohorizon_csrf` cookie the
service sets at sign-in (readable by the bundle, `SameSite`, not `HttpOnly`). A worker nobody
runs is a 404 before anything reaches the pool, one that exits in between is a 409, and either
failed check is a 403 with an audit record saying what was missing. Without
`server.auth.users` the route is not mounted at all (docs/adr/0011).

`POST /api/v1/failed/{job}/retry` pushes one failed job back onto its queue under the same
session, role and token, and `DELETE /api/v1/failed/{job}` deletes it. The retry answers 202 with
`{job, queue}` because the work is queued; the delete answers 200 with the same body because the
record is already gone. Both are the application's own commands (`php artisan queue:retry` and
`queue:forget`), so GoHorizon neither writes a queue key nor re-publishes a payload itself
(docs/adr/0012). A job no attempt history knows is a 404, an attempt whose newest outcome is not
`failed` is a 409, and a failed job the application no longer stores is a 404.

`GET /api/v1/failed?[limit=…][before=…]` lists the application's own failed store as
`{failed_jobs: [{id, uuid, connection, queue, job, failed_at, exception}], next_before}`, newest
first, so the list names the records the two actions above act on instead of GoHorizon's pruned
attempt history (docs/adr/0013). The command behind it is
`php artisan gohorizon:failed:list --json`, which reads the configured failer; the payload is
never returned and `exception` is the redacted first line of the stored trace. `limit` defaults
to 50 and may not exceed 200, `before` is the `next_before` of the previous page and is empty on
the last page, and either being malformed is a 400. An application that cannot answer is a 503,
an empty store is `[]`, and any signed-in role may read it.

`GET /metrics` serves the state `/api/v1/health` reports in the Prometheus text format
(`text/plain; version=0.0.4; charset=utf-8`), without a client library:
`gohorizon_up`, `gohorizon_lock_held`,
`gohorizon_telemetry_events_total{result="event|duplicate|invalid|error"}`,
`gohorizon_queue_jobs{queue,state="pending|reserved|delayed"}`,
`gohorizon_workers{supervisor,state}` and `gohorizon_crash_looping_workers{supervisor}`.
The `error` result counts Redis failures that were retried, not failed jobs. A count the
driver could not report has no sample rather than a zero, and label values are escaped as
the format requires. `/metrics` stays unauthenticated, because Prometheus scrapes it without
a session; keep it behind the reverse proxy. `/api/v1/health` is behind the gate like the
rest of the API once `server.auth.users` is configured.

Not implemented yet: per-action administrator rights, queryable audit history, balancing.

## Layout

```
cmd/gohorizon          CLI and service entrypoint
internal/config        YAML loading, env expansion, validation
internal/redis         key ownership and the typed Redis client
internal/process       worker state machine, launch specs, process manager
internal/adapter       gohorizon:capabilities probe, queue inspection, failed-job listing, launch preflight
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
the history query the range picker builds, and the jobs screen's query, cursor trail and the
`null` exception that must read `none` rather than blank.

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

One browser test follows them: `tests/browser` (Playwright) starts the compiled binary
against the same fixture and a real store, dispatches a job that succeeds and one that throws,
and asserts through Chromium that the overview shows the service's status and the queue it
supervises, that the jobs page shows the attempt the worker ran, that the failed page shows the
failed attempt with the exception it threw, that the metrics page charts both attempts in the
minutes they finished in and reports an empty daily window as empty, and that a deep link
outside the navigation returns the shell instead of a 404. It is skipped unless `GOHORIZON_E2E=1` too, and CI fails when it is:

```sh
cd tests/browser
npm ci
npx playwright install chromium
GOHORIZON_E2E=1 npm test
```

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
  
## Support this project

If you find this project useful, consider supporting its development.

[![Sponsor](https://img.shields.io/badge/Sponsor-GitHub-ea4aaa?logo=github)](https://github.com/sponsors/marktamis)
[![Buy Me a Coffee](https://img.shields.io/badge/Buy_Me_a_Coffee-Support-FFDD00?logo=buy-me-a-coffee&logoColor=000)](https://buymeacoffee.com/marktamis)
