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

## Consequences

- A release is still one `go build`; the dashboard has no separate deployment or asset host.
- A dashboard change carries its rebuilt `dist` in the same commit, and a forgotten rebuild
  fails CI rather than shipping a stale screen.
- The committed JavaScript is checked by the drift check, not reviewed line by line; the
  sources stay the reviewable artifact.
- Serving goes through a plain `http.FileSystem`, so a later slice can put the dev server or
  a CDN in front without changing the router's contract.
