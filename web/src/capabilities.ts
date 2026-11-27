import type { Capabilities, CapabilityOperations } from './api'

// The controls each screen offers, and why one is missing. A capabilities read that has not
// answered (null) leaves the controls as they were: a transient failure must not hide a
// control the service does offer, because the service refuses an action it cannot take on its
// own (docs/adr/0014).

export interface FailedJobControls {
  retry: boolean
  delete: boolean
  reason: string
}

export interface WorkerControls {
  restart: boolean
  reason: string
}

const noUsers = 'Read-only: this service was started without users, so it has no administrative routes.'

// failedJobControls gates the two controls on the application's descriptor: both are its own
// `queue:retry` and `queue:forget`, so a deployment that cannot answer offers neither.
export function failedJobControls(capabilities: Capabilities | null, canAct: boolean): FailedJobControls {
  if (!canAct) {
    return { retry: false, delete: false, reason: noUsers }
  }
  if (capabilities === null) {
    return { retry: true, delete: true, reason: '' }
  }
  const retry = capabilities.operations.failed_job_retry
  const remove = capabilities.operations.failed_job_delete
  return {
    retry,
    delete: remove,
    reason: retry || remove ? '' : unsupported(capabilities, 'failed_job_retry'),
  }
}

// workerControls gates the restart button. Restarting is the supervisor's own action, so it
// outlives an application that cannot answer; the descriptor still decides, so a deployment
// that later supervises workers elsewhere needs no new screen.
export function workerControls(capabilities: Capabilities | null, canAct: boolean): WorkerControls {
  if (!canAct) {
    return { restart: false, reason: noUsers }
  }
  if (capabilities === null) {
    return { restart: true, reason: '' }
  }
  const restart = capabilities.operations.restart_workers
  return { restart, reason: restart ? '' : unsupported(capabilities, 'restart_workers') }
}

// readOnlyFor is how long a read-only deployment has been read-only, in the words a banner uses.
// Minutes are enough up to an hour and hours up to a day: the exact instant is `read_only_since`,
// and a banner that ticked by the second would be noise on a screen that already refreshes. An
// instant from the future reads as the shortest age rather than a negative one.
export function readOnlyFor(since: number, now: number = Math.floor(Date.now() / 1000)): string {
  const seconds = now - since
  if (seconds < 60) {
    return 'less than a minute'
  }
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) {
    return minutes === 1 ? '1 minute' : `${minutes} minutes`
  }
  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return hours === 1 ? '1 hour' : `${hours} hours`
  }
  const days = Math.floor(hours / 24)
  return days === 1 ? '1 day' : `${days} days`
}

// mayReread is who the re-read control is drawn for: asking the application to account for
// itself again starts a command on its host, so it is the administrator's, which is the role the
// service requires of the call. A service started without users has nobody holding it.
export function mayReread(role: string | null): boolean {
  return role === 'administrator'
}

// readResult is what a re-read found, in the words the control shows beside the banner. A
// descriptor that is still unreadable is an answer about the deployment rather than a failure of
// the call, so it repeats the reason the banner is already carrying rather than a status.
export function readResult(capabilities: Capabilities): string {
  if (!capabilities.available) {
    const reason = capabilities.error ?? ''
    return reason === '' ? 'Still read-only.' : `Still read-only: ${reason}`
  }
  const schema = capabilities.schema ?? 0
  const packageName = [capabilities.package, capabilities.package_version]
    .filter((part) => part !== undefined && part !== '')
    .join(' ')
  return packageName === ''
    ? `Readable: schema ${schema}`
    : `Readable: schema ${schema}, package ${packageName}`
}

// unsupported is the service's own answer, so a control that is missing says why rather than
// only disappearing. The operation's own reason comes first: it names the fact that withheld it
// (no telemetry stream, no inspectable connection), where the descriptor's error only describes
// a deployment that could not be read at all. The age is added only when there is one: a readable
// descriptor that withholds one screen publishes no instant, so that banner says why and nothing
// else (docs/adr/0014).
function unsupported(capabilities: Capabilities, operation: keyof CapabilityOperations): string {
  const reason = capabilities.operation_reasons?.[operation] ?? capabilities.error ?? ''
  const detail =
    reason === '' ? 'the application does not report support for this action.' : reason
  const since = capabilities.read_only_since ?? 0
  return since > 0 ? `Read-only for ${readOnlyFor(since)}: ${detail}` : `Read-only: ${detail}`
}
