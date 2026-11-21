# GoHorizon
## Technical Architecture & Implementation Handoff

**Project:** GoHorizon  
**Version:** 1.0 — Initial implementation specification  
**Status:** Approved for development  
**Backend:** Go  
**Dashboard:** React + TypeScript + Vite  
**Laravel integration:** Composer package  
**Target framework:** Laravel 13  
**PHP:** 8.3+  
**Queue backend:** Existing Redis  
**Job execution:** Existing Laravel PHP workers  
**Initial deployment scope:** Single Laravel application  
**Future architecture:** Multi-application capable  
**Persistence:** SQLite initially  
**Operating system:** Linux / systemd

---

# 1. Project objectives

Build an independent Go-based queue management and worker supervision platform inspired by Laravel Horizon.

GoHorizon will provide:

- Queue monitoring.
- Worker process supervision.
- Job execution metrics.
- Automatic worker recovery.
- Configurable worker allocation.
- Dynamic queue balancing.
- Failed job monitoring and management.
- Job lifecycle tracking.
- Operational dashboard.
- HTTP API.
- Prometheus-compatible metrics.

GoHorizon must work with existing Laravel applications without requiring Laravel Horizon.

## 1.1 Fundamental architectural decision

GoHorizon is NOT a replacement queue engine.

It is a control plane that manages existing Laravel queue workers.

The responsibilities are divided as follows:

| Responsibility | Technology |
|---|---|
| Job execution | Laravel / PHP |
| Job serialization | Laravel |
| Queue transport | Existing Redis |
| Worker lifecycle | Go |
| Worker balancing | Go |
| Queue monitoring | Go |
| Metrics collection | Go + Laravel adapter |
| Dashboard | React + TypeScript |
| Historical metrics | SQLite |
| Configuration | YAML |

Existing Laravel applications and their jobs must continue working without modifications to business logic.

## 1.2 Initial deployment scope

GoHorizon v1 manages one Laravel application per deployment.

However, internal application identifiers and component interfaces must be designed so that future multi-application support does not require a fundamental architectural rewrite.

Do not implement multi-tenant administration, application discovery or cross-application management in v1.

---

# 2. Non-negotiable design principles

1. Never modify Laravel queue payloads.
2. Never remove jobs directly from Redis.
3. Never acknowledge or release jobs from Go.
4. Never interfere with Laravel retry and reservation mechanisms.
5. Never assume undocumented Laravel Redis key structures are stable.
6. Never terminate workers abruptly during normal operation.
7. Never restart all workers when only one needs restarting.
8. Never expose Redis credentials through the dashboard or API.
9. Never allow two independent supervisors to manage the same worker pool.
10. Preserve job execution semantics across deployments and service restarts.
11. Telemetry failures must never prevent jobs from executing.
12. The supervisor must recover safely after its own restart.

---

# 3. System architecture

## 3.1 High-level architecture

```text
                  GoHorizon
       +-----------------------------+
       |                             |
       |       Go Control Plane      |
       |                             |
       |  Configuration Manager      |
       |  Process Supervisor         |
       |  Queue Monitor              |
       |  Balancing Engine           |
       |  Metrics Collector          |
       |  HTTP API                   |
       |  Authentication             |
       |                             |
       +-------------+---------------+
                     |
          +----------+-----------+
          |                      |
          v                      v
      Redis Adapter         Process Manager
          |                      |
          |                      v
          |                 PHP Processes
          |                      |
          |                      v
          |                 Laravel Workers
          |                      |
          v                      v
      Redis Queue <-------- Laravel Application
          ^
          |
      Laravel Adapter
      Telemetry Publisher
      Queue Inspection
```

## 3.2 Component responsibilities

### A. Go control plane

- Application lifecycle.
- Configuration loading.
- Worker orchestration.
- Queue monitoring.
- Metrics aggregation.
- Balancing decisions.
- HTTP API.
- Health checks.
- Authentication.

### B. Redis adapter

- Redis connectivity.
- Queue depth sampling.
- Reading telemetry events.
- Supervisor coordination.
- Distributed locking.
- Metrics persistence coordination.

Separate namespaces for:

- Laravel queue data.
- GoHorizon telemetry.
- GoHorizon coordination.

