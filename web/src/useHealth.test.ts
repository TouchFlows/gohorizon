import { describe, expect, it } from 'vitest'
import { applyHealth, type HealthState } from './useHealth'
import type { HealthReport } from './api'

const report = {
  status: 'ok',
  version: '0.1.0-m1',
  uptime_seconds: 3,
  lock: { held: true, degraded: false },
  telemetry: { events: 0, duplicates: 0, invalid: 0, errors: 0, failing: false },
  queues: { depths: [] },
  supervisors: [],
} satisfies HealthReport

const empty: HealthState = { report: null, degraded: false, error: null }

describe('applyHealth', () => {
  it('replaces the empty state with the first answer', () => {
    expect(applyHealth(empty, { kind: 'report', report, degraded: false })).toEqual({
      report,
      degraded: false,
      error: null,
    })
  })

  // A blank screen is the one thing the dashboard must not do on a bad start: the shell has an
  // error to render, and no report is invented to render beside it.
  it('records a first failure without inventing a report', () => {
    expect(applyHealth(empty, { kind: 'failure', message: '/api/v1/health returned 500' })).toEqual({
      report: null,
      degraded: false,
      error: '/api/v1/health returned 500',
    })
  })

  // A blip must not blank a screen an operator is watching, so the report and its 503 flag
  // survive the failed poll and only the reason is added.
  it('keeps the report a failed poll could not refresh', () => {
    const loaded = applyHealth(empty, { kind: 'report', report, degraded: true })
    expect(applyHealth(loaded, { kind: 'failure', message: 'fetch failed' })).toEqual({
      report,
      degraded: true,
      error: 'fetch failed',
    })
  })

  it('clears the error when a poll answers again', () => {
    const failed = applyHealth(empty, { kind: 'failure', message: 'fetch failed' })
    expect(applyHealth(failed, { kind: 'report', report, degraded: false })).toEqual({
      report,
      degraded: false,
      error: null,
    })
  })
})
