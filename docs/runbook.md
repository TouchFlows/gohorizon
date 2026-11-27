# Runbook

One entry per alert in `deployments/prometheus/gohorizon.rules.yml`, in the order they can fire.
Each section says what the state means and what to do about it; the alert's `runbook` annotation
links to the section by its heading. `internal/api/prometheus_rules_test.go` checks that every
alert has a section here, so an alert cannot ship without its instruction.

Every capability entry is answered from the same three places, which is why the rules need no
service restart to be believed: `capabilities.error` in `GET /api/v1/health` (the reason in the
words a refused action returns), `operation_reasons` in `GET /api/v1/capabilities` (which screen is
withheld and by what fact), and the `capability-change` audit lines in the service log (when the
state changed and why it was then).

## GoHorizonCapabilitiesReadOnly

**Means:** the application's capability descriptor cannot be read, so the dashboard offers no administrative action, restart included — a fact, not a stopped service.
**Do:** read `capabilities.error` for the reason, then run `php artisan gohorizon:capabilities --json` in the application: a missing or unreadable package needs `composer require gohorizon/laravel`, a schema number this service does not understand needs one side upgraded, and a failed command needs the error it printed.

## GoHorizonCapabilitiesReadOnlyTooLong

**Means:** the deployment has been read-only for more than 30 minutes — the age comes from `read_only_since`, so every control has been withheld since then and nobody repaired it.
**Do:** treat it as an outage of the dashboard rather than of the queue (the workers are still supervised), and fix the descriptor as for `GoHorizonCapabilitiesReadOnly`; the age resets the moment a re-read answers, so no restart is needed to confirm the repair.

## GoHorizonCapabilitiesContradiction

**Means:** the application's current answer contradicts the running configuration — a `retry_after` below a worker's `timeout`, or a `timeout` without `pcntl` — which is the launch preflight refusing a configuration that is already running.
**Do:** read `capabilities.contradiction` for the sentence a refused launch would have printed, raise `retry_after` above `timeout` (or `timeout` below it) in the application's `config/queue.php`, and restart the supervisors on your own schedule: the service publishes this contradiction and never stops healthy workers over it.

## GoHorizonCapabilitiesLoopStale

**Means:** the re-read loop got an answer at some point and has not got one for more than two intervals, so every other capability sample describes an answer that may be minutes old — a deployment that has never answered at all is read-only instead, and `GoHorizonCapabilitiesReadOnly` is the alert for it.
**Do:** check the service log for the re-read's failure (`application capabilities re-read failed`) and for whether the loop is still running at all, then confirm with `gohorizon_capabilities_last_success_timestamp_seconds` that a newer instant appears — a loop that stopped needs a service restart, a PHP command that hangs needs the application fixed.
