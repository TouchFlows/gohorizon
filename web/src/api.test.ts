import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  AdminActionError,
  Unauthorized,
  deleteFailedJob,
  isCapabilities,
  isFailedJobPage,
  isHealthReport,
  isHistory,
  isJobPage,
  rereadCapabilities,
  retryFailedJob,
} from './api'

const report = {
  status: 'ok',
  version: '0.1.0-m1',
  uptime_seconds: 17,
  lock: { held: true, degraded: false },
  telemetry: { events: 0, duplicates: 0, invalid: 0, errors: 0, failing: false },
  queues: { depths: [] },
  supervisors: [],
}

const history = {
  queue: 'default',
  resolution: 'minute',
  from: '2026-11-21T12:00:00Z',
  to: '2026-11-21T12:05:00Z',
  buckets: [{ start: '2026-11-21T12:00:00Z', processed: 1, released: 0, failed: 0, timed_out: 0, duration_ms: 120 }],
}

describe('isHistory', () => {
  it('accepts a history the chart can draw', () => {
    expect(isHistory(history)).toBe(true)
    expect(isHistory({ ...history, buckets: [] })).toBe(true)
  })

  it('rejects a bucket without the counters a column needs', () => {
    expect(isHistory({ ...history, buckets: [{ start: '2026-11-21T12:00:00Z', processed: 1 }] })).toBe(false)
    expect(isHistory({ ...history, resolution: 'weekly' })).toBe(false)
    expect(isHistory({ ...history, buckets: null })).toBe(false)
  })

  it('rejects a bucket whose duration the screen would draw as NaN', () => {
    expect(
      isHistory({
        ...history,
        buckets: [
          { start: '2026-11-21T12:00:00Z', processed: 1, released: 0, failed: 0, timed_out: 0 },
        ],
      }),
    ).toBe(false)
  })
})

const jobPage = {
  jobs: [
    {
      id: '1774191600000-0',
      job_id: '7',
      uuid: '',
      class: 'App\\Jobs\\Marker',
      queue: 'default',
      connection: 'default',
      outcome: 'failed',
      attempts: 3,
      duration_ms: 120,
      finished_at: '2026-11-21T20:40:00Z',
      exception: { class: 'RuntimeException', message: 'boom' },
    },
  ],
  next_before: '',
}

describe('isJobPage', () => {
  it('accepts a page whose attempt failed and one whose attempt did not', () => {
    expect(isJobPage(jobPage)).toBe(true)
    expect(isJobPage({ jobs: [{ ...jobPage.jobs[0], exception: null }], next_before: 'c1' })).toBe(true)
    expect(isJobPage({ jobs: [], next_before: '' })).toBe(true)
  })

  it('rejects an attempt that omits the exception instead of saying there was none', () => {
    const withoutException: Record<string, unknown> = { ...jobPage.jobs[0] }
    delete withoutException.exception

    expect(isJobPage({ jobs: [withoutException], next_before: '' })).toBe(false)
  })

  it('rejects a page or attempt missing what the table reads', () => {
    expect(isJobPage({ jobs: jobPage.jobs })).toBe(false)
    expect(isJobPage({ ...jobPage, jobs: [{ ...jobPage.jobs[0], outcome: 7 }] })).toBe(false)
    expect(isJobPage({ ...jobPage, jobs: [{ ...jobPage.jobs[0], exception: { class: 'E' } }] })).toBe(false)
    expect(isJobPage(null)).toBe(false)
  })

  it('rejects an attempt without the identity the detail panel prints', () => {
    const withoutUuid: Record<string, unknown> = { ...jobPage.jobs[0] }
    delete withoutUuid.uuid
    const withoutJobID: Record<string, unknown> = { ...jobPage.jobs[0] }
    delete withoutJobID.job_id

    expect(isJobPage({ jobs: [withoutUuid], next_before: '' })).toBe(false)
    expect(isJobPage({ jobs: [withoutJobID], next_before: '' })).toBe(false)
  })
})

const failedPage = {
  failed_jobs: [
    {
      id: 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d',
      uuid: 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d',
      connection: 'redis',
      queue: 'default',
      job: 'App\\Jobs\\AlwaysFails',
      failed_at: '2026-11-26 09:30:00',
      exception: 'RuntimeException: boom',
    },
  ],
  next_before: '',
}

describe('isFailedJobPage', () => {
  it('accepts a page of the application failed store', () => {
    expect(isFailedJobPage(failedPage)).toBe(true)
    expect(isFailedJobPage({ failed_jobs: [], next_before: 'cursor' })).toBe(true)
  })

  it('rejects a record missing what the row or the actions take', () => {
    for (const field of ['id', 'uuid', 'queue', 'job', 'failed_at', 'exception']) {
      const without: Record<string, unknown> = { ...failedPage.failed_jobs[0] }
      delete without[field]
      expect(isFailedJobPage({ failed_jobs: [without], next_before: '' }), field).toBe(false)
    }
  })

  it('rejects a page without its cursor or record list', () => {
    expect(isFailedJobPage({ failed_jobs: failedPage.failed_jobs })).toBe(false)
    expect(isFailedJobPage({ ...failedPage, failed_jobs: null })).toBe(false)
    expect(isFailedJobPage(null)).toBe(false)
  })
})