Suggested prefix:

`gohorizon:{application}:{environment}:`

### C. Process manager

- Launch Laravel workers.
- Track process IDs.
- Monitor process exits.
- Restart failed workers.
- Apply configuration changes.
- Gracefully stop workers.
- Enforce process limits.
- Maintain desired versus actual worker counts.

### D. Laravel adapter

Composer package:

`gohorizon/laravel`

Responsibilities:

- Publish Laravel-specific telemetry.
- Expose application metadata.
- Report job lifecycle events.
- Provide queue inspection capabilities.
- Provide safe administrative operations.
- Expose framework capabilities and versions.

The package is optional for basic process supervision but required for complete Laravel-specific telemetry.

---

# 4. Repository structure

Use a monorepo.

```text
gohorizon/
|
+-- cmd/
|   +-- gohorizon/
|       +-- main.go
|
+-- internal/
|   +-- config/
|   +-- application/
|   +-- supervisor/
|   +-- process/
|   +-- redis/
|   +-- queues/
|   +-- telemetry/
|   +-- metrics/
|   +-- balancing/
|   +-- api/
|   +-- auth/
|   +-- storage/
|   +-- events/
|   +-- health/
|
+-- web/
|   +-- frontend/
|   |   +-- src/
|   |   +-- public/
|   |   +-- package.json
|   |   +-- vite.config.ts
|   |   +-- tsconfig.json
|   |
|   +-- embed.go
|
+-- migrations/
|
+-- deployments/
|   +-- systemd/
|   +-- docker/
|
+-- examples/
|   +-- gohorizon.yaml
|
+-- packages/
|   +-- laravel/
|       +-- composer.json
|       +-- src/
|       +-- config/
|       +-- routes/
|       +-- tests/
|
+-- docs/
|   +-- architecture.md
|   +-- configuration.md
|   +-- api.md
|   +-- security.md
|
+-- tests/
|   +-- integration/
|   +-- fixtures/
|
+-- go.mod
+-- README.md
+-- LICENSE
```

The Laravel package remains in the same repository initially but must be independently releasable through Composer.

---

# 5. Technology choices

## Go backend

| Purpose | Technology |
|---|---|
| Redis | go-redis |
| HTTP routing | Chi |
| Configuration | koanf |
| Logging | slog |
| Metrics | Prometheus client |
| SQLite | modernc.org/sqlite |
| PostgreSQL future option | pgx |
| Database migrations | golang-migrate |
| Process management | os/exec |
| Static asset embedding | Go embed |

Prefer standard Go interfaces and dependency injection through constructors.

Avoid unnecessary frameworks.

## Frontend

Mandatory stack:

- React.
- TypeScript.
- Vite.
- React Router or equivalent.
- Lightweight component library.
- Responsive dashboard.
- Embedded production assets.

The frontend must be independently developed and built.

The final production build must be embedded into the Go binary.

Development mode may use the Vite development server.

Do not use Laravel Filament or Livewire for the GoHorizon dashboard.

Filament remains available for the Laravel application's own administration.

---

# 6. Laravel integration package

## 6.1 Package requirements

- Laravel 13.
- PHP 8.3+.
- Composer.
- No dependency on Laravel Horizon.

Namespace:

`GoHorizon\Laravel`

Configuration:

`config/gohorizon.php`

Example:

```php
return [
    'enabled' => env('GOHORIZON_ENABLED', true),

    'application' => env('GOHORIZON_APP_NAME', env('APP_NAME')),

    'environment' => env('APP_ENV'),

    'redis' => [
        'connection' => env('GOHORIZON_REDIS_CONNECTION', 'default'),
        'prefix' => 'gohorizon',
    ],

    'telemetry' => [
        'enabled' => true,
        'batch_size' => 100,
        'flush_interval' => 2,
    ],

    'tags' => [
        'enabled' => true,
    ],
];
```

## 6.2 Job lifecycle events

Listen to the appropriate Laravel queue events.

At minimum:

- JobProcessing.
- JobProcessed.
- JobExceptionOccurred.
- JobFailed.
- JobReleasedAfterException, if available.

Verify the actual Laravel 13 event classes, payloads and lifecycle semantics against framework source and integration tests.

Capture:

