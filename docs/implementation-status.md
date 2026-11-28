# Implementation status

_Extracted from the repository README. Every statement below was checked against the source, tests and CI at the time of writing; see [the roadmap](ROADMAP.md) for what is planned rather than built._

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
  so the screen and the retry/delete actions name the same records (docs/adr/0013). A database
  store is paged with a keyset query on `failed_at` and its id, one page per request; the file
  failer is read whole but Laravel caps that file at its own configured limit. Laravel's
  `failed_jobs` table has no index in that order, so a large store should add one —
  `(failed_at, id)`, or `(failed_at, uuid)` for the `database-uuids` driver — and the page
  becomes a seek instead of a sort (`docs/adr/0013`). The job payload
  is never returned; `exception` is the redacted first line of the stored trace.
- Capabilities: `GET /api/v1/capabilities` publishes what the application said about itself
  before the workers started — package, Laravel and PHP versions, connections with their
  drivers and `retry_after`, `pcntl`, the telemetry stream and the lifecycle events — together
  with an `operations` map saying which actions this deployment can actually offer. The
  dashboard gates `Retry`, `Delete` and `Restart` on that map, so an application that cannot
  answer offers no action it could not perform and says why (docs/adr/0014). The service re-reads
  the descriptor every 30 s and the dashboard refreshes it with the health poll, so a deployment
  repaired while it runs gets its controls back without a restart or a page reload. The page is
  not the only gate: the retry, delete and restart routes   answer 503 with that same reason when
  the descriptor is unavailable, so a caller that kept a control is refused it by the service.
  The map is derived from what the payload says, not only from whether it parsed: `telemetry`
  follows the reported stream and `inspect_queues` follows the connections' drivers, and every
  withheld entry carries its reason in `operation_reasons`, so a screen the deployment cannot
  fill is withheld with the fact that withheld it rather than as a read-only deployment.
  The state is also on `/api/v1/health` (`capabilities`) and in the scrape as
  `gohorizon_capabilities_available` / `gohorizon_capabilities_failed_reads`, so a read-only
  deployment is alertable without reading the log. The package reports the versions its
  `gohorizon:inspect` and `gohorizon:failed:list` commands emit, and the descriptor publishes them
  (`application_payload_schemas`) beside the versions this service reads (`payload_schemas`). A
  mismatch withholds only the operation that would misread that payload, with both numbers in
  its reason, and the service stops making the read it cannot use: the queue loop skips
  `gohorizon:inspect` (publishing the reason as `queues.error`) and the failed-list endpoint
  answers 503 with that reason, rather than starting PHP to fail decoding on every tick or
  request. The decoder still refuses such a payload at read time — so a package upgrade cannot
  change queue depth or the failed   store silently. A descriptor transition — read-only, readable again, or a package upgrade — is
  written to the audit log as `capability-change`, so "when did this go read-only, and why" is
  answerable from the log rather than only from a warning nobody kept. The transition also
  stamps `read_only_since` in health, and a service that starts against a deployment it cannot
  read stamps it at launch and records `started read-only`, so a restart into the read-only
  state publishes an age instead of waiting for a change that already happened. The descriptor
  publishes the same instant (`read_only_since`), and the dashboard's read-only banner reads it,
  so a withheld control says how long the deployment has been read-only as well as why.
- Read-only `GET /api/v1/health`, and `GET /metrics` for a Prometheus scrape of the same
  state: `gohorizon_up`, `gohorizon_lock_held`, `gohorizon_capabilities_available`,
  `gohorizon_capabilities_failed_reads`, `gohorizon_capabilities_contradiction` (a re-read that
  contradicts the running configuration, such as a `retry_after` lowered under a worker's
  timeout, with the sentence in health),   `gohorizon_capabilities_last_success_timestamp_seconds`
  (when the re-read loop last answered, so a loop that stopped is alertable on its age —
  `last_read_at` in health), `gohorizon_capabilities_read_only_since_timestamp_seconds` (when the
  deployment went read-only, so a state nobody repaired is alertable on its age —
  `read_only_since` in health), telemetry counters by result, queue depth by state, and workers by
  state and crash loops per supervisor. The rules themselves ship with the metrics in
  `deployments/prometheus/gohorizon.rules.yml`: one per capability state, each naming a section of
  `docs/runbook.md` that says what it means and what to do. A test checks every expression against
  the families `/metrics` writes and every alert against its runbook entry, so a renamed sample
  fails the build instead of leaving a rule that quietly never fires. The rules ship with their own
  `promtool test rules` cases (`deployments/prometheus/gohorizon.rules.test.yml`), which pin what
  the guards decide — a loop that has never answered does not fire the stale alert, and the
  read-only age ignores the zero it writes while the deployment is readable — and CI runs both
  `promtool check rules` and `promtool test rules` on them.
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
  slot, so the job in flight finishes instead of being killed. Those three controls are also
  gated on `/api/v1/capabilities`: one the deployment does not advertise is disabled and the
  screen says why (docs/adr/0014). Without `server.auth.users` there are no administrative
  routes to call, so the failed screen and the worker rows stay read-only. The Vite bundle under `web/` is embedded in the binary, hashed assets are
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

Not implemented yet: per-action administrator rights, queryable audit history, balancing.
