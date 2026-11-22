// Everything a screen shows is built here, away from React, so it can be tested without a
// browser. A count the driver could not report becomes "unknown": zero is a real answer and
// saying it for an unanswered question would be a lie.
import type { HealthReport, QueueDepth, Queues, Supervisor, Worker } from './api'

export function count(value: number | null | undefined): string {
  return typeof value === 'number' ? String(value) : 'unknown'
}

// countNoun words a tally of a noun that pluralizes with -s: one is singular, everything
// else — zero included — takes the plural, so a lone worker reads "1 worker" and not
// "1 workers".
export function countNoun(value: number, noun: string): string {
  return `${value} ${noun}${value === 1 ? '' : 's'}`
}

// formatSpan renders a duration in whole seconds the way an operator reads it: seconds,
// then minutes, then hours and minutes, then days and hours.
export function formatSpan(seconds: number): string {
  if (seconds < 60) {
    return `${seconds}s`
  }
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) {
    return `${minutes}m`
  }
  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return `${hours}h ${minutes % 60}m`
  }
  const days = Math.floor(hours / 24)
  const remaining = hours % 24
  return remaining === 0 ? `${days}d` : `${days}d ${remaining}h`
}

// formatAge reports how long something has been waiting. It takes the browser's clock, not
// the service's, so the two only have to agree on unix seconds.
export function formatAge(atSeconds: number | null, nowMilliseconds: number): string {
  if (atSeconds === null) {
    return 'unknown'
  }
  const age = Math.max(0, Math.floor(nowMilliseconds / 1000) - atSeconds)
  return age < 5 ? 'just now' : formatSpan(age)
}

export interface TelemetryRow {
  label: string
  value: string
}

export interface QueueRow {
  queue: string
  pending: string
  reserved: string
  delayed: string
  oldest: string
  error: string | null
}

export interface OverviewModel {
  status: string
  healthy: boolean
  application: string
  version: string
  uptime: string
  lock: { ok: boolean; label: string; detail: string }
  telemetry: TelemetryRow[]
  telemetryFailure: string | null
  supervisors: { count: number; running: number; desired: number; crashed: number; crashLooping: number }
  queues: QueueRow[]
  queueError: string | null
}

export function queueRows(depths: QueueDepth[] | null, nowMilliseconds: number): QueueRow[] {
  return (depths ?? []).map((depth) => ({
    queue: depth.queue,
    pending: count(depth.pending),
    reserved: count(depth.reserved),
    delayed: count(depth.delayed),
    oldest: formatAge(depth.oldest_pending_at, nowMilliseconds),
    error: depth.oldest_pending_error === undefined || depth.oldest_pending_error === '' ? null : depth.oldest_pending_error,
  }))
}

// queueError is why the inspection failed, or nothing when it answered. Both pages that
// show queues need the same answer, so they ask here instead of repeating the test.
export function queueError(queues: Queues): string | null {
  return queues.error === undefined || queues.error === '' ? null : queues.error
}

export function crashLooping(supervisors: Supervisor[] | null): number {
  return (supervisors ?? []).reduce(
    (total, supervisor) => total + (supervisor.workers ?? []).filter((worker) => worker.crash_looping).length,
    0,
  )
}

export function buildOverview(report: HealthReport, nowMilliseconds: number): OverviewModel {
  const supervisors = report.supervisors ?? []
  const lock = report.lock
  return {
    status: report.status,
    healthy: report.status === 'ok',
    application: [report.application, report.environment].filter(Boolean).join(' / '),
    version: report.version,
    uptime: formatSpan(report.uptime_seconds),
    lock: {
      ok: lock.held,
      label: lock.held ? 'held' : 'lost',
      detail: lock.last_error === undefined || lock.last_error === '' ? '' : lock.last_error,
    },
    telemetry: [
      { label: 'events', value: String(report.telemetry.events) },
      { label: 'duplicates', value: String(report.telemetry.duplicates) },
      { label: 'invalid', value: String(report.telemetry.invalid) },
      { label: 'errors', value: String(report.telemetry.errors) },
    ],
    telemetryFailure: report.telemetry.failing
      ? (report.telemetry.last_error ?? 'ingestion is failing right now')
      : null,
    supervisors: {
      count: supervisors.length,
      running: supervisors.reduce((total, supervisor) => total + supervisor.running, 0),
      desired: supervisors.reduce((total, supervisor) => total + supervisor.desired, 0),
      crashed: supervisors.reduce((total, supervisor) => total + supervisor.crashed, 0),
      crashLooping: crashLooping(supervisors),
    },
    queues: queueRows(report.queues.depths, nowMilliseconds),
    queueError: queueError(report.queues),
  }
}

export interface WorkerRow {
  uuid: string
  queue: string
  pid: number
  state: string
  restarts: number
  lastExit: string
  reason: string
  crashLooping: boolean
  adopted: boolean
}

export interface SupervisorRows {
  id: string
  running: number
  desired: number
  crashed: number
  escalations: number
  workers: WorkerRow[]
}

function workerRow(worker: Worker): WorkerRow {
  return {
    uuid: worker.uuid,
    queue: worker.queue,
    pid: worker.pid,
    state: worker.state,
    restarts: worker.restarts,
    lastExit: String(worker.last_exit_code),
    reason: worker.last_exit_reason ?? '',
    crashLooping: worker.crash_looping,
    adopted: worker.adopted,
  }
}

export function buildWorkers(report: HealthReport): SupervisorRows[] {
  return (report.supervisors ?? []).map((supervisor) => ({
    id: supervisor.id,
    running: supervisor.running,
    desired: supervisor.desired,
    crashed: supervisor.crashed,
    escalations: supervisor.escalations,
    workers: (supervisor.workers ?? []).map(workerRow),
  }))
}