- Application ID.
- Environment.
- Queue connection.
- Queue name.
- Job UUID.
- Job class.
- Attempt number.
- Timestamp.
- Execution duration.
- Success/failure status.
- Exception class.
- Sanitized exception message.
- Job tags.

Never publish complete job payloads or sensitive arguments.

## 6.3 Telemetry transport

Use a dedicated Redis Stream:

`gohorizon:{application}:{environment}:events`

Use consumer groups for reliable ingestion.

Requirements:

- Event acknowledgement.
- Replay and recovery.
- Configurable retention.
- Duplicate event detection.
- Bounded buffering.
- Explicit overflow policy.

Telemetry failures must never block Laravel job execution indefinitely.

## 6.4 Capabilities command

Implement:

`php artisan gohorizon:capabilities --json`

Return:

- Laravel version.
- PHP version.
- Package version.
- Application identifier.
- Environment.
- Available queue drivers.
- Redis connection information, sanitized.
- Available lifecycle events.
- Supported administration operations.

GoHorizon must use capability detection instead of assuming every Laravel version exposes identical functionality.

## 6.5 Queue inspection

Implement a Laravel-side inspection interface.

Supported information:

- Available queues.
- Approximate pending jobs.
- Reserved jobs.
- Delayed jobs.
- Failed jobs.

Use framework abstractions wherever possible.

Redis-specific inspection must be isolated in a version-tested driver adapter.

Never make the Go service dependent on undocumented Redis key layouts without a compatibility layer.

---

# 7. Worker process supervision

## 7.1 Worker launch

Workers must execute the existing Artisan command.

Example:

```bash
php artisan queue:work redis \
    --queue=default \
    --sleep=3 \
    --tries=3 \
    --timeout=60 \
    --memory=256 \
    --max-jobs=1000 \
    --max-time=3600
```

Generate arguments from validated configuration.

Use argument arrays, not shell concatenation.

Set:

- Laravel application directory as working directory.
- Configured PHP executable.
- Correct environment variables.
- Explicit process identity.

## 7.2 Process tracking

Each worker record must contain:

- Application ID.
- Environment.
- Supervisor ID.
- Queue.
- PID.
- Unique worker UUID.
- Process start time.
- Last heartbeat.
- Desired state.
- Actual state.
- Restart count.
- Last exit code.
- Last exit reason.

Do not rely exclusively on PID because Linux can reuse process IDs.

## 7.3 Process states

Implement:

- Starting.
- Running.
- Stopping.
- Stopped.
- Crashed.
- Restarting.
- Unhealthy.

Transitions must be explicit and logged.

## 7.4 Graceful shutdown

1. Stop scheduling new work where supported.
2. Send the verified graceful termination signal.
3. Wait for the configured grace period.
4. Confirm process exit.
5. Escalate only according to explicit policy.

Do not use the global Laravel `queue:restart` signal to restart a single worker.

Use `--max-jobs` and `--max-time` as additional worker recycling mechanisms.

Verify SIGTERM behaviour in the exact target Laravel/PHP versions.

---

# 8. Configuration

Example `gohorizon.yaml`:

```yaml
version: 1

server:
  listen: "127.0.0.1:8090"
  metrics: true

storage:
  driver: sqlite
  path: /var/lib/gohorizon/gohorizon.db

redis:
  address: "127.0.0.1:6379"
  password_env: GOHORIZON_REDIS_PASSWORD
  db: 0
  tls: false

applications:
  - id: production
    name: My Laravel Application

    path: /var/www/application

    php: /usr/bin/php

    artisan: artisan

    environment: production

    redis_connection: default

    supervisors:
      - id: default-supervisor

        queues:
          - default
          - emails
          - notifications

        balance:
          strategy: fixed

        workers:
          min: 3
          max: 3

        worker:
          sleep: 3
          tries: 3
          timeout: 60
          memory: 256
          max_jobs: 1000
          max_time: 3600

        shutdown_grace: 120
        restart_backoff: 5
```

This is illustrative.

The implementation must validate every configuration value before launching workers.

Requirements:

- One Laravel application in v1.
- Application ID retained internally.
- Multiple queues.
- Multiple supervisor configurations.
- Environment variable expansion.
- Secret protection.
- Configuration validation.
- Reload without unnecessary restarts.

