# ADR 0011 — The first administrative endpoint

**Status:** accepted (Milestone 3, third slice)
**Implements:** handoff §13 (role-based access, CSRF protection, audit logs), continuing
docs/adr/0009 and docs/adr/0010.

## Context

ADR 0010 added roles and audit records and deliberately left both unenforced, on the rule that
the check ships in the same commit as the endpoint it protects. There is an endpoint worth
protecting now: an operator's job is to act when the pool misbehaves — a worker pinned by a
stuck job, a worker that keeps crashing — and restart is the smallest action that helps and the
one the process manager already supports (`Manager.StopWorker`).

Two questions come with it: who may do it, and how a cookie-authenticated mutation is protected
from being made by another origin. ADR 0009 deferred the second to this slice.

## Decision

1. **One endpoint: `POST /api/v1/workers/{worker}/restart`**, answering 202 with
   `{worker, supervisor}`. The id is one the API just served in `GET /api/v1/health`; the API
   never accepts a command, a path or a PID (handoff §13). An id no supervisor reports is a 404
   before anything touches the pool, so a caller cannot probe the pool with invented ids, and a
   worker that exits between the snapshot and the stop is a 409 rather than a failure.
2. **Restart means stop gracefully and let the slot replace it.** The supervisor's next
   reconcile starts the replacement in the same slot, and the job in flight finishes first
   (docs/adr/0002). An operator's restart must never be the thing that loses a job, so there is
   no kill path here; a worker that will not stop is the shutdown grace's business, exactly as
   on service stop.
3. **`operator` is the minimum role.** A viewer reads; an operator acts on the work the pool is
   running; `administrator` is left for what changes the deployment rather than its work
   (configuration, capacity), which is not exposed yet. The check compares role *rank*
   (`auth.Rank`) rather than equality, so a role above the minimum passes and a role the
   service does not recognise ranks below every threshold and fails closed.
4. **The role check and the endpoint are one commit**, and the refusal is recorded: a viewer's
   attempt writes `worker-restart`, `refused`, with the actor, their role and
   `<minimum> role required` as the detail. A control that is not exercised is a control nobody
   verifies, and a refusal that leaves no line is indistinguishable from a bug.
5. **A mutation must echo a double-submit token.** The service sets a `gohorizon_csrf` cookie
   (32 random bytes, `SameSite=Lax`, `Secure` under the same trusted-proxy rule as the session
   cookie, deliberately **not** `HttpOnly` because the bundle has to read it) at sign-in, and
   again from `GET /api/v1/session` when a session carries none. `X-CSRF-Token` must equal it,
   compared in constant time. This costs no server state, and neither half is a secret: what it
   proves is that the request came from a page on this origin, because another site can neither
   read the cookie nor set the header. It complements `SameSite=Lax` rather than replacing it —
   it is what still holds if a browser ignores the attribute, or if a captured request body is
   replayed.
6. **Administration is mounted only when the deployment has users.** With no users there is
   nobody to hold a role, and an anonymous administrator is precisely what the role model exists
   to avoid, so an open service answers the route like any unknown path: 404. Openness stays a
   property of reading, as ADR 0009 decided it.
7. **The audit action is `worker-restart`**, `allowed` with `supervisor <id>` as the detail, or
   `refused` with the reason (role, token, or unknown worker). It is one line in the service log
   under `msg=audit`, like every other record (docs/adr/0010).

## Consequences

- The dashboard has no control for this yet: the endpoint is exercised by the browser suite's
  service, by the Go tests, and by an operator with `curl`. A button is a slice of its own.
- The endpoint cannot be used to *stop* a worker for good, by design: the slot always comes back.
  Taking a supervisor out of service remains a configuration change and a restart of the service.
- Refusals are recorded, so a viewer who keeps trying is visible in the log; there is still no
  queryable history, and no alerting on a refusal rate.
- The token lives as long as its cookie (one session TTL) and is not rotated per request. It is
  defence in depth for a SameSite cookie over TLS, not a substitute for either.
- Role and token checks are per request and read configuration live (ADR 0010), so a demotion
  refuses the next attempt without waiting for the session to expire.
- Deferred with their owners: a dashboard control, failed-job retry/delete (the next
  administrative actions, which will extend the same group and audit shape), and per-action
  authorisation for administrators.
