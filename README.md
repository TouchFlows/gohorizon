# GoHorizon

**Universal queue operations and worker orchestration — built in Go.**

GoHorizon is an independent, open-source control plane for observing, supervising, and
operating background-job infrastructure. It gives teams one operational view across queues,
workers, executions, and failures without replacing the applications or message brokers that
already do the work.

**Starting point:** Laravel 13 + Redis.<br>
**Long-term direction:** a modular platform for multiple brokers and worker runtimes, including
RabbitMQ and ecosystems beyond Laravel.

[![Sponsor](https://img.shields.io/badge/Sponsor-GitHub-ea4aaa?logo=github)](https://github.com/sponsors/marktamis)
[![Buy Me a Coffee](https://img.shields.io/badge/Buy_Me_a_Coffee-Support-FFDD00?logo=buy-me-a-coffee&logoColor=000)](https://buymeacoffee.com/marktamis)

> **Project status:** GoHorizon is under active development. Check
> [implementation status](docs/implementation-status.md) and release notes before using it in
> production. Planned integrations are not current capabilities.

## Why GoHorizon?

Background jobs are essential infrastructure, but their operational tooling is often tied to a
particular framework or broker. GoHorizon separates the operational control plane from the
systems that execute and transport jobs.

- **Observe:** inspect queue depth, workers, job activity, failures, and historical metrics.
- **Supervise:** manage worker processes and their lifecycle.
- **Operate:** investigate failed jobs and perform supported operational actions.
- **Integrate:** connect framework telemetry and broker capabilities through explicit adapters.
- **Extend:** build toward a common operational experience across application runtimes and brokers.

GoHorizon is not a message broker and does not replace Laravel's queue execution model. Laravel
continues to execute jobs, Redis remains the queue transport, and GoHorizon provides the
operational visibility and process supervision around them.

## What it does today

Verified against the current source and tests; [implementation status](docs/implementation-status.md)
carries the detail and the architecture decisions behind each item.

- **Worker supervision:** an explicit state machine, per-queue allocation, crash backoff, exit-code
  classification, graceful SIGTERM with bounded SIGKILL escalation, and adoption of processes left
  by a previous supervisor.
- **Single supervisor guarantee:** a Redis ownership lock with renewal and fencing stops a worker
  pool from ever being supervised by two services.
- **Capabilities preflight:** startup refuses a `worker.timeout` that PHP cannot enforce (no
  `pcntl`) or that is not below the queue connection's `retry_after`.
- **Telemetry ingestion:** a Redis Stream consumer group that deduplicates by event id, validates
  job identity, and retries Redis failures without touching the workers.
- **Historical metrics:** SQLite job rows and per-queue minute, hourly and daily aggregates, pruned
  on configurable retention windows, read back through `GET /api/v1/metrics` and `GET /api/v1/jobs`.
- **Queue depth:** every 15 seconds `gohorizon:inspect` reports each supervised queue's pending,
  reserved and delayed counts, plus the age of its oldest waiting job.
- **Failed jobs:** the application's own failed store, paged through `GET /api/v1/failed`, with
  `Retry` and `Delete` actions that run the application's own commands.
- **Capability descriptor:** `GET /api/v1/capabilities` publishes what the application says about
  itself and an `operations` map; the dashboard and the API both withhold an action this deployment
  cannot perform, with the reason.
- **Health and metrics:** read-only `GET /api/v1/health` and a Prometheus scrape at `/metrics`,
  shipping with alerting rules and `promtool` tests.
- **Dashboard:** a React dashboard at `/` with overview, queues, workers, jobs, failed jobs and
  metrics screens.
- **Laravel package:** `gohorizon/laravel` with configuration, lifecycle telemetry,
  `gohorizon:capabilities`, `gohorizon:inspect` and `gohorizon:failed:list`.
- **Sign-in and roles:** session authentication with PBKDF2 hashes and `gohorizon passwd`, roles
  (`viewer`, `operator`, `administrator`), CSRF protection, and an audit record for every session
  and administrative action.
- **Tests:** integration tests against a real Laravel application and Redis, and a Playwright
  browser acceptance test.

Not implemented yet: per-action administrator rights, queryable audit history, balancing.

## Architecture at a glance

```text
Laravel application
       |
       | queue execution + integration telemetry
       v
Redis queue infrastructure <----> GoHorizon Go control plane
                                      |
                                      +-- worker supervision
                                      +-- metrics and health APIs
                                      +-- historical persistence
                                      +-- React dashboard
```

The supported deployment is a single GoHorizon deployment. These concerns stay separate:

- **Broker adapters** — broker-specific inspection and operations.
- **Runtime/framework adapters** — framework-specific metadata and worker integration.
- **Control plane** — lifecycle, coordination, API, persistence, and authorization.
- **Dashboard** — a web interface over the documented APIs.

Broker semantics differ. GoHorizon exposes capabilities explicitly instead of pretending every
broker supports identical operations or guarantees.

## Supported integrations and roadmap

| Integration | Status |
|---|---|
| Laravel 13 | Initial integration target |
| Redis | Initial broker/backend target |
| RabbitMQ | Planned; not supported until an adapter and its tests exist |
| Other worker runtimes | Long-term direction; not currently supported |

GoHorizon does not claim exactly-once processing. Delivery, acknowledgement, retry, ordering and
visibility semantics remain broker-specific.

[docs/ROADMAP.md](docs/ROADMAP.md) describes the intended sequence — hardening, first stable
release, the adapter contract, then RabbitMQ and further runtimes. It describes direction, not a
promise or a delivery schedule.

## Quick start

Requires Go, PHP with `pcntl` for worker supervision, and a Redis instance.

```sh
go build ./...
go test -race ./...

GOHORIZON_REDIS_PASSWORD=secret go run ./cmd/gohorizon validate -config examples/gohorizon.yaml
GOHORIZON_REDIS_PASSWORD=secret go run ./cmd/gohorizon run -config examples/gohorizon.yaml
curl -s localhost:8090/api/v1/health | jq
```

`examples/gohorizon.yaml` points at `/var/www/application`; adjust the application path and PHP
binary before running the service. The specification lives in [`handoff.md`](handoff.md); the
verified Laravel 13 behaviour that adjusts it is in
[`docs/laravel-13-recon.md`](docs/laravel-13-recon.md).

Configuration is loaded from YAML with environment expansion and validated by
`gohorizon validate`. Authentication is off until `server.auth.users` is configured; pass hashes
are minted with `gohorizon passwd`. Operational guidance and alert meanings are in
[`docs/runbook.md`](docs/runbook.md).

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

The HTTP API is documented in [docs/api.md](docs/api.md).

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

## Design principles

1. **Do not replace the broker.** GoHorizon operates around existing queue infrastructure.
2. **Preserve native semantics.** Adapters expose real capabilities and limitations.
3. **Keep integrations modular.** Framework integration and broker integration are separate concerns.
4. **Prefer observable, recoverable operations.** Lifecycle transitions and administrative actions
   are auditable.
5. **Be honest about guarantees.** No exactly-once promises, no uniform behaviour across brokers.
6. **Ship incrementally.** Protect the working Laravel/Redis path while adding capabilities.
7. **Secure by default.** Authentication, authorization, CSRF protection, secret handling and
   auditability are core product concerns.

## Sponsorship

GoHorizon is an independent open-source project. Sponsorship helps fund the time and
infrastructure required to make it reliable, useful, and sustainable.

Your support helps with:

- Core GoHorizon development and maintenance.
- Compatibility testing across supported Laravel, PHP, and Redis versions.
- Broker adapters, beginning with the planned RabbitMQ integration.
- Worker-runtime integrations and adapter documentation.
- Security, reliability, and observability improvements.
- Documentation, examples, releases, and community support.

- [Sponsor on GitHub](https://github.com/sponsors/marktamis)
- [Buy Me a Coffee](https://buymeacoffee.com/marktamis)

Sponsorship supports ongoing open-source development. It does not purchase a commercial SLA,
guaranteed response times, or guaranteed feature delivery.

## Contributing

Contributions, issue reports, documentation improvements, and thoughtful feature proposals are
welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. For
security-sensitive reports, follow [SECURITY.md](SECURITY.md) rather than publishing exploit
details in a public issue.

## License

The repository does not contain a `LICENSE` file yet, and the maintainer has not selected a
license. Until a license is added, do not assume that public visibility grants permission to use,
modify, or redistribute the code. [LICENSE-DECISION.md](LICENSE-DECISION.md) records the decision
that has to be made and the options considered.