---

# 9. Worker balancing

Implement balancing progressively.

## Phase 1: Fixed workers

A defined number of workers per queue.

## Phase 2: Simple balancing

Distribute workers evenly between configured queues.

## Phase 3: Automatic balancing

Strategies:

- Size: pending job count.
- Time: estimated queue clearing time.
- Logarithmic: avoid disproportionate allocation to extremely large queues.

Example:

```yaml
balance:
  strategy: auto
  scaling_strategy: time

  min_processes: 1
  max_processes: 10

  max_shift: 1
  cooldown_seconds: 3
```

Prevent scaling oscillations using:

- Moving averages.
- Cooldown periods.
- Minimum observation intervals.
- Scale-up/scale-down hysteresis.
- Per-queue caps.

Ensure worker assignments actually allow queue-level balancing.

Do not launch every worker against an identical multi-queue list and claim dynamic queue allocation.

---

# 10. Metrics and monitoring

## Overview

- Pending jobs.
- Jobs processed.
- Failed jobs.
- Active workers.
- Worker utilization.
- Average execution time.
- Average queue wait time.
- Jobs per minute.

## Per queue

- Queue depth.
- Processing rate.
- Failure rate.
- Average wait time.
- Average runtime.
- Active workers.
- Desired workers.

## Per worker

- PID.
- Queue.
- Uptime.
- Jobs processed.
- Current state.
- Memory usage.
- CPU usage.
- Last restart.

## Job monitoring

- Job UUID.
- Class.
- Queue.
- Execution duration.
- Attempts.
- Status.
- Exception information.
- Timestamp.
- Tags.

## Historical metrics

Use SQLite initially.

Suggested retention:

| Data | Retention |
|---|---|
| Raw telemetry | 24–72 hours |
| Minute aggregates | 7 days |
| Hourly aggregates | 30 days |
| Daily aggregates | 12 months |

Make retention configurable.

Provide Prometheus metrics independently of the dashboard.

---

# 11. HTTP API

Base path:

`/api/v1`

| Method | Endpoint | Purpose |
|---|---|---|
| GET | /health | Service health |
| GET | /applications | List applications |
| GET | /applications/{id} | Application details |
| GET | /applications/{id}/queues | Queue information |
| GET | /applications/{id}/workers | Worker list |
| GET | /applications/{id}/metrics | Metrics |
| GET | /applications/{id}/jobs | Job history |
| GET | /applications/{id}/failed | Failed jobs |
| POST | /workers/{id}/restart | Restart worker |
| POST | /workers/{id}/stop | Stop worker |
| POST | /workers/{id}/start | Start worker |
| POST | /supervisors/{id}/pause | Pause supervisor |
| POST | /supervisors/{id}/resume | Resume supervisor |
| POST | /failed/{id}/retry | Retry failed job |
| DELETE | /failed/{id} | Delete failed job |

All framework-specific administrative operations must use the Laravel adapter.

Never manipulate Laravel queue data directly from Go.

Use pagination for job history and failed jobs.

Use JSON responses and appropriate HTTP status codes.

---

# 12. React dashboard

The dashboard is a core product component, not an optional development convenience.

## 12.1 Main navigation

- Overview.
- Queues.
- Workers.
- Jobs.
- Failed Jobs.
- Supervisors.
- Metrics.
- Settings.

## 12.2 Overview page

Display:

- Total jobs.
- Pending jobs.
- Failed jobs.
- Active workers.
- Processing rate.
- Queue wait time.
- Execution time.
- Recent activity.

## 12.3 Queues page

Display each queue with:

- Queue name.
- Pending count.
- Reserved count.
- Delayed count.
- Processing rate.
- Worker allocation.
- Failure rate.

## 12.4 Workers page

Display:

- Worker UUID.
- PID.
- Queue.
- Status.
- Uptime.
- Memory.
- CPU.
- Restart count.

Provide appropriate administrative actions.

## 12.5 Jobs page

Searchable and paginated job history.

Filters:

- Queue.
- Status.
- Job class.
- Date.
- Execution duration.

## 12.6 Failed jobs page

Provide:

- Exception details.
- Job class.
- Attempts.
- Failure timestamp.
- Queue.
- Retry action.
- Delete action.

