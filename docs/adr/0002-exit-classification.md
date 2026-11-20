# ADR 0002 — Worker exit classification and restart policy

**Status:** accepted (Milestone 1)
**Enforcement:** items 1–4 and 6 are implemented; item 5 is **pending**.
**Refines:** handoff §7.3 (process states) and §7.4 (graceful shutdown / escalation).

## Context

The handoff models a crashed worker as the failure state and assumes the supervisor can
decide restarts from process signals and a grace period.

Laravel 13's worker exits for reasons that are not failures:

| Exit | Reason (`WorkerStopReason`) | Correct reaction |
|---|---|---|
| 0 | `interrupted` | we asked it to stop — do not restart |
| 0 | `restart_signal` (`queue:restart` during deploy) | restart after the deploy window |
| 0 | `max_jobs`, `max_time`, `empty`, `empty_for` | recycle immediately (Laravel's Supervisor model) |
| 12 | `memory` | recycle immediately |
| 1 | error, or `timed_out` | restart with backoff, and flag the timeout separately |
| signal | e.g. SIGKILL from an OOM killer | restart with backoff, count as crash |

The reason enum only exists inside the worker; the exit status alone cannot separate
"recycled" from "deploy restart". Also, `--timeout` makes the worker SIGKILL itself
(`posix_kill(getmypid(), SIGKILL)`), so a timeout is indistinguishable from an external
kill unless the adapter reports it.

## Decision

1. Each worker instance stores `LastExitCode` and `LastExitReason`; state transitions
   are explicit and logged (`starting → running → stopping → stopped`, `running →
   crashed → restarting → starting`).
2. Exit status is classified:
   - requested stop → `stopped`, never restarted.
   - `0` or `12` → `stopped` with reason `recycled`; replacement starts after
     `restart_backoff`, without counting against the crash budget.
   - anything else → `crashed`; replacement starts after exponential backoff
     (`restart_backoff * 2^(n-1)`, capped at 60s), and `RestartCount` increments.
3. Backoff resets after the worker has stayed up for `2 * restart_backoff + 30s`.
4. Graceful stop sends SIGTERM to the worker process, waits `shutdown_grace`, then
   SIGKILLs the process group. Escalation is logged and recorded on the record.
   Laravel may need up to one job duration or one `--sleep` to honour SIGTERM, so the
   grace period must exceed `timeout + sleep`; configuration validation enforces
   `shutdown_grace > timeout + sleep`.
5. **Pending enforcement (telemetry ingestion).** When the package reports a
   `WorkerStopping` reason for a worker UUID, that reason overrides exit-code
   inference. Nothing consumes that reason yet — the Go telemetry consumer is not
   written — so today `exit 1` is reported as `error` and never silently as
   `timed_out`. This is the remaining piece of the ADR.
6. **Enforced at supervisor startup.** `pcntl` is a hard prerequisite for a
   configured `--timeout`, and `--timeout` must stay below the queue connection's
   `retry_after`. `internal/adapter` runs `gohorizon:capabilities --json` before any
   worker exists, and `supervisor.New` refuses to start (no process spawned) when the
   report contradicts the configuration. Scope: the check needs the package to
   answer. If the command is unavailable (package absent, artisan failing) the
   preflight is skipped with a logged warning and supervision continues, because
   ADR 0001 degrades adapter features instead of blocking workers. A report that
   answers with an unknown capabilities schema is refused, not guessed.

## Consequences

- An unsafe timeout configuration fails at startup with the reason and the fix,
  instead of producing workers whose jobs are retried while still running.
- Memory-limit and deploy restarts no longer look like crashes in the dashboard.
- A crash loop is visible: `RestartCount`, backoff growth, and `LastExitReason`.
- The supervisor never restarts the whole pool because one worker exited; each slot is
  independent (handoff §2.7 preserved).
