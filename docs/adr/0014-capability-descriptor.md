# ADR 0014 — Publishing the application's capabilities, and gating on them

**Status:** accepted (universal-platform slice 1, continuing docs/adr/0001 and
docs/adr/0011)
**Implements:** handoff §5.3 (capability negotiation) and §11 ("the UI must use capability
information from the API to hide or disable unsupported operations").

## Context

GoHorizon already asks the application what it supports: `internal/adapter` runs
`php artisan gohorizon:capabilities --json` before any worker starts, and refuses a launch the
payload contradicts (a `--timeout` without pcntl, or one at or above the connection's
`retry_after`). That answer is a subset of what the package reports — the connections, the
runtime, the telemetry stream and the lifecycle events — and until now it lived only inside the
startup preflight.

Meanwhile the dashboard decides what it may offer from the session alone: a deployment with
users draws `Restart`, `Retry` and `Delete`, and a deployment without users draws none. That was
correct while Laravel/Redis was the only integration. It stops being correct the moment a
deployment's application does not answer the probe, or reports a schema this service does not
know: the screen would offer actions whose commands cannot run, and the only thing between the
operator and a refusal would be the 503 the action returns.

## Decision

1. **`GET /api/v1/capabilities` publishes the descriptor read at startup.** It is a read in the
   session-gated group, so any signed-in role may ask, and it is read-only: a POST is a 405. It
   answers `Cache-Control: no-store`, like every other operational read.
2. **The descriptor is a snapshot, not a live probe.** The values are the ones `supervisor.New`
   read and the preflight acted on, held on the service; nothing on the HTTP path starts PHP.
   The endpoint and the launch decision therefore cannot disagree, and a dashboard load costs no
   process. A deployment whose application changes its queue configuration restarts the service —
   which it already has to do, because the preflight is a startup check.
3. **The response separates "what the application said" from "what this service can do".**
   `available` is true only when the probe answered with a schema this service understands;
   `error` otherwise says which (the probe's own wording, or the two schema numbers). The
   descriptor fields (`schema`, `package`, `package_version`, `laravel`, `php`, `application`,
   `environment`, `default_connection`, `connections`, `pcntl`, `telemetry_stream`, `events`) are
   published unchanged when it is true and omitted when it is not: a field this service thinks
   it recognises is not meaningful in another schema. `retry_after` is null for a driver with no
   reservation window, never an invented zero.
4. **`operations` is a map of booleans, and each entry says what this service offers against
   this deployment.** `inspect_queues`, `telemetry`, `failed_jobs_list`, `failed_job_retry` and
   `failed_job_delete` all run the package's own artisan commands, so they follow `available`.
   `restart_workers` is the supervisor's own action, but it follows `available` as well: a
   deployment whose descriptor could not be read is presented as read-only until it is
   repaired, so the dashboard never offers an action beside an unexplained failure. The
   supervisor can still restart a process it owns — the operator just is not offered that
   control from a page that cannot say what the deployment is.
5. **An unavailable or unreadable adapter is a 200, not a 503.** "This deployment cannot do
   that" is the answer the caller asked for; a 503 would describe a failed read and invite a
   retry of a fact that does not change until the service restarts.
6. **The dashboard gates its controls on the descriptor.** The failed screen offers `Retry` and
   `Delete` per `failed_job_retry` / `failed_job_delete` and the worker rows offer `Restart` per
   `restart_workers`; a control that is not advertised is disabled (or, when neither failed-job
   action is offered, replaced by the reason), and the reason is the descriptor's `error` in the
   service's own words, so a missing control explains itself instead of only disappearing. The
   session still decides first: without users there are no administrative routes at all
   (docs/adr/0011). A descriptor read that has not answered — still in flight, or a transient
   failure — leaves the controls as the session alone decides, because a blip must not hide a
   control the service still offers, and the service refuses what it cannot do. The dashboard
   refreshes the descriptor on the health poll's interval instead of reading it once at mount,
   so a deployment repaired and restarted gets its controls back without a page reload; a
   refresh that fails keeps the last descriptor, since falling back would put back a control
   the service would refuse.

## Consequences

- The screen and the service now agree about what exists: a deployment whose application cannot
  answer offers no failed-job actions and says why, and an operator installing or repairing the
  package sees the same message in the dashboard as in the service log.
- `restart_workers` follows `available` rather than standing alone. The supervisor losing a
  capability of its own would be wrong — it still owns the pool — but the dashboard's reading of
  an unavailable descriptor is "this deployment is read-only, and here is what to repair", which
  is only coherent if every control obeys it. The descriptor, not the screen, still decides,
  which is the seam a later external-worker integration needs.
- The refusal is the service's, not only the page's. `requireCapability` answers 503 with the
  descriptor's own `error` for `POST /api/v1/workers/{worker}/restart`,
  `POST /api/v1/failed/{job}/retry` and `DELETE /api/v1/failed/{job}` whenever the descriptor is
  unavailable, records the refusal under that action's audit entry, and runs after the role and
  token checks — so a caller refused for its role is told that, not that the deployment is
  read-only. Nothing is asked of the pool or the application first: the refusal is not an action
  that happened and was then explained.
- A repaired deployment recovers on its own. Because the descriptor is a startup snapshot
  (decision 2), the only way it changes is a restarted service — which is exactly what an
  operator does after installing the package. The dashboard's refresh then picks the new answer
  up within one poll, and the read-only banner and the withheld controls leave with it.
- The endpoint publishes what the application reports about itself, including its Laravel and
  PHP versions and event list. It does not publish payloads, credentials or keys; the package
  already omits `has_password` values, and the probe ignores them.
- Broken payloads stay distinguishable from unknown schemas: one fills `error` with the artisan
  output, the other with the schema numbers, so an operator can tell a missing package from an
  older one.
- An unknown schema degrades startup instead of stopping it. `supervisor.New` skips the launch
  preflight with a warning, exactly as it does when the command cannot answer (docs/adr/0001,
  docs/adr/0002), and the endpoint publishes `available: false` with both versions; the
  deployment keeps supervising its pool and loses only its controls, so the operator sees the
  reason on the page rather than a service that never came up. Nothing is read from the
  payload, which is the refusal the schema number exists for.
- `internal/adapter` now keeps the identifiers and events it only ignored before, and the
  package's `events` list replaces the illustrative `operations` array in the fixture: the
  operations map is this service's derivation, not a field the package sends.
- Tests: `internal/api/capabilities_test.go` covers the published descriptor, the unavailable
    adapter, the unknown schema, the session/read-only rules and the refusal of all three actions
  when the descriptor is unreadable (the same role, token and report succeed when it is not);
  `web/src/capabilities.test.ts` covers the gating, including the transient-failure and
  per-action cases, and `web/src/useCapabilities.test.ts` the refresh — the interval, a
  descriptor that becomes readable, a failed refresh keeping the last answer, and the 401 that
  ends the session; `tests/integration`
  drives the real binary through the endpoint against the application's own payload, against a
  PHP that refuses the probe, and against one that answers an unknown schema — where a signed-in
  operator's retry, delete and restart are refused and the worker is still the one running.
  `tests/browser/smoke.spec.ts` restarts a refusing deployment on its own port with a PHP that
  answers, and asserts the controls come back on the page that was already open, which is the
  refresh and not a reload. The second broker
  adapter that would make the descriptor earn its keep is still deferred.
