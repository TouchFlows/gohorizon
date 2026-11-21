import { describe, expect, it } from 'vitest'
import { isHealthReport } from './api'

const report = {
  status: 'ok',
  version: '0.1.0-m1',
  uptime_seconds: 17,
  lock: { held: true, degraded: false },
  telemetry: { events: 0, duplicates: 0, invalid: 0, errors: 0, failing: false },
  queues: { depths: [] },
  supervisors: [],
}

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
