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

// The JSON /api/v1/capabilities returns: what the application said about itself at startup, and
// which operations this service can offer given that answer. `available` is false when the
// application could not answer or answered a schema this service does not understand; `error`
// then says which, and `operations` still answers what works (restarting a worker is this
// service's own action, so it survives an application that cannot answer at all).
export interface CapabilityOperations {
  inspect_queues: boolean
  telemetry: boolean
  failed_jobs_list: boolean
  failed_job_retry: boolean
  failed_job_delete: boolean
  restart_workers: boolean
}

// One queue connection as the descriptor reports it. retry_after is null when the driver has
// no reservation window, which is not a zero-second window.
export interface CapabilityConnection {
  driver?: string
  retry_after: number | null
}

export interface Capabilities {
  available: boolean
  schema?: number
  package?: string
  package_version?: string
  laravel?: string
  php?: string
  application?: string
  environment?: string
  default_connection?: string
  connections?: Record<string, CapabilityConnection>
  pcntl?: boolean
  telemetry_stream?: string
  events?: string[]
  // The versions this service reads of the application's queue-inspection and failed-list
  // payloads; each read refuses one that declares another, so a package upgrade is visible here
  // rather than misread.
  payload_schemas?: { inspect: number; failed_jobs: number }
  operations: CapabilityOperations
  // Why a withheld operation is withheld, in the service's own words. A false entry without one
  // would be a control that disappears without saying why.
  operation_reasons?: Partial<Record<keyof CapabilityOperations, string>>
  error?: string
}

export async function fetchCapabilities(signal?: AbortSignal): Promise<Capabilities> {
  const response = await fetch('/api/v1/capabilities', {
    signal,
    headers: { Accept: 'application/json' },
    cache: 'no-store',
  })
  if (response.status === 401) {
    throw new Unauthorized()
  }
  if (response.status !== 200) {
    throw new Error(`/api/v1/capabilities returned ${response.status}`)
  }
  const payload: unknown = await response.json()
  if (!isCapabilities(payload)) {
    throw new Error('/api/v1/capabilities did not return a capability descriptor')
  }
  return payload
}

// isCapabilities checks the fields the controls are gated on. Every operation is required: a
// missing one would read as "not supported" and hide a control the service does offer.
export function isCapabilities(value: unknown): value is Capabilities {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const capabilities = value as Partial<Capabilities>
  if (typeof capabilities.available !== 'boolean') {
    return false
  }
  if (typeof capabilities.operations !== 'object' || capabilities.operations === null) {
    return false
  }
  const operations = capabilities.operations as unknown as Record<string, unknown>
  return (
    ['inspect_queues', 'telemetry', 'failed_jobs_list', 'failed_job_retry', 'failed_job_delete', 'restart_workers'] as const
  ).every((name) => typeof operations[name] === 'boolean')
}

// Unauthorized is the 401 the API answers when the request carried no session, or one that
// has ended: a sign-out, an expiry, or a user the configuration no longer has. It is not a
// failed read, so it is its own error: the shell can tell it apart from a blip and ask for a
// sign-in instead of reporting a failure the operator cannot do anything about.
export class Unauthorized extends Error {
  constructor() {
    super('authentication required')
    this.name = 'Unauthorized'
  }
}

// Session is who the service says this browser is.
export interface Session {
  user: string
  expires_at: string
}

// SessionLookup is what /api/v1/session answers before the dashboard draws anything. 'open'
// is a service started without users: there is nobody to sign in as, and the pages are drawn
// exactly as they were before authentication existed.
export type SessionLookup =
  | { kind: 'signed-in'; user: string }
  | { kind: 'signed-out' }
  | { kind: 'open' }

