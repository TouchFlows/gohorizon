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
   Cookies are `HttpOnly`, `SameSite=Lax`, `Path=/`, and `Secure` whenever the browser
   reached the service over TLS: either the request itself carries a TLS connection, or a
   proxy terminated it and said so in `X-Forwarded-Proto`. The second half is the normal
   deployment, and without it every session cookie behind a proxy would be issued without
   `Secure`. **Both proxy decisions read one list**: `X-Forwarded-Proto` is believed only
   from a peer in `server.trusted_proxies`, the same list that says whose `X-Forwarded-For`
   the limiter counts. A deployment therefore cannot trust a forwarded header while not
   trusting its sender, and a client that is not a proxy cannot have its cookie marked
   `Secure` — which, since a browser drops such a cookie over plain HTTP, would otherwise be
   a way to have the browser throw away the session it just received.
4. **Sign-out revokes the token in memory.** A signed token cannot be un-signed, so the
   store keeps a revocation list of ids until their expiry. It is memory: a restart loses
   it, and a copied token would work again until it expires. That is why the TTL is short
   (12 h default, `auth.session_hours`) and why signing out is not a credential rotation.
5. **A session names a user, and the user still has to exist.** The configuration is the
   source of truth for who may sign in, so a token whose subject is no longer in
   `server.auth.users` is refused at the next request. Removing an operator ends their
   access immediately instead of when the signature expires, and it does not depend on the
   in-memory revocation list, which a restart would have lost.
6. **Failed sign-ins are limited per client address**: five failures in a minute lock that
   address for the rest of the window, whether or not the next password is correct. The
   answer for a wrong password and an unknown user is byte-identical, and an unknown name
   still costs one hash verification, so names cannot be timed or read apart. **The address
   is the client, not the peer**: behind a reverse proxy every request arrives from the
   proxy, and counting the peer would let five bad guesses from one person lock out every
   operator. `server.trusted_proxies` names the proxies whose `X-Forwarded-For` is believed
   (addresses or CIDR ranges); the chain is walked from the right over the hops this
   deployment runs, and the first hop it does not run is the client. A request from a peer
   that is not trusted is counted on that peer whatever it claims in the header, so a direct
   client cannot rotate the header to outrun the limit, and an unreadable chain falls back to
   the peer rather than to the client's word for it.
7. **The gate covers the operational API only.** Everything under `/api/v1` except the
   session routes requires a session. `/metrics` stays open because Prometheus scrapes it
   without one, and the dashboard shell stays open because it is a bundle with no data:
   its fetches are what need the cookie. `GET /api/v1/session` is registered even with no
   users, answering 404 in the API's JSON envelope, so the bundle can tell an open service
   from a signed-out browser instead of guessing from a missing route.
8. **The dashboard asks for the session before it draws a page.** A signed-out browser gets
   the sign-in screen and nothing else; a 401 on the health poll (an expiry, a sign-out in
   another tab, a removed user) drops back to it rather than being reported as a failed
   read. An open service answers "no authentication is configured" and the pages are drawn
   exactly as they were before this ADR, with no sign-out control.
9. **No roles yet.** The handoff's three roles arrive with the endpoints that need them
   (worker restart, failed-job retry/delete); a role field no endpoint reads would be
   configuration that looks like a control. docs/adr/0010 adds the role as per-user
   configuration and audit data and keeps enforcement with that first endpoint. CSRF tokens are deferred the same way: the
   only state-changing calls today are sign-in and sign-out, both protected by
   `SameSite=Lax` plus the same-origin dashboard, and the double-submit token lands with
   the first administrative mutation.

## Consequences

- A deployment with users is now usable by a person: the bundle signs in, keeps the cookie
  the service set, and offers a sign-out that both clears the cookie and revokes the token.
- Removing a user from the configuration is an access control, not only a tidy-up: their
  existing sessions stop at the next request. Rotating the signing key remains the lever for
  a suspected session leak.
- `server.trusted_proxies` is optional and empty by default: a deployment behind a proxy that
  does not list it gets one shared sign-in budget, which is the safe failure (a lockout)
  rather than an unthrottled one, and its cookies are issued according to the connection it
  sees, which is plain HTTP and so without `Secure`.
- Role-based authorisation, audit logs and CSRF tokens are unbuilt; the gate is
  authentication only.
- Sessions are per instance. v1 runs one instance behind the ownership lock, so there is
  no shared session store to build yet.
- Rotating the signing key signs everyone out, which is the intended lever for a suspected
  session leak.
