# API reference

_The routes the service serves today, with their requests, responses and status codes. Authentication, roles and CSRF are described in [implementation status](implementation-status.md) and architecture decisions 0009–0014._

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
(docs/adr/0012). The application's store decides whether the record exists: a failed job it no
longer stores is a 404. Attempt history is read only to name the queue in the answer and the
audit record, never to refuse, so a failed record older than the history window is still
actionable and a deployment without history still retries and deletes.

`GET /api/v1/failed?[limit=…][before=…]` lists the application's own failed store as
`{failed_jobs: [{id, uuid, connection, queue, job, failed_at, exception}], next_before}`, newest
first, so the list names the records the two actions above act on instead of GoHorizon's pruned
attempt history (docs/adr/0013). The command behind it is
`php artisan gohorizon:failed:list --json`, which reads the configured failer; the payload is
never returned and `exception` is the redacted first line of the stored trace. `limit` defaults
to 50 and may not exceed 200, `before` is the `next_before` of the previous page and is empty on
the last page, and either being malformed is a 400. An application that cannot answer is a 503,
an empty store is `[]`, and any signed-in role may read it. Paging a database store is a keyset
query, so it stays one page per request; give the store an index on `(failed_at, id)` (or
`(failed_at, uuid)` for `database-uuids`) and the database seeks the page instead of sorting
the table (`docs/adr/0013`, decision 4).

`GET /api/v1/capabilities` publishes the descriptor startup read from
`php artisan gohorizon:capabilities --json`, as `{available, schema, package, package_version,
laravel, php, application, environment, default_connection, connections: {name: {driver,
retry_after}}, pcntl, telemetry_stream, events, operations}`. `available` is false when the
application could not answer or answered a schema this service does not understand; `error`
then says which, the descriptor fields are omitted, and the answer is still a 200. Either case
still supervises the pool: the deployment loses its controls, not its workers. `operations`
names what this service can do: `inspect_queues`, `telemetry`, `failed_jobs_list`,
`failed_job_retry` and `failed_job_delete` follow `available` because they run the package's
commands, and so does `restart_workers`: a deployment whose descriptor cannot be read is
presented as read-only until it is repaired. Any signed-in role may read it; it is read-only
and not cached (docs/adr/0014).

`POST /api/v1/capabilities/refresh` re-reads the descriptor now and answers with the same
document, so a repair can be confirmed without waiting out the 30-second interval or restarting
the service. It needs the administrator role and the token, because it starts a PHP command on
the application's host, it is recorded under `capability-refresh` with what the read found
(`readable: …` or `still read-only: …`), and it is deliberately not gated on the descriptor: the
deployment presented as read-only is exactly the one worth re-reading. A read that fails is
still a 200 with `available: false` and the reason (docs/adr/0014).

The dashboard draws that call as `Re-read now` beside the read-only banner on the workers and
failed-jobs screens: an administrator sees it, it is disabled while the request is in flight,
and the outcome sits beside it (`Readable: schema 1, package gohorizon/laravel 0.1.0`,
`Still read-only: …`, or the refusal). The controls on the page then follow the answer without a
reload.

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
