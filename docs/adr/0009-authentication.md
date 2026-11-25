# ADR 0009 — Sign-in for the operational API

**Status:** accepted (Milestone 3, first slice)
**Implements:** handoff §13 (authentication, secure sessions, rate limiting, CSRF).

## Context

Milestones 1 and 2 left every endpoint open. The service binds to localhost by default
and the README told operators to keep it behind a reverse proxy until Milestone 3 added
authentication. The dashboard has no sign-in screen, and the API has no notion of a
caller: `GET /api/v1/health` reports queue depth, worker state and telemetry counters to
anyone who can reach the port, and job history carries exception messages.

Handoff §13 asks for authentication, secure session handling, CSRF protection for cookie
authentication, rate limiting and three roles (viewer, operator, administrator).

## Decision

1. **Authentication is opt-in per deployment.** `server.auth.users` non-empty turns it on;
   absent leaves the service open exactly as it was, which keeps localhost development and
   every existing test honest. A block that names a key environment variable but no users
   is a validation error, never a silently open service, and a configured deployment whose
   signing key is missing refuses to start (`auth.New` runs before the listener does).
2. **Passwords are PBKDF2-HMAC-SHA256 hashes** in the form
   `pbkdf2-sha256$<iterations>$<salt>$<key>`, 210 000 iterations, 16-byte salt, 32-byte
   key, produced by `gohorizon passwd`. The iteration count travels with the hash, so it
   can be raised without invalidating existing users. The standard library's
   `crypto/pbkdf2` (Go 1.24+) keeps this dependency-free; bcrypt and argon2 would both be a
   new module for a dashboard with a handful of operators.
3. **Sessions are signed stateless cookies**, `gohorizon_session`, carrying the user, an
   expiry and a random id, HMAC-SHA256 signed with a key read from the environment named by
   `auth.session_key_env` (at least 32 bytes). Stateless means a service restart does not
   sign every operator out, which matters because the service is restarted to deploy.
   Cookies are `HttpOnly`, `SameSite=Lax`, `Path=/`, and `Secure` whenever the request
   arrived over TLS.
4. **Sign-out revokes the token in memory.** A signed token cannot be un-signed, so the
   store keeps a revocation list of ids until their expiry. It is memory: a restart loses
   it, and a copied token would work again until it expires. That is why the TTL is short
   (12 h default, `auth.session_hours`) and why signing out is not a credential rotation.
5. **Failed sign-ins are limited per address**: five failures in a minute lock that
   address for the rest of the window, whether or not the next password is correct. The
   answer for a wrong password and an unknown user is byte-identical, and an unknown name
   still costs one hash verification, so names cannot be timed or read apart.
6. **The gate covers the operational API only.** Everything under `/api/v1` except the
   session routes requires a session. `/metrics` stays open because Prometheus scrapes it
   without one, and the dashboard shell stays open because it is a bundle with no data:
   its fetches are what need the cookie.
7. **No roles yet.** The handoff's three roles arrive with the endpoints that need them
   (worker restart, failed-job retry/delete); a role field no endpoint reads would be
   configuration that looks like a control. CSRF tokens are deferred the same way: the
   only state-changing calls today are sign-in and sign-out, both protected by
   `SameSite=Lax` plus the same-origin dashboard, and the double-submit token lands with
   the first administrative mutation.

## Consequences

- **The dashboard has no sign-in screen yet.** That is the next slice: with auth enabled
  today, the pages' fetches answer 401 and the UI shows its read failures. Nothing is
  silently open, and nothing claims to work that does not.
- Role-based authorisation, audit logs and CSRF tokens are unbuilt; the gate is
  authentication only.
- Sessions are per instance. v1 runs one instance behind the ownership lock, so there is
  no shared session store to build yet.
- Rotating the signing key signs everyone out, which is the intended lever for a suspected
  session leak.
