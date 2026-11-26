# ADR 0012 — Retrying and deleting a failed job from the control plane

**Status:** accepted (Milestone 3, fourth slice; extended by the fifth with delete, and by the
failed-store slice with a gate the store decides)
**Implements:** handoff §11 (`POST /failed/{id}/retry`) and §13, continuing docs/adr/0001,
docs/adr/0010 and docs/adr/0011.

## Context

The API already serves failed attempts as job history (`GET /api/v1/jobs?outcome=failed` and
`GET /api/v1/jobs/{id}`), and ADR 0011 left failed-job retry and delete as the next
administrative actions under the same operator role, the same token and the same audit shape.
The question this slice answers is where those actions are allowed to happen.

GoHorizon's own history is an observation: it stores what a worker reported about an attempt,
not the payload, and it is pruned. Laravel owns the failed-job record and the payload behind it
(the `failed_jobs` table, a file, DynamoDB, or a driver that discards them entirely). Writing
either of those from Go would break the rule the project is built around — Go never reads or
writes a Laravel queue key, and framework-specific operations go through the adapter
(handoff §11, docs/adr/0001).

## Decision

1. **Two endpoints: `POST /api/v1/failed/{job}/retry` and `DELETE /api/v1/failed/{job}`.** The
   retry answers 202 with `{job, queue}` because the work is queued rather than done; the delete
   answers 200 with the same body because the record is already gone when the application
   replies. The identity is the one the API already serves: the job's uuid (the `id:` fallback
   an attempt can carry has no failed record and is refused). Both are mounted in the
   administrative group, so they need an `operator` session and the double-submit token, and
   neither is mounted at all on a service without users (docs/adr/0011).
2. **Both actions are the application's own commands.** The adapter runs
   `php artisan queue:retry <uuid>` or `php artisan queue:forget <uuid>` and nothing else:
   Laravel forgets the failed record and re-publishes or discards the payload it stored, exactly
   as an operator running the command would get. GoHorizon writes no queue key, no payload and
   no failed record of its own.
3. **The application's store decides, and history is only detail.** The id from the path goes
   straight to `queue:retry` / `queue:forget`, whose answer is the whole verdict: a record the
   store does not hold (never stored, already retried, already forgotten) is a 404, a command
   that could not run is a 503, and anything else is the action having happened. Job history is
   read afterwards for one thing only — the queue name in the answer and in the audit record —
   and its absence never refuses: history is pruned to 72 hours, so a failed record older than
   its window is still actionable, and a deployment with history disabled still retries and
   deletes. Nothing local is used as an allow-list, so the id is checked only for the shape that
   keeps it one, non-option argument on the command line; whether the record exists is the
   store's answer to give (docs/adr/0013). An earlier revision decided this in history instead
   (404 unknown, 409 when the newest outcome was not `failed`, 503 when history was disabled),
   which listed records the screen showed but the actions refused.
4. **The audit actions are `failed-job-retry` and `failed-job-delete`**, `allowed` with
   `queue <name>` as the detail (`queue unknown` when history cannot name it), or `refused` with
   the reason (role, token, or one the application no longer stores). Each is one line under
   `msg=audit`, like every other record (docs/adr/0010).
5. **The two commands report a missing id differently, and neither status is the whole answer.**
   `queue:retry` prints "Unable to find failed job with ID" and still exits successfully;
   `queue:forget` prints "No failed job matches the ID" and exits unsuccessfully. The adapter
   therefore reads the command's answer before it turns the status into a failure, and both
   phrasings map to its own `ErrFailedJobNotFound`. This is a version-tested reading of
   documented output, not of an internal key (docs/adr/0001).

## Consequences

- A record the failed screen lists is actionable, whatever its age: the store that listed it is
  the store that decides, so pruned history and a deployment with `storage.enabled: false` no
  longer refuse an action the application would have performed. The cost is that the action
  itself is the existence check — a caller asking about an id the store does not hold learns it
  from the application's answer rather than from a local lookup.
- Retry and delete are the two actions of this pair. The browser suite retries the failing
  fixture job through the real binary and reads the new attempt and the audit record it left;
  delete is covered by the handler and adapter tests, which drive `queue:forget` through a fake
  artisan and the handler through a stub provider.
- The dashboard still has no control for either. An operator reaches the endpoints with `curl`.
- A retried job is executed again by whatever supervisor serves its queue, so a job that always
  throws fails again and is stored again. The endpoint reports the retry, not the outcome.
- The smoke fixture had to keep failed jobs to have one worth retrying: it now uses Laravel's
  `file` failer. The acceptance fixture still uses `null`, so no test depends on a shared store.
  Deleting the smoke's failing job would remove the attempt later cases read, so the smoke does
  not delete it.
- Refusals are recorded but not queryable, as with a restart; a queryable audit history is still
  deferred.
