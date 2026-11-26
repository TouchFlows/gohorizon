# ADR 0013 — Listing the application's own failed jobs

**Status:** accepted (Milestone 3, sixth slice)
**Implements:** handoff §11 (`GET …/failed`) and §13, continuing docs/adr/0001, docs/adr/0007 and
docs/adr/0012.

## Context

Retry and delete go through Laravel's own `queue:retry` / `queue:forget`, so they act on the
application's failed store. The Failed screen had no way to read that store: it showed
`GET /api/v1/jobs?outcome=failed`, which is GoHorizon's observation of attempts, pruned after
72 hours. The two sources can disagree in both directions — a record deleted from the store is
still in history until it is pruned, and a failed record older than the raw window is gone from
history while the application still stores it. The mutation run for ADR 0012 produced the first
case: the panel offered Retry and Delete for a record the application had already deleted, so
the only possible answer was 404.

## Decision

1. **One package command: `php artisan gohorizon:failed:list --json`.** It reads
   `Illuminate\Queue\Failed\FailedJobProviderInterface::all()` — the framework surface the
   failer exposes — and never reads the `failed_jobs` table, a queue key or a provider's
   internal storage. Applications with the `database`, `file` or any custom failer all answer
   the same way.
2. **The payload never leaves the application.** A record is reported as
   `{id, uuid, connection, queue, job, failed_at, exception}`. `job` is the `displayName` (or
   `job`) Laravel recorded inside its own payload envelope; the payload and its arguments are
   not copied, returned or logged. `exception` is the first line of the stored trace, passed
   through the same redaction as a telemetry message (ADR 0006) and truncated there.
3. **Paging continues before a record id.** The interface offers no ordering or paging, so the
   command orders records by their provider id, newest first, and a page continues before the
   last id it returned. Both built-in failers allocate ids in increasing order. `--limit`
   defaults to 50 and may not exceed 200; `--before` accepts decimal digits only, because it
   becomes one argument of the command and must never be read as an option. `next_before` is
   `null` on the last page, so a client knows there is no further page without a total.
4. **One read endpoint: `GET /api/v1/failed?[limit=…][before=…]`.** It answers
   `{failed_jobs: [...], next_before}`, with `failed_jobs` empty rather than `null` when the
   application stores none. It is mounted with the other reads: a session is required, but any
   role may look, because seeing failed work is not an administrative act. A malformed limit or
   cursor is a 400; an application that cannot answer is a 503 and is not confused with an empty
   store; the response never carries the application path or the command output.
5. **The adapter owns the boundary.** It validates the page before running anything, checks the
   payload's `schema` (1, independent of the capabilities schema), and maps a failed or
   unparseable command to its own unavailable error, which the endpoint answers with 503. A
   package without the command therefore degrades to "failed jobs unavailable" rather than to an
   empty list that would look like "nothing failed".
6. **The dashboard is not switched in this slice.** The Failed screen still reads history until
   a follow-up changes it; the endpoint exists so that change is a client change and not another
   server one.

## Consequences

- The listing reads every record the failer returns and slices the page in PHP: `all()` is the
  whole interface. A store with very many failed jobs costs one full read per page. A future
  provider-specific pager would need a new interface or capability, and is not assumed here.
- Ordering is by provider id. A custom failer that assigns ids out of order gets pages in its
  own id order; the command does not invent a timestamp sort key the interface does not promise.
- Until the dashboard switches, the two sources still disagree, and the screen keeps its current
  caveat. After the switch, failed history remains useful as attempt history: it records what ran
  and when, while this endpoint records what the application can still act on.
- The Go API has gained a read that shells out to the application per request. It is bounded
  (15 s command timeout) and read-only, and it is the same pattern queue depth already uses.
