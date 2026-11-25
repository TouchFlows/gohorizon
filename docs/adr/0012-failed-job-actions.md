# ADR 0012 — Retrying and deleting a failed job from the control plane

**Status:** accepted (Milestone 3, fourth slice; extended by the fifth with delete)
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
3. **History decides who may ask, the application decides what happens.** The uuid is looked up
   in job history before anything is asked of the application: unknown is a 404, and an attempt
   whose newest outcome is not `failed` is a 409, so the endpoints cannot be used on a payload a
   caller invented. The application's own answer decides the rest: a failed job it no longer
   stores (already retried, already forgotten) is a 404, and a command that could not run is a
   503 — the application is the one place that knows, and the endpoints do not pretend
   otherwise. One lookup, one mapping and one record shape serve both actions.
4. **The audit actions are `failed-job-retry` and `failed-job-delete`**, `allowed` with
   `queue <name>` as the detail, or `refused` with the reason (role, token, unknown job, an
   attempt that did not fail, or one the application no longer stores). Each is one line under
   `msg=audit`, like every other record (docs/adr/0010).
5. **The two commands report a missing id differently, and neither status is the whole answer.**
   `queue:retry` prints "Unable to find failed job with ID" and still exits successfully;
   `queue:forget` prints "No failed job matches the ID" and exits unsuccessfully. The adapter
   therefore reads the command's answer before it turns the status into a failure, and both
   phrasings map to its own `ErrFailedJobNotFound`. This is a version-tested reading of
   documented output, not of an internal key (docs/adr/0001).

## Consequences

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