export async function readSession(signal?: AbortSignal): Promise<SessionLookup> {
  const response = await fetch('/api/v1/session', {
    signal,
    headers: { Accept: 'application/json' },
    cache: 'no-store',
  })
  if (response.status === 401) {
    return { kind: 'signed-out' }
  }
  if (response.status === 404) {
    return { kind: 'open' }
  }
  if (response.status !== 200) {
    throw new Error(`/api/v1/session returned ${response.status}`)
  }
  const payload: unknown = await response.json()
  if (!isSession(payload)) {
    throw new Error('/api/v1/session did not return a session')
  }
  return { kind: 'signed-in', user: payload.user }
}

// startSession exchanges a name and password for a session cookie. The refusals carry the
// wording the form shows; the service does not say whether the name or the password was
// wrong, so neither does this.
export async function startSession(name: string, password: string): Promise<Session> {
  const response = await fetch('/api/v1/session', {
    method: 'POST',
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, password }),
  })
  if (response.status === 401) {
    throw new Error('That name and password do not match.')
  }
  if (response.status === 429) {
    throw new Error('Too many attempts. Wait a minute and try again.')
  }
  if (response.status !== 200) {
    throw new Error(`/api/v1/session returned ${response.status}`)
  }
  const payload: unknown = await response.json()
  if (!isSession(payload)) {
    throw new Error('/api/v1/session did not return a session')
  }
  return payload
}

// endSession clears the cookie and revokes the token it carried. A failure is swallowed by
// the caller's next session read, which is what decides where the browser goes next.
export async function endSession(): Promise<void> {
  await fetch('/api/v1/session', { method: 'DELETE', headers: { Accept: 'application/json' } })
}

export function isSession(value: unknown): value is Session {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const session = value as Partial<Session>
  return (
    typeof session.user === 'string' &&
    session.user !== '' &&
    typeof session.expires_at === 'string'
  )
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
  if (response.status === 401) {
    throw new Unauthorized()
  }
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
  if (response.status === 401) {
    throw new Unauthorized()
  }
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
      typeof (bucket as Bucket).timed_out === 'number' &&
      typeof (bucket as Bucket).duration_ms === 'number',
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
  if (response.status === 401) {
    throw new Unauthorized()
  }
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

// One record of the application's own failed store, as /api/v1/failed returns it. The payload
// never leaves the application: `job` is the class Laravel recorded and `exception` is the
// redacted first line of the stored trace, never null. `id` is the record's own id — a row id
// or, with the file failer, the job uuid — and `uuid` is the identity retry and delete take.
export interface FailedJob {
  id: string
  uuid: string
  connection: string
  queue: string
  job: string
  failed_at: string
  exception: string
}

export interface FailedJobPage {
  failed_jobs: FailedJob[]
  // next_before is an opaque cursor, empty on the last page.
  next_before: string
}

// FailedJobsUnavailable is the 503 the service answers when the application could not list
// its failed store. It is not an empty store, and the screen must not draw it as one.
export class FailedJobsUnavailable extends Error {
  constructor() {
    super('failed jobs are unavailable')
    this.name = 'FailedJobsUnavailable'
  }
}

export async function fetchFailedJobs(query: string, signal?: AbortSignal): Promise<FailedJobPage> {
  const response = await fetch(`/api/v1/failed?${query}`, {
    signal,
    headers: { Accept: 'application/json' },
  })
  if (response.status === 401) {
    throw new Unauthorized()
  }
  if (response.status === 503) {
    throw new FailedJobsUnavailable()
  }
  if (response.status !== 200) {
    throw new Error(`/api/v1/failed returned ${response.status}`)
  }
  const payload: unknown = await response.json()
  if (!isFailedJobPage(payload)) {
    throw new Error('/api/v1/failed did not return a page of failed jobs')
  }
  return payload
}

function isFailedJob(value: unknown): value is FailedJob {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const job = value as Partial<FailedJob>
  // Every field is required, exception included: an absent exception would otherwise read as
  // a record that threw nothing, and the class and uuid are what the row and the two actions
  // take.
  return (
    typeof job.id === 'string' &&
    typeof job.uuid === 'string' &&
    typeof job.connection === 'string' &&
    typeof job.queue === 'string' &&
    typeof job.job === 'string' &&
    typeof job.failed_at === 'string' &&
    typeof job.exception === 'string'
  )
}

export function isFailedJobPage(value: unknown): value is FailedJobPage {
  if (typeof value !== 'object' || value === null) {
    return false
  }
  const page = value as Partial<FailedJobPage>
  return (
    typeof page.next_before === 'string' &&
    Array.isArray(page.failed_jobs) &&
    page.failed_jobs.every(isFailedJob)
  )
}

// The double-submit token lives in a cookie the page is deliberately allowed to read
// (docs/adr/0011).
const csrfCookieName = 'gohorizon_csrf'

// readCSRFToken reads the token a mutation must echo. A jar without one answers with an empty
// token, and the service refuses the request rather than the page inventing a value.
function readCSRFToken(): string {
  const prefix = `${csrfCookieName}=`
  for (const part of document.cookie.split(';')) {
    const entry = part.trim()
    if (entry.startsWith(prefix)) {
      return decodeURIComponent(entry.slice(prefix.length))
    }
  }
  return ''
}

// AdminActionError is an administrative mutation the service refused or could not carry out,
// carrying the status that said so. A 404 is the record the action named no longer being
// there, which is an answer about the record rather than a fault in the request, so a caller
// can tell it apart.
export class AdminActionError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'AdminActionError'
    this.status = status
  }
}