// failed-job answers, as the service sends them: the queue is detail history supplies, so a
// pruned attempt or a deployment with history disabled answers with an empty or absent name.
function failedJobAnswer(status: number, body: unknown): void {
  vi.stubGlobal('fetch', vi.fn(async () => ({ status, json: async () => body }) as Response))
}

describe('failed job actions', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('resolves a retry whose answer names no queue', async () => {
    // readCSRFToken reads the cookie, so the action needs a document under test.
    vi.stubGlobal('document', { cookie: '' })
    failedJobAnswer(202, { job: 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d', queue: '' })

    await expect(retryFailedJob('e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d')).resolves.toBe('')
  })

  it('resolves a delete whose answer carries no queue field', async () => {
    vi.stubGlobal('document', { cookie: '' })
    failedJobAnswer(200, { job: 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d' })

    await expect(deleteFailedJob('e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d')).resolves.toBe('')
  })

  it('still answers with the queue when the service named one', async () => {
    vi.stubGlobal('document', { cookie: '' })
    failedJobAnswer(202, { job: 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d', queue: 'default' })

    await expect(retryFailedJob('e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d')).resolves.toBe('default')
  })
})

describe('isCapabilities', () => {
  const operations = {
    inspect_queues: true,
    telemetry: true,
    failed_jobs_list: true,
    failed_job_retry: false,
    failed_job_delete: false,
    restart_workers: true,
  }

  it('accepts a descriptor that says which operations work', () => {
    expect(isCapabilities({ available: false, operations, error: 'the package is not installed' })).toBe(true)
  })

  it('rejects a descriptor missing an operation the controls are gated on', () => {
    const withoutRestart: Record<string, unknown> = { ...operations }
    delete withoutRestart.restart_workers

    expect(isCapabilities({ available: true, operations: withoutRestart })).toBe(false)
    expect(isCapabilities({ available: true, operations: { ...operations, failed_job_retry: 'no' } })).toBe(false)
    expect(isCapabilities({ operations })).toBe(false)
    expect(isCapabilities(null)).toBe(false)
  })
})

describe('isHealthReport', () => {
  it('accepts a report the screens can render', () => {
    expect(isHealthReport(report)).toBe(true)
  })

  it('rejects a payload missing a section a screen reads', () => {
    const withoutQueues: Record<string, unknown> = { ...report }
    delete withoutQueues.queues

    expect(isHealthReport(withoutQueues)).toBe(false)
    expect(isHealthReport({ ...report, telemetry: null })).toBe(false)
    expect(isHealthReport({ ...report, lock: null })).toBe(false)
    expect(isHealthReport({ ...report, uptime_seconds: '17' })).toBe(false)
  })

  it('rejects what is not an object', () => {
    expect(isHealthReport(null)).toBe(false)
    expect(isHealthReport('ok')).toBe(false)
  })
})

// rereadAnswer stubs the one request the control makes and answers it with this status and
// body, so the test reads the call the page sends as well as what it does with the reply.
function rereadAnswer(status: number, body: unknown) {
  const fetchMock = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => {
    return { status, json: async () => body } as Response
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const rereadDescriptor = {
  available: true,
  schema: 1,
  package: 'gohorizon/laravel',
  operations: {
    inspect_queues: true,
    telemetry: true,
    failed_jobs_list: true,
    failed_job_retry: true,
    failed_job_delete: true,
    restart_workers: true,
  },
}

describe('rereadCapabilities', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('asks for a read now, carrying the token a mutation has to echo', async () => {
    vi.stubGlobal('document', { cookie: 'gohorizon_csrf=token-1' })
    const fetchMock = rereadAnswer(200, rereadDescriptor)

    await expect(rereadCapabilities()).resolves.toMatchObject({ available: true })

    const call = fetchMock.mock.calls[0]
    expect(call[0]).toBe('/api/v1/capabilities/refresh')
    expect(call[1]?.method).toBe('POST')
    const headers = (call[1]?.headers ?? {}) as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('token-1')
  })

  it('rejects an answer that is not a descriptor instead of publishing it', async () => {
    // A refresh that answered something else would otherwise replace the descriptor the controls
    // are gated on with fields that are not there.
    vi.stubGlobal('document', { cookie: '' })
    rereadAnswer(200, { available: true })

    await expect(rereadCapabilities()).rejects.toThrow(
      '/api/v1/capabilities/refresh did not return a capability descriptor',
    )
  })

  it("carries the service's refusal wording and the status that said it", async () => {
    vi.stubGlobal('document', { cookie: '' })
    rereadAnswer(403, { error: 'administrator role required' })

    const failure = rereadCapabilities()
    await expect(failure).rejects.toBeInstanceOf(AdminActionError)
    await expect(failure).rejects.toMatchObject({
      status: 403,
      message: 'administrator role required',
    })
  })

  it('reports an ended session as the 401 it is', async () => {
    vi.stubGlobal('document', { cookie: '' })
    rereadAnswer(401, { error: 'authentication required' })

    await expect(rereadCapabilities()).rejects.toBeInstanceOf(Unauthorized)
  })
})