Retry and deletion must use safe Laravel-side operations.

## 12.7 UI requirements

- Responsive design.
- Clear operational status indicators.
- Automatic refresh.
- Configurable refresh interval.
- No unnecessary full-page reloads.
- Loading and error states.
- Confirmation for destructive operations.
- Accessible controls.
- Consistent visual language.

Use REST initially.

Consider Server-Sent Events for real-time dashboard updates later.

Do not introduce WebSockets without a demonstrated need.

---

# 13. Security

Requirements:

- Bind to localhost by default.
- Authenticate administrative endpoints.
- Secure session handling.
- Reverse-proxy support.
- HTTPS deployment support.
- CSRF protection for cookie authentication.
- Role-based access.
- Protection against command injection.
- Restricted application paths.
- Restricted PHP executable paths.
- Sanitized exception messages.
- Credential redaction.
- Rate limiting.
- Audit logs.

Roles:

- Viewer.
- Operator.
- Administrator.

Run under a dedicated unprivileged Linux account.

Never accept arbitrary executable commands through the API.

---

# 14. Reliability and recovery

GoHorizon must handle:

- Redis disconnection.
- Laravel application downtime.
- PHP process crashes.
- Go service restarts.
- Host reboots.
- Configuration errors.
- Corrupted metrics data.
- Telemetry backlog.
- Duplicate lifecycle events.

Recovery rules:

1. Never delete jobs because telemetry is unavailable.
2. Never assume a worker is dead solely because a heartbeat is missing.
3. Reconcile actual processes with desired state after startup.
4. Never launch duplicate workers during recovery.
5. Use bounded retries and exponential backoff.
6. Make telemetry processing idempotent.
7. Treat queue depth as an observation, not an authoritative execution ledger.
8. Never allow a failed telemetry consumer to block worker recovery.

Use a single-instance ownership lock for v1.

If a second GoHorizon instance attempts to manage the same application, refuse to start the supervisor unless an explicitly supported failover mechanism is implemented.

---

# 15. Deployment

Initial deployment target: Linux systemd.

Directory layout:

```text
/opt/gohorizon/
/etc/gohorizon/gohorizon.yaml
/var/lib/gohorizon/
/var/log/gohorizon/
```

Provide:

- Go binary.
- Systemd unit.
- Configuration validation.
- Database migration command.
- Health-check command.
- Graceful service shutdown.
- Structured logs.
- Automatic startup after reboot.

The GoHorizon process must be independent of PHP-FPM and the Laravel web server.

The Laravel application must remain usable if GoHorizon is unavailable.

---

# 16. Testing strategy

## Unit tests

- Configuration parsing.
- Configuration validation.
- Worker state transitions.
- Balancing algorithms.
- Restart backoff.
- Metrics aggregation.
- Event deduplication.
- Authentication and authorization.

## Integration tests

Use actual Redis and a disposable Laravel 13 application.

Test:

1. Worker startup.
2. Successful job execution.
3. Failed job execution.
4. Job retry.
5. Delayed jobs.
6. Multiple queues.
7. Multiple workers.
8. Worker crash and recovery.
9. Graceful shutdown.
10. Go service restart.
11. Redis outage and recovery.
12. Laravel deployment and worker recycling.
13. Telemetry backlog and recovery.
14. Duplicate telemetry delivery.
15. Queue balancing under varying workloads.

## Critical safety tests

- No lost jobs during normal worker restarts.
- No duplicate worker spawning after service restart.
- No direct mutation of Laravel queue data by Go.
- No uncontrolled process creation.
- No indefinite shutdown wait.
- No interference with other Laravel workers.

Run:

- Go unit tests.
- Go race detector.
- PHP unit and integration tests.
- PHP static analysis.
- Frontend tests.
- End-to-end dashboard tests.

---

# 17. Implementation milestones

## Milestone 1 — Proof of concept

**Priority: Highest**

Deliver:

- Go service.
- Redis connectivity.
- Laravel package skeleton.
- Application registration.
- Queue discovery.
- Worker launch.
- Process monitoring.
- Basic telemetry.
- Basic operational status endpoint.

Acceptance criteria:

