# ADR 0008 — The dashboard ships inside the binary

**Status:** accepted (Milestone 2)
**Implements:** handoff §12 (React dashboard), §Milestone 2 (embedded frontend assets).

## Context

The dashboard is a product component, not a development convenience (handoff §12), and it
has to reach operators the way everything else does: one binary installed by systemd or a
container, on a host that has no Node. Two questions have to be settled before a screen is
written:

- How does the bundle reach a running process?
- How does the repository keep `go build` working from a clean clone, where no
  `npm install` has run, without letting the committed bundle drift from its sources?

## Decision

`web/` is a Vite + React + TypeScript app. `web/embed.go` embeds `dist`, and `internal/api`
serves it: every path the versioned API does not claim is a file of the bundle if one
exists, and `index.html` otherwise, so a client route deep-links instead of 404ing. Hashed
assets are served `public, max-age=31536000, immutable` — Vite names each file after its
contents — while `index.html` is `no-store`, because it names the build whose assets the
next deploy may remove.

`web/dist` is committed. `go build` therefore needs no Node, and a clone, a reviewer and a
release all serve the same bytes. The price is a build artifact in the tree, so CI rebuilds
it and fails when `git status --porcelain -- web/dist` is not empty: sources and committed
output that disagree is the failure this decision could introduce. `.gitattributes` marks
the directory generated, so review tools collapse it instead of pretending it is read.

### The rules a path follows

The router owns the boundary between the API and the bundle, and the three kinds of path
answer differently:

- under `/api/`: a JSON 404, so a mistyped endpoint never returns a page a client would try to
  parse;
- under `/assets/`: a 404 when no such file exists, because `index.html` cannot stand in for a
  stylesheet or a module;
- anything else: the file if the bundle has one and `index.html` otherwise, which is what makes
  a bookmark into the app load it.

Methods other than `GET` and `HEAD` are answered 405. Nothing on this server changes state
through the dashboard, so a POST has nowhere to go and must not look as if it does.

### What the dashboard deliberately does not do

Every screen is read-only, and every request it makes is unauthenticated — the same posture as
`/api/v1/health` and `/metrics`: keep it behind the reverse proxy until Milestone 3 adds
authentication. No screen offers to retry, delete or release a job, even where it names those
outcomes: that is job administration, it needs an authenticated mutating API, and it is
Milestone 5. The dashboard renders the absence of the feature rather than a disabled button
that would imply the endpoint exists.

### Tests

`web/` tests its pure functions (`npm test`): the report-to-screen mapping, the bucket-to-column
transform, the history query, the jobs query and cursor trail, and the decoders that decide
whether a payload can be drawn at all. Behaviour that needs a server is not tested there.
`tests/browser` is the one browser test: a Playwright smoke against the compiled binary, the
fixture application's worker and a real store, asserting that the overview shows the service's
status and the queue it supervises, that the jobs page shows an attempt the worker ran, that the
failed page shows a failed attempt with the exception it threw, that the metrics page charts
both attempts in the minute they ran and reports an empty daily window as empty, and that a
deep link outside the navigation returns the shell. It runs in CI after the acceptance
suite, because it needs the fixture and Redis that suite installs.

## Consequences

- A release is still one `go build`; the dashboard has no separate deployment or asset host.
- A dashboard change carries its rebuilt `dist` in the same commit, and a forgotten rebuild
  fails CI rather than shipping a stale screen.
- The committed JavaScript is checked by the drift check, not reviewed line by line; the
  sources stay the reviewable artifact.
- Serving goes through a plain `http.FileSystem`, so a later slice can put the dev server or
  a CDN in front without changing the router's contract.
