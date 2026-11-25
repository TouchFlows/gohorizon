# ADR 0010 — Roles and audit records

**Status:** accepted (Milestone 3, second slice)
**Implements:** handoff §13 (three roles, audit log), continuing docs/adr/0009.

## Context

ADR 0009 added authentication and deferred two things it named explicitly: roles, because no
endpoint read one, and audit records, because there was no actor to record. There is an actor
now, and the next slice of Milestone 3 is administration — restarting a worker, retrying a
failed job — which is the first thing worth restricting and the first thing worth recording.

## Decision

1. **A role is a property of the configured user**, `server.auth.users[].role`, one of
   `viewer`, `operator`, `administrator`. It travels inside the signed session token, so
   reading it back costs no lookup per request and a client cannot edit it. An absent role is
   `viewer`: least privilege for a user added without one, and also what a token signed before
   roles existed reads as, so an upgrade does not sign everyone out. An unknown role is a
   validation error and is refused by the session store too, so a configuration that never
   passed validation cannot start with a role nobody can check.
2. **Nothing is enforced yet, and that is stated rather than implied.** Every endpoint today
   reports operational state, which the least of the three roles may read, so there is nothing
   for a check to refuse. The role is audit data and a value the API reports; the middleware
   that refuses a `viewer` arrives with the first mutating endpoint, in the same commit as the
   endpoint, because a check with nothing to protect is a control nobody verifies.
3. **An audit record is one line in the service log**, written under the message `audit` with
   stable field names: `action`, `outcome`, `user`, `role`, `address`, and `detail` when it
   says something. Actions are dotted names (`sign-in`, `sign-out`); outcomes are `allowed`,
   `refused` or `limited`. `address` is the same client address the sign-in limiter counts, so
   a record and a lockout can be read against one another.
4. **Session actions are recorded: sign-in allowed, refused and limited, and sign-out.** These
   are the events that exist, they are the ones an operator asks about after a lockout or a
   suspected leak, and they are written by the handlers that already answer them. A refused
   sign-in names the name that was claimed and carries no role, because no role was
   established; the actor's role appears only where a session existed.
5. **A record cannot fail the action it describes.** The write is a log call with nothing to
   return; a lost line must not turn a successful sign-in into an error. Shipping, rotation
   and retention are the operator's, through whatever reads the service log.

## Consequences

- The audit trail is only as durable and as trustworthy as the service log: it is
  append-only in practice, not tamper-evident, and it is not queryable from the dashboard.
- `GET /api/v1/session` reports the role, so the dashboard can show it later; nothing in the
  bundle branches on it yet.
- Deferred with their owners: the authorisation middleware and the endpoints it guards, a
  queryable audit history (and the store it would need), and audit records for actions other
  than sessions — those arrive with the actions.
