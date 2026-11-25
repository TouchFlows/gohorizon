# ADR 0012 — Retrying a failed job from the control plane

**Status:** accepted (Milestone 3, fourth slice)
**Implements:** handoff §11 (`POST /failed/{id}/retry`) and §13, continuing docs/adr/0001,
docs/adr/0010 and docs/adr/0011.

## Context

The API already serves failed attempts as job history (`GET /api/v1/jobs?outcome=failed` and
`GET /api/v1/jobs/{id}`), and ADR 0011 left failed-job retry as the next administrative action
under the same operator role, the same token and the same audit shape. The question this slice
answers is where a retry is allowed to happen.

GoHorizon's own history is an observation: it stores what a worker reported about an attempt,
not the payload, and it is pruned. Laravel owns the failed-job record and the payload behind it
(the `failed_jobs` table, a file, DynamoDB, or a driver that discards them entirely). Writing
either of those from Go would break the rule the project is built around — Go never reads or
writes a Laravel queue key, and framework-specific operations go through the adapter
(handoff §11, docs/adr/0001).

## Decision

1. **One endpoint: `POST /api/v1/failed/{job}/retry`**, answering 202 with `{job, queue}`. The
   identity is the one the API already serves: the job's uuid (the `id:` fallback an attempt can
   carry has no failed record to retry and is refused). It is mounted in the administrative
   group, so it needs an `operator` session and the double-submit token, and it is not mounted
   at all on a service without users (docs/adr/0011).
2. **The retry is the application's own command.** The adapter runs
   `php artisan queue:retry <uuid>` and nothing else: Laravel forgets the failed record and
   re-publishes the payload it stored, exactly as an operator running the command would get.
   GoHorizon writes no queue key, no payload and no failed record of its own.
3. **History decides who may ask, the application decides what happens.** The uuid is looked up
   in job history before anything is asked of the application: unknown is a 404, and an attempt
   whose newest outcome is not `failed` is a 409, so the endpoint cannot be used to push a
   payload a caller invented. The application's own answer decides the rest: a failed job it no
   longer stores (already retried, already forgotten) is a 404, and a command that could not run
   is a 503 — the application is the one place that knows, and the endpoint does not pretend
   otherwise.
4. **The audit action is `failed-job-retry`**, `allowed` with `queue <name>` as the detail, or
   `refused` with the reason (role, token, unknown job, an attempt that did not fail, or one the
   application no longer stores). It is one line under `msg=audit`, like every other record
   (docs/adr/0010).
5. **`queue:retry` reports a missing id on its output and exits successfully.** The adapter
   therefore reads the command's answer as well as its status, and both phrasings Laravel
   uses for "unable to find" map to its own `ErrFailedJobNotFound`. This is a version-tested
   reading of documented output, not of an internal key (docs/adr/0001).

## Consequences

- Retry is the action this slice ships; deleting a failed job (`DELETE /failed/{id}`, through
  `queue:forget`) is the other half of the same pair and reuses this adapter and audit shape.
- The dashboard still has no control for it. An operator reaches the endpoint with `curl`, and
  the browser suite retries the failing fixture job through the real binary and reads the new
  attempt and the audit record it left.
- A retried job is executed again by whatever supervisor serves its queue, so a job that always
  throws fails again and is stored again. The endpoint reports the retry, not the outcome.
- The smoke fixture had to keep failed jobs to have one worth retrying: it now uses Laravel's
  `file` failer. The acceptance fixture still uses `null`, so no test depends on a shared store.
- Refusals are recorded but not queryable, as with a restart; a queryable audit history is still
  deferred.