- GoHorizon starts an existing Laravel worker.
- It observes successful jobs.
- It observes failed jobs.
- It identifies worker crashes.
- It safely restarts workers.
- Existing Laravel jobs require no changes.
- Telemetry failures do not affect job execution.
- GoHorizon can restart without duplicating workers.

Do not proceed until these conditions are met.

## Milestone 2 — React dashboard and monitoring

Deliver:

- React + TypeScript + Vite dashboard.
- Embedded frontend assets.
- Overview.
- Queue monitoring.
- Worker monitoring.
- Job history.
- Failed jobs.
- Historical metrics.
- Prometheus endpoint.

## Milestone 3 — Production hardening

Deliver:

- Authentication.
- Authorization.
- Audit logs.
- Graceful worker management.
- Multiple supervisors.
- Configuration reload.
- Restart policies.
- Resource limits.
- Systemd deployment.

## Milestone 4 — Automatic balancing

Deliver:

- Simple balancing.
- Automatic scaling.
- Size-based scaling.
- Time-based scaling.
- Logarithmic scaling.
- Cooldown and hysteresis.
- Global resource limits.

**Do not implement automatic balancing before worker lifecycle management and graceful recovery have passed integration tests.**

## Milestone 5 — Advanced functionality

Deliver:

- Job tagging.
- Batch visibility.
- Notifications.
- Alerting.
- Advanced job administration.
- API integrations.
- PostgreSQL persistence option.
- Multi-application capability.

Multi-application support is not required for the initial release.

---

# 18. Compatibility requirements

Initial supported environment:

- Laravel 13.
- PHP 8.3+.
- Redis 7+.
- Linux systemd.
- Current supported Go release.
- Node.js LTS for frontend builds.

The Laravel package must isolate framework-specific code behind interfaces.

The Go service must not require Laravel Horizon.

Redis Cluster support must not be assumed. Document the supported Redis topology and test it explicitly.

Future Laravel version support must be implemented through capability detection and compatibility tests.

---

# 19. Documentation deliverables

The repository must include:

- README.md
- INSTALLATION.md
- CONFIGURATION.md
- ARCHITECTURE.md
- API.md
- SECURITY.md
- OPERATIONS.md
- TROUBLESHOOTING.md
- CONTRIBUTING.md

Include a reproducible Laravel test fixture.

Document every framework-specific assumption.

---

# 20. Definition of done

The initial production release is acceptable only when:

- A Laravel 13 application can register with GoHorizon.
- Existing Redis queues remain compatible.
- Workers can be managed independently of the Laravel web server.
- Jobs execute through Laravel without behavioural changes.
- Job lifecycle telemetry is accurate.
- Worker crashes are recovered automatically.
- Graceful restarts are tested.
- Queue metrics are available.
- React dashboard is functional.
- Production frontend assets are embedded in Go.
- API authentication is enforced.
- Deployment documentation is complete.
- Integration tests pass.
- No known job-loss or duplicate-worker safety issue remains.

---

# 21. Instructions to the coding agent

Before implementing:

1. Inspect Laravel 13 queue internals.
2. Inspect Laravel 13 queue events and their payloads.
3. Verify worker signal-handling behaviour.
4. Inspect the existing Redis queue implementation.
5. Identify statistics available through supported Laravel APIs.
6. Identify operations requiring a Laravel-side adapter.
7. Document framework-specific assumptions.
8. Produce architecture decision records for deviations.

Do not silently change the architectural direction.

Do not implement the entire project in one large development pass.

Start with Milestone 1.

Provide:

- Repository structure.
- Architecture decision records.
- Working Go service.
- Working Laravel package.
- Integration tests.
- Demonstration of safe worker lifecycle management.

Only after the proof of concept passes should implementation proceed to the dashboard and subsequent milestones.

## Final architectural directive

**GoHorizon is an independent Go-based control plane for existing Laravel queue infrastructure.**

Laravel remains responsible for job execution. Redis remains the queue transport. GoHorizon provides monitoring, process supervision, balancing and operational management.

The most important acceptance criterion is that GoHorizon must never compromise the integrity or execution semantics of existing Laravel jobs.

Build for one application initially, but design the internal architecture so that multiple applications can be supported later without a fundamental rewrite.

**Build incrementally. Prove worker safety first. Add intelligence second.**