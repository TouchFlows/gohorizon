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
2. **The descriptor is a snapshot the service refreshes, never a probe on a read path.**
   The endpoint reads the last answer from memory, so a dashboard load costs no process and
   cannot block on PHP. A re-read is asked for explicitly — by the loop, or by an administrator
   who has just repaired the deployment (see below). `supervisor.New` publishes the preflight's own answer first, and
   `probeLoop` then re-reads it every 30 seconds, so a deployment repaired, upgraded or broken
   while the service runs is presented as it is now without a restart. The refresh only
   publishes: the launch preflight is a decision about workers that are already running, so a
   contradiction found later is reported, never enforced — a queue configuration that changes
   under a live pool still needs the restart it always needed.
3. **The response separates "what the application said" from "what this service can do".**
   `available` is true only when the probe answered with a schema this service understands;
   `error` otherwise says which (the probe's own wording, or the two schema numbers). The
   descriptor fields (`schema`, `package`, `package_version`, `laravel`, `php`, `application`,
   `environment`, `default_connection`, `connections`, `pcntl`, `telemetry_stream`, `events`) are
   published unchanged when it is true and omitted when it is not: a field this service thinks
   it recognises is not meaningful in another schema. `retry_after` is null for a driver with no
   reservation window, never an invented zero.
4. **`operations` is derived from what the descriptor says, and `operation_reasons` says why a
   withheld entry is withheld.** A descriptor that cannot be read is read-only as a whole:
   every entry false, restart included, because a deployment whose descriptor could not be read
   is presented as read-only until it is repaired, so the dashboard never offers an action
   beside an unexplained failure. The supervisor can still restart a process it owns — the
   operator just is not offered that control from a page that cannot say what the deployment
   is. A descriptor that *can* be read is still not a promise that every screen has something
   to show, so two entries follow the facts the payload carries: `telemetry` follows
   `telemetry.stream` (no stream, no events to show), and `inspect_queues` follows both the
   inspect payload version the application reports (decision 7) and the connections' drivers
   (`sync` runs a job inline and `null` drops it, so neither has a queue, a backlog or a depth;
   an unknown driver is assumed inspectable, because the inspect payload omits a count its
   driver cannot answer instead of inventing one). `failed_jobs_list` follows the failed-list
   version the application reports; `failed_job_retry`, `failed_job_delete` and
   `restart_workers` follow `available`, because retry and delete run Laravel's own commands
   rather than that payload, and restarting is the supervisor's own action. Every withheld entry carries its reason in the service's words, so a control that
   disappears says why instead of only disappearing.
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
7. **Both sides of the payload contract are published, and a mismatch withholds only the
   affected operation.** `gohorizon:capabilities` reports the versions its own inspect and
   failed-list commands print, taken from the constants those commands print so the two cannot
   drift apart. The descriptor publishes the versions this service reads in `payload_schemas` —
   they describe this service, so they are published even when the descriptor is unavailable —
   and what the application reported in `application_payload_schemas`, so a client sees two
   numbers rather than only a sentence. When the application reports a version this service does
   not read, only the operation that would misread that payload is withheld (`inspect_queues` or
   `failed_jobs_list`), with both numbers in its reason; everything else keeps working, because
   a package that changed one payload is not a deployment that cannot describe itself. A package
   older than the field reports nothing, which is treated as compatible: refusing a package for
   a claim it never made would read as a deployment fault. The service also stops making the
   read it knows it cannot use: the queue loop does not start `gohorizon:inspect` while the
   reported inspect payload is one this service does not read, and `GET /api/v1/failed` answers
   503 with that reason instead of running the command — a mismatch is not worth a PHP process
   per interval or per request. A descriptor that could not be read is *not* treated this way:
   it says nothing about the other commands, which may still answer (a package whose only
   broken command is `gohorizon:capabilities` still has a readable queue). The decoders still
   refuse a payload declaring another version at read time, so the check holds for a caller that
   never asks the descriptor — and the acceptance tests compare both published numbers with what
   the fixture's own commands print, so a package bump cannot pass unnoticed.

## Consequences

- The screen and the service now agree about what exists: a deployment whose application cannot
  answer offers no failed-job actions and says why, and an operator installing or repairing the
  package sees the same message in the dashboard as in the service log.
- `restart_workers` follows `available` rather than standing alone. The supervisor losing a
  capability of its own would be wrong — it still owns the pool — but the dashboard's reading of
  an unavailable descriptor is "this deployment is read-only, and here is what to repair", which
  is only coherent if every control obeys it. The descriptor, not the screen, still decides,
  which is the seam a later external-worker integration needs.
- A withheld payload is also a read the service skips. `sampleQueues` publishes the reason as
  `queues.error` (and logs it once per change) instead of starting PHP to fail decoding on every
  tick, and `failedJobList` answers 503 with the reason before asking the application. Both
  resume on the next tick or request after a repair, so the skip is the current answer rather
  than a latch; neither changes the report's status, because the pool is still supervised.
- A descriptor transition is recorded in the audit log. When the deployment becomes read-only
  (with the reason), becomes readable again, or the package changes under a readable descriptor,
  the service writes a `capability-change` record with the outcome `changed` and no user: nobody
  acted, and the line exists for the question asked after the fact. A reason that varies while
  the deployment stays read-only stays in the log, so the trail is transitions rather than one
  line per interval. The same transition stamps `read_only_since` in health, and the launch
  probe stamps it when the service starts against a deployment it cannot read, recording
  `started read-only: …` before Redis is even opened. A restart into the read-only state is
  therefore a record with an age rather than only a warning, and "how long has this been
  read-only" is arithmetic on a field instead of a search of the log. The descriptor publishes
  the same instant as `read_only_since`, so the dashboard's banner says how long the controls
  have been withheld from the payload it already gates them on, without a second poll to health
  for one number.
- The refusal is the service's, not only the page's. `requireCapability` derives the same
  operations the descriptor publishes and answers 503 with the withheld entry's own reason (the
  descriptor's `error` when the whole descriptor could not be read) for
  `POST /api/v1/workers/{worker}/restart`,
  `POST /api/v1/failed/{job}/retry` and `DELETE /api/v1/failed/{job}` whenever the descriptor is
  unavailable, records the refusal under that action's audit entry, and runs after the role and
  token checks — so a caller refused for its role is told that, not that the deployment is
  read-only. Nothing is asked of the pool or the application first: the refusal is not an action
  that happened and was then explained.
- A repaired deployment recovers on its own, without a restart. `probeLoop` re-reads the
  descriptor on an interval, so installing the package — or removing it — changes what the
  endpoint publishes within one interval, and the dashboard's refresh draws the controls, or the
  reason, that follow. The re-read is cheap but not free: it starts PHP once per interval per
  application, which is why the interval is slow and why the HTTP read path still starts nothing.
- A repair can be confirmed on demand. `POST /api/v1/capabilities/refresh` re-reads the
  descriptor now and answers with the document a GET would, so whoever has just installed the
  package or raised `retry_after` reads the result instead of waiting out the interval — or
  restarting the service, which would stop the pool to answer a question. It needs the
  administrator role and the token, because it starts a PHP command on the application's host,
  and it is recorded under `capability-refresh` with what the read found (`readable: schema 1,
  package gohorizon/laravel 0.1.0`, or `still read-only: …`), so "did the repair work, and who
  checked" is answerable afterwards. It is deliberately not gated on the descriptor: the
  deployment presented as read-only is exactly the one worth re-reading. The answer is published
  as it was read, without the debounce a scheduled read gets, because the caller is waiting on
  this read and keeping the last answer would tell them nothing — and it is still only
  published, so a contradiction found here is reported and never enforced. Reads are serialized,
  so the loop and the operator cannot publish each other's answer out of order, and a manual
  read counts as the loop's last answer because it is one: it updates
  `gohorizon_capabilities_last_success_timestamp_seconds` when it gets an answer. The dashboard
  draws it as `Re-read now` beside the read-only banner, for the administrator role only,
  disabled while the request is in flight, and says what the read found (`Readable: schema 1,
  package gohorizon/laravel 0.1.0`, `Still read-only: …`, or the service's refusal). That answer
  outlives the banner: a successful read is what takes the banner away, so the notice is mounted
  whether or not one is drawn, and the outcome stays on screen after the controls come back
  instead of unmounting at the moment it became the answer. The banner keeps its own line: the
  button is a sibling, so the sentence the alerting language is matched on is not decorated.
- The state is alertable without the log. `GET /api/v1/health` carries a `capabilities` object
  (`available`, `schema`, `failed_reads`, `error`, `contradiction`, `last_read_at`,
  `read_only_since`) and `/metrics`
  carries `gohorizon_capabilities_available`, `gohorizon_capabilities_failed_reads` and
  `gohorizon_capabilities_contradiction`, all gauges. A fourth sample,
  `gohorizon_capabilities_last_success_timestamp_seconds`, is when the loop last got an answer:
  every other value here describes the last answer, so a re-read loop that stopped would publish
  a healthy descriptor forever, failed streak of zero and all. It is a timestamp rather than an
  age, so the alert is `time() - it > 3 * interval`, the launch read counts as the first answer,
  and an answer this service cannot read still counts as a read — that failure is `available`'s
  to report. A fifth sample,
  `gohorizon_capabilities_read_only_since_timestamp_seconds`, is the instant the deployment went
  read-only: zero while it is readable, stamped by the transition and cleared by the repair, so a
  rule can fire on how long a state has lasted rather than on a fact that was already true. The
  report status is not degraded by either: this service still supervises a pool it cannot
  describe, and a health check that restarted it over that would take healthy workers down
  (docs/adr/0004 item 7). The failed-read streak is published so an alert can fire on a run of
  failures before the debounce presents the deployment as read-only, and the reason is the same
  sentence the endpoint and a refused action publish.
- The rules ship with the metrics. `deployments/prometheus/gohorizon.rules.yml` holds one rule
  per state this section describes — read-only, read-only for more than thirty minutes (from
  `read_only_since`), the contradiction, and a stale loop — and each alert names a section of
  `docs/runbook.md` that says what the state means and what to do. A test renders a scrape and
  checks every metric an expression names against the families the exporter writes, and checks
  every alert against its runbook section: a renamed sample or a retired alert fails the build
  instead of leaving a rule that never fires or an instruction nobody can find. The rules carry
  `promtool test rules` cases of their own, which is where the guards are pinned rather than only
  argued: a loop that never answered (`last_success` 0) must not fire the stale alert, and the
  read-only age must ignore the zero it writes while the deployment is readable. CI runs
  `promtool check rules` and `promtool test rules` on the shipped files, so a syntax error and a
  guard that stopped guarding both fail the build.
- The launch preflight has a second life. Every re-read runs `adapter.Check` again, so an
  application that lowers a `retry_after` under a worker's timeout, or loses pcntl under a
  `--timeout`, is the contradiction the launch would have been refused over — found after the
  launch. It is published in `capabilities.contradiction` and as
  `gohorizon_capabilities_contradiction`, logged once when it appears, changes or clears, and
  never enforced: the workers are already running, and a health read must not stop a pool that is
  doing its job (docs/adr/0004 item 7). A descriptor this service cannot read claims nothing and
  is not a contradiction, so it clears the field rather than inventing one.
- A failed re-read is debounced, because a PHP run that times out once is not a deployment that
  lost its package. One failure keeps the last answer and only records that it could not be
  refreshed; `probeFailureThreshold` (2) consecutive failures publish the read-only state, and
  any read that answers — an unknown schema included — resets the count, so an application that
  fails once in a while never loses its controls. The startup read has nothing to keep, so a
  failure there is still published as read-only immediately, exactly as before.
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
  `POST /api/v1/capabilities/refresh` is covered in `internal/api/capabilities_test.go` (the
  role, the token, the answer it publishes and the record it writes),
  `internal/supervisor/service_test.go` (a forced read publishes its answer, a failure
  included) and `tests/integration`, which repairs the application's answer while the service
  runs and confirms it with one call;   `tests/browser/smoke.spec.ts` repairs a refusing deployment in place — the executable it
  already runs starts answering the probe — and asserts the controls come back on the page that
  was already open, which is the service's re-read plus the dashboard's refresh, with neither a
  restart nor a reload;   `internal/supervisor/service_test.go` covers the re-read, the loop that stops with the service
  and the report's capabilities state;   `internal/api/prometheus_test.go` and
  `internal/api/router_test.go` cover the gauge and the health field, and
  `TestCapabilitiesWithholdWhatThePayloadCannotSupport` / `...WhenNoConnectionHasAQueue` cover
  the derivation and the invariant that every withheld entry has a reason;
  `internal/adapter/payload_schema_test.go` pins the refusal behind each published payload
  schema, and the acceptance contract test compares `payload_schemas` with the schemas the
  fixture's `gohorizon:inspect` and `gohorizon:failed:list` actually print. The second broker
  adapter that would make the descriptor earn its keep is still deferred.
