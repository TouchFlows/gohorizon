// The JSON /api/v1/health returns. The dashboard reads it defensively: an optional field
// the service omits stays optional here, and a list the service could not fill arrives as
// null rather than [], which the view model turns into "unknown" instead of zero.
export interface HealthReport {
  status: string
  version: string
  application?: string
  environment?: string
  uptime_seconds: number
  lock: LockState
  telemetry: Telemetry
  queues: Queues
  supervisors: Supervisor[] | null
}

export interface LockState {
  held: boolean
  degraded: boolean
  last_error?: string
}

export interface Telemetry {
  events: number
  duplicates: number
  invalid: number
  errors: number
  failing: boolean
  last_error?: string
}

export interface Queues {
  connection?: string
  error?: string
  depths: QueueDepth[] | null
}

export interface QueueDepth {
  queue: string
  pending: number | null
  reserved: number | null
  delayed: number | null
  oldest_pending_at: number | null
  oldest_pending_error?: string
}

export interface Supervisor {
  id: string
  desired: number
  running: number
  stopping: number
  crashed: number
  adopted: number
  escalations: number
  workers: Worker[] | null
}

export interface Worker {
  uuid: string
  queue: string
  pid: number
  state: string
  restarts: number
  last_exit_code: number
  last_exit_reason?: string
  adopted: boolean
  crash_looping: boolean
}

// fetchHealth reads one report. A 503 is not an error: the service answers 503 when its
// lock is degraded and the body still says why, which is exactly the state the dashboard
// exists to show.
export async function fetchHealth(
  signal?: AbortSignal,
): Promise<{ report: HealthReport; degraded: boolean }> {
  const response = await fetch('/api/v1/health', {
    signal,
    headers: { Accept: 'application/json' },
  })
  if (response.status !== 200 && response.status !== 503) {
    throw new Error(`/api/v1/health returned ${response.status}`)
  }
  const payload: unknown = await response.json()
  if (!isHealthReport(payload)) {
    throw new Error('/api/v1/health did not return a health report')
  }
  return { report: payload, degraded: response.status === 503 }
}

// The JSON /api/v1/metrics returns: one queue's aggregates for [from, to) at a resolution.
// An empty window is an empty buckets list, never an invented zero row.
export type Resolution = 'minute' | 'hourly' | 'daily'

export interface Bucket {
  start: string
  processed: number
  released: number
  failed: number
  timed_out: number
  duration_ms: number
}

export interface History {
  queue: string
  resolution: Resolution
  from: string
  to: string
  buckets: Bucket[]
}

// HistoryUnavailable is the 503 the service answers when its metrics database was never
// opened. The page shows it as "history is unavailable" and keeps working around it.
export class HistoryUnavailable extends Error {
  constructor() {
    super('history is unavailable')
    this.name = 'HistoryUnavailable'
  }
}

export async function fetchHistory(query: string, signal?: AbortSignal): Promise<History> {
  const response = await fetch(`/api/v1/metrics?${query}`, {
    signal,
    headers: { Accept: 'application/json' },
  })
  if (response.status === 503) {
    throw new HistoryUnavailable()
  }
  if (response.status !== 200) {
    throw new Error(`/api/v1/metrics returned ${response.status}`)
  }
  const payload: unknown = await response.json()
  if (!isHistory(payload)) {
    throw new Error('/api/v1/metrics did not return a history')
  }
  return payload
}

// isHistory checks what the chart cannot draw without: a resolution it knows, a window and
// a bucket list. A bucket missing a counter would draw a wrong column, so those are
// checked too.
export function isHistory(value: unknown): value is History {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const history = value as Partial<History>
  if (typeof history.queue !== 'string' || typeof history.from !== 'string' || typeof history.to !== 'string') {
    return false
  }
  if (history.resolution !== 'minute' && history.resolution !== 'hourly' && history.resolution !== 'daily') {
    return false
  }
  if (!Array.isArray(history.buckets)) {
    return false
  }
  return history.buckets.every(
    (bucket: unknown) =>
      typeof bucket === 'object' &&
      bucket !== null &&
      typeof (bucket as Bucket).start === 'string' &&
      typeof (bucket as Bucket).processed === 'number' &&
      typeof (bucket as Bucket).released === 'number' &&
      typeof (bucket as Bucket).failed === 'number' &&
      typeof (bucket as Bucket).timed_out === 'number',
  )
}

// isHealthReport checks the fields a screen cannot render without. The rest of the report is
// read defensively, so a report that grows a field never breaks the dashboard.
export function isHealthReport(value: unknown): value is HealthReport {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const report = value as Partial<HealthReport>
  return (
    typeof report.status === 'string' &&
    typeof report.uptime_seconds === 'number' &&
    typeof report.lock === 'object' &&
    report.lock !== null &&
    typeof report.telemetry === 'object' &&
    report.telemetry !== null &&
    // The overview reads this before it renders a single queue, so a payload without it
    // would blank the screen rather than show an empty table.
    typeof report.queues === 'object' &&
    report.queues !== null
  )
}
