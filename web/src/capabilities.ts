import type { Capabilities } from './api'

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
    reason: retry || remove ? '' : unsupported(capabilities),
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
  return { restart, reason: restart ? '' : unsupported(capabilities) }
}

// unsupported is the service's own answer, so a control that is missing says why rather than
// only disappearing.
function unsupported(capabilities: Capabilities): string {
  const error = capabilities.error ?? ''
  return error === ''
    ? 'Read-only: the application does not report support for this action.'
    : `Read-only: ${error}`
}
