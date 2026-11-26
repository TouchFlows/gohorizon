# ADR 0013 — Listing the application's own failed jobs

**Status:** accepted (Milestone 3, sixth slice; extended by the seventh with the dashboard switch)
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

1. **One package command: `php artisan gohorizon:failed:list --json`.** Laravel's two
   database failers (`DatabaseFailedJobProvider` and `DatabaseUuidFailedJobProvider`) expose
   their table through a public `getTable()`, so the command reads one page from it with a
   keyset query instead of materialising the store. Every other failer — the file failer and
   custom implementations — is read through the framework's
   `FailedJobProviderInterface::all()`, the only listing surface the interface offers. A queue
   key or a provider's private storage is never touched, and all sources answer with the same
   payload.
2. **The payload never leaves the application.** A record is reported as
   `{id, uuid, connection, queue, job, failed_at, exception}`. `job` is the `displayName` (or
   `job`) and `uuid` is the job uuid Laravel recorded inside its own payload envelope — the
   file failer stores no separate uuid column, and the database failer's payload is the only
   place the uuid the retry endpoint takes is guaranteed to be. The payload and its arguments
   are not copied, returned or logged. `exception` is the first line of the stored trace with
   the ` in <file>:<line>` location removed, passed through the same redaction as a telemetry
   message (ADR 0006) and truncated there.
3. **Paging continues after an opaque record position.** The interface offers no ordering or
   paging, so the command orders records by their failure time, newest first, then by id, and a
   page continues after a cursor holding the last returned record's `failed_at` and id. Ids are
   not assumed to be numbers: the file failer uses the job uuid as the record id, and the
   database failer uses a row id, so a numeric-id cursor would drop every record in one of the
   two. The keyset page applies the same two-part order in SQL and asks for at most one record
   more than the page; the cursor shape does not depend on which path read it. `--limit`
   defaults to 50 and may not exceed 200; `--before` is the previous page's `next_before` and
   is only ever sent as the value of one option. An unreadable cursor is
   refused with the command's invalid-argument status, which the adapter maps to a 400 rather
   than to an unavailable application. `next_before` is `null` on the last page, so a client
   knows there is no further page without a total. The cursor is unpadded base64url JSON of
   `{"at":<unix seconds>,"id":"<record id>"}`, and its bytes are pinned by a test and replayed
   against both the keyset query and the `all()` path, so the two read the same page from the
   same position.
4. **The keyset page needs the store's own index.** Laravel's `failed_jobs` migration creates
   no index beyond the primary key, so `order by failed_at desc, id desc` still sorts the whole
   table to answer a page. The store belongs to the application, so the package does not alter
   its schema; the operator adds a composite index in the store's own ordering instead:

   ```sql
   -- database-uuids / DatabaseUuidFailedJobProvider, ordered by (failed_at, uuid)
   create index failed_jobs_failed_at_uuid_index on failed_jobs (failed_at, uuid);

   -- classic DatabaseFailedJobProvider, ordered by (failed_at, id)
   create index failed_jobs_failed_at_id_index on failed_jobs (failed_at, id);
   ```

   The column order matches the ordering, so the index satisfies both keys and the page is a
   seek plus a bounded read rather than a sort. Without it the query is still one bounded page,
   but the database reads and sorts the table to produce it.
5. **One read endpoint: `GET /api/v1/failed?[limit=…][before=…]`.** It answers
   `{failed_jobs: [...], next_before}`, with `failed_jobs` empty rather than `null` when the
   application stores none. It is mounted with the other reads: a session is required, but any
   role may look, because seeing failed work is not an administrative act. A malformed limit or
   cursor is a 400; an application that cannot answer is a 503 and is not confused with an empty
   store; the response never carries the application path or the command output.
6. **The adapter owns the boundary.** It validates the page before running anything, checks the
   payload's `schema` (1, independent of the capabilities schema), and maps a failed or
   unparseable command to its own unavailable error, which the endpoint answers with 503. A
   package without the command therefore degrades to "failed jobs unavailable" rather than to an
   empty list that would look like "nothing failed".
7. **The dashboard reads this endpoint.** The Failed screen lists the application's own store,
   pages it with the opaque cursor and keeps `Retry` and `Delete` in the record's detail panel.
   It no longer reads `GET /api/v1/jobs?outcome=failed`; attempt history keeps its own role on
   the Jobs screen. A record the store no longer holds therefore cannot be offered for an
   action, which is the disagreement this ADR exists to remove.

## Consequences

- A database-backed failed store costs one bounded keyset query per page, not a full read. The
  classic provider ties on its row id and the uuid provider on its uuid column, matching the id
  that provider reports as the record id; the cursor carries whichever id the store issued. The
  query still reads one ordered page rather than materialising the table in PHP, but on a store
  without the composite index above the ordering cost is the database's, not the page's — a
  large table is sorted to answer each page. Adding `(failed_at, id)` (or `(failed_at, uuid)`)
  makes that cost a bounded seek, which is decision 4's guidance and the reason this ADR names
  the index rather than leaving it to the reader.
- The file failer is still read whole, but Laravel bounds that file itself (`queue.failed.limit`,
  100 by default), so its read stays small. A custom failer is read whole because the interface
  exposes no paging, and that cost belongs to the store's own design. Both fallbacks still order
  every record they were given and slice the same page, so the cursor and the page stay exact.
- Ordering is by failure time and then id. A custom failer whose records carry neither gets
  the epoch as a time, which groups them; the command does not invent a timestamp the record
  does not have.
- Failed history remains useful as attempt history: it records what ran and when, while this
  endpoint records what the application can still act on. The two are expected to differ after a
  retry or a delete, which is why the screen reads the store rather than the history.
- The Go API has gained a read that shells out to the application per request. It is bounded
  (15 s command timeout) and read-only, and it is the same pattern queue depth already uses.