// restartWorker asks the supervisor to stop one worker gracefully and start its replacement in
// the same slot, so the job in flight finishes instead of being killed (docs/adr/0002). The
// service answers 202 with the worker and its supervisor, both of which the row already
// shows, so only the answer's status is read.
export async function restartWorker(uuid: string): Promise<void> {
  await adminAction('POST', `/api/v1/workers/${encodeURIComponent(uuid)}/restart`)
}

// retryFailedJob pushes one failed job back onto its queue through the application's own
// `queue:retry` (docs/adr/0012). The service answers 202 because the work is queued.
export async function retryFailedJob(uuid: string): Promise<string> {
  return failedJobAction('POST', `/api/v1/failed/${encodeURIComponent(uuid)}/retry`)
}

// deleteFailedJob removes one failed job through the application's own `queue:forget`. The
// service answers 200 because the record is already gone when it does.
export async function deleteFailedJob(uuid: string): Promise<string> {
  return failedJobAction('DELETE', `/api/v1/failed/${encodeURIComponent(uuid)}`)
}

// failedJobAction answers with the queue the job belongs to — the attempt's own id is already
// on screen — so a retry can say where the work landed. The name is detail: history may have
// been pruned or disabled, and the service then answers with an empty one, which is a done
// action rather than a failed request.
async function failedJobAction(method: string, url: string): Promise<string> {
  const answer = (await adminAction(method, url)) as { queue?: unknown }
  return typeof answer.queue === 'string' ? answer.queue : ''
}

// adminAction is the one shape every administrative mutation takes: the token from the cookie
// repeated in the header, on a request the browser also sends its session cookie with, and a
// refusal that carries the service's own `error` wording.
async function adminAction(method: string, url: string): Promise<unknown> {
  const response = await fetch(url, {
    method,
    headers: { Accept: 'application/json', 'X-CSRF-Token': readCSRFToken() },
  })
  if (response.status === 401) {
    throw new Unauthorized()
  }
  if (response.status !== 200 && response.status !== 202) {
    throw new AdminActionError(response.status, await errorText(response))
  }
  return response.json()
}

async function errorText(response: Response): Promise<string> {
  try {
    const payload = (await response.json()) as { error?: unknown }
    if (typeof payload.error === 'string' && payload.error !== '') {
      return payload.error
    }
  } catch {
    // A refusal without a JSON body is still a refusal; its status is the answer.
  }
  return `the service answered ${response.status}`
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
