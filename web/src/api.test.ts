import { describe, expect, it } from 'vitest'
import { isHealthReport, isHistory } from './api'

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
