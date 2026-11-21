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
    report.telemetry !== null
  )
}
