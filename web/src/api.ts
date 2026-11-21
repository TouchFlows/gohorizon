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

// One finished attempt as /api/v1/jobs returns it. Exception is null for an attempt that
// threw nothing — never an empty object — which the table must show as "none", not blank.
export interface JobException {
  class: string
  message: string
}

export interface Job {
  id: string
  job_id: string
  uuid: string
  class: string
  queue: string
  connection: string
  outcome: string
  attempts: number
  duration_ms: number
  finished_at: string
  exception: JobException | null
}

export interface JobPage {
  jobs: Job[]
  // next_before is empty on the last page; otherwise it is the cursor for the page after.
  next_before: string
}

export async function fetchJobs(query: string, signal?: AbortSignal): Promise<JobPage> {
  const response = await fetch(`/api/v1/jobs?${query}`, {
    signal,
    headers: { Accept: 'application/json' },
  })
  if (response.status === 503) {
    throw new HistoryUnavailable()
  }
  if (response.status !== 200) {
    throw new Error(`/api/v1/jobs returned ${response.status}`)
  }
  const payload: unknown = await response.json()
  if (!isJobPage(payload)) {
    throw new Error('/api/v1/jobs did not return a page of jobs')
  }
  return payload
}

function isJob(value: unknown): value is Job {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const job = value as Partial<Job>
  const exceptionOk =
    job.exception === null ||
    (typeof job.exception === 'object' &&
      job.exception !== null &&
      typeof job.exception.class === 'string' &&
      typeof job.exception.message === 'string')
  return (
    typeof job.id === 'string' &&
    typeof job.class === 'string' &&
    // The detail panel prints these two, and an absent uuid is not the same as a job that
    // published none: the panel would show a blank row instead of "none published".
    typeof job.job_id === 'string' &&
    typeof job.uuid === 'string' &&
    typeof job.queue === 'string' &&
    typeof job.outcome === 'string' &&
    typeof job.attempts === 'number' &&
    typeof job.duration_ms === 'number' &&
    typeof job.finished_at === 'string' &&
    exceptionOk
  )
}

export function isJobPage(value: unknown): value is JobPage {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const page = value as Partial<JobPage>
  return typeof page.next_before === 'string' && Array.isArray(page.jobs) && page.jobs.every(isJob)
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
