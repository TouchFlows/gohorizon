# GoHorizon Roadmap

This document describes the intended product direction. It is not a delivery commitment. Priorities may change based on implementation findings, user feedback, and maintainer capacity.

## Product direction

GoHorizon aims to become a universal queue operations and worker orchestration control plane.

It starts with Laravel 13 and Redis, then evolves through independently testable broker and runtime adapters. The goal is a common operational experience without flattening the differences between queue systems.

## Status legend

- **Implemented / verify:** reported in the repository during the November 2026 review; re-check source and tests before treating as complete.
- **In progress:** work may exist but needs completion, hardening, or explicit acceptance criteria.
- **Planned:** future direction; do not advertise as available.
- **Exploratory:** requires a design decision or proof of feasibility.

## Phase 1 — Reliable Laravel + Redis foundation

**Goal:** make the initial integration dependable, secure, understandable, and installable.

Areas reported as implemented or substantially present; verify each against current code:

- Go control plane and configuration validation.
- Redis coordination and telemetry ingestion.
- Worker process supervision and lifecycle management.
- Queue depth and worker visibility.
- Failed-job inspection and supported operations.
- SQLite historical metrics and rollups.
- Health and Prometheus metrics endpoints.
- React dashboard.
- Laravel Composer integration.
- Authentication and session protections.
- Integration and browser acceptance tests.

Remaining hardening areas to assess:

- Per-action authorization and least-privilege permissions.
- Queryable, tamper-aware audit history.
- Upgrade and migration strategy.
- Backup, restore, and retention behavior.
- Operational diagnostics and actionable error messages.
- Reproducible installation and upgrade documentation.
- Supported-version compatibility matrix.
- Tagged release and release notes.

## Phase 2 — Stable public release

**Goal:** establish a trustworthy installation and maintenance experience.

Deliverables:

- A selected open-source license in the repository root.
- Versioned configuration reference.
- Documented prerequisites and deployment guide.
- Reproducible build and release process.
- Installation and upgrade tests.
- A clear support policy and security reporting process.
- First tagged release only when the maintainer considers the product usable and sufficiently tested.
- Screenshots or a short demonstration based on the real application.

## Phase 3 — Adapter architecture

**Goal:** make extensibility a real architectural property, not just a slogan.

Design and implement explicit interfaces for:

- Broker adapters.
- Runtime/framework adapters.
- Capability discovery and negotiation.
- Normalized operational metadata, with broker-specific extensions where needed.
- Adapter health and compatibility reporting.

The adapter contract must document which operations are supported, unsupported, or only partially supported. It must not manufacture uniform semantics where none exist.

## Phase 4 — RabbitMQ integration

**Goal:** add RabbitMQ as a second broker through a dedicated adapter.

Design and validate:

- Queue and consumer inspection.
- Message counts and available broker metadata.
- Acknowledgement and requeue semantics.
- Dead-letter and retry patterns where configured.
- Connection and permission requirements.
- Operational actions that are safe and supported.
- Differences between Redis and RabbitMQ surfaced clearly in the UI and API.

Do not describe RabbitMQ as supported until the adapter, documentation, integration tests, and operational safety checks are complete.

## Phase 5 — Additional runtimes

**Goal:** support workers and applications beyond the initial Laravel/PHP integration.

Potential ecosystems include Go, Python, and Node.js. These are exploratory targets, not current support claims.

Each runtime integration should define:

- How it identifies an application and worker.
- How it reports job lifecycle events.
- How it authenticates telemetry.
- Which controls can be safely requested.
- How version compatibility is declared.

## Phase 6 — Broader operational platform

Potential future areas, subject to demand and design review:

- Additional broker adapters.
- More advanced worker balancing and scaling policies.
- Deeper OpenTelemetry integration.
- PostgreSQL persistence option.
- Multi-instance control-plane coordination and high availability.
- Richer alerting and operational automation.

The initial deployment scope is a single deployment. Distributed control-plane HA should not be introduced prematurely.

## Non-goals and guardrails

- GoHorizon is not intended to become a new message broker.
- It should not replace the application framework's job execution code.
- It should not claim exactly-once processing as a generic guarantee.
- It should not hide broker-specific delivery and retry behavior.
- It should not introduce distributed control-plane complexity before the single-deployment product is reliable.

## How roadmap changes are made

Feature proposals should explain the operational problem, affected integrations, security implications, and test strategy. The maintainer should update this document when priorities or status materially change.
