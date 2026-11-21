import { describe, expect, it } from 'vitest'
import type { HealthReport } from './api'
import { buildOverview, buildWorkers, count, formatAge, formatSpan, queueError, queueRows } from './view'

function report(overrides: Partial<HealthReport> = {}): HealthReport {
  return {
    status: 'ok',
    version: '0.4.0',
    application: 'shop',
    environment: 'production',
    uptime_seconds: 3600,
    lock: { held: true, degraded: false },
    telemetry: { events: 12, duplicates: 1, invalid: 0, errors: 0, failing: false },
    queues: { connection: 'redis', depths: [] },
    supervisors: [],
    ...overrides,
  }
}

describe('count', () => {
  it('renders a reported zero as zero', () => {
    expect(count(0)).toBe('0')
    expect(count(7)).toBe('7')
  })

  it('renders a count nobody reported as unknown', () => {
    expect(count(null)).toBe('unknown')
    expect(count(undefined)).toBe('unknown')
  })
})

describe('formatSpan', () => {
  it('reads durations the way an operator does', () => {
    expect(formatSpan(45)).toBe('45s')
    expect(formatSpan(600)).toBe('10m')
    expect(formatSpan(9000)).toBe('2h 30m')
    expect(formatSpan(2 * 86400)).toBe('2d')
    expect(formatSpan(2 * 86400 + 5 * 3600)).toBe('2d 5h')
  })
})

describe('formatAge', () => {
  const now = 1_763_000_000_000

  it('reports a missing timestamp as unknown, not as zero', () => {
    expect(formatAge(null, now)).toBe('unknown')
  })

  it('measures against the browser clock', () => {
    expect(formatAge(Math.floor(now / 1000) - 90, now)).toBe('1m')
    expect(formatAge(Math.floor(now / 1000) - 3, now)).toBe('just now')
  })

  it('never reports a negative age for a clock that runs behind', () => {
    expect(formatAge(Math.floor(now / 1000) + 30, now)).toBe('just now')
  })
})

describe('queueRows', () => {
  it('keeps the driver error and turns unreported counts into unknown', () => {
    const rows = queueRows(
      [
        {
          queue: 'default',
          pending: null,
          reserved: 0,
          delayed: null,
          oldest_pending_at: null,
          oldest_pending_error: 'RedisException',
        },
      ],
      1_763_000_000_000,
    )

    expect(rows).toEqual([
      {
        queue: 'default',
        pending: 'unknown',
        reserved: '0',
        delayed: 'unknown',
        oldest: 'unknown',
        error: 'RedisException',
      },
    ])
  })

  it('treats a missing depth list as no queues', () => {
    expect(queueRows(null, 0)).toEqual([])
  })
})

describe('queueError', () => {
  it('reports why inspection failed', () => {
    expect(queueError({ error: 'goHorizon:inspect failed', depths: null })).toBe('goHorizon:inspect failed')
  })

  it('reports nothing when the inspection answered', () => {
    expect(queueError({ depths: [] })).toBeNull()
    expect(queueError({ error: '', depths: [] })).toBeNull()
  })
})

describe('buildOverview', () => {
  it('summarises a healthy report', () => {
    const model = buildOverview(report({ supervisors: [] }), 0)

    expect(model.healthy).toBe(true)
    expect(model.status).toBe('ok')
    expect(model.application).toBe('shop / production')
    expect(model.uptime).toBe('1h 0m')
    expect(model.lock).toEqual({ ok: true, label: 'held', detail: '' })
    expect(model.telemetry).toEqual([
      { label: 'events', value: '12' },
      { label: 'duplicates', value: '1' },
      { label: 'invalid', value: '0' },
      { label: 'errors', value: '0' },
    ])
    expect(model.telemetryFailure).toBeNull()
  })

  it('counts a crash loop on a worker, including across supervisors', () => {
    const model = buildOverview(
      report({
        supervisors: [
          {
            id: 'one',
            desired: 2,
            running: 1,
            stopping: 0,
            crashed: 1,
            adopted: 0,
            escalations: 1,
            workers: [{ uuid: 'a', queue: 'default', pid: 10, state: 'crashed', restarts: 4, last_exit_code: 12, crash_looping: true, adopted: false }],
          },
          {
            id: 'two',
            desired: 1,
            running: 1,
            stopping: 0,
            crashed: 0,
            adopted: 0,
            escalations: 0,
            workers: [{ uuid: 'b', queue: 'emails', pid: 11, state: 'running', restarts: 0, last_exit_code: 0, crash_looping: true, adopted: false }],
          },
        ],
      }),
      0,
    )

    expect(model.supervisors).toEqual({ count: 2, running: 2, desired: 3, crashed: 1, crashLooping: 2 })
  })

  it('reports a lost lock and a failing ingestion without hiding them', () => {
    const model = buildOverview(
      report({
        status: 'degraded',
        lock: { held: false, degraded: true, last_error: 'lock renewal failed' },
        telemetry: { events: 1, duplicates: 0, invalid: 2, errors: 3, failing: true, last_error: 'redis is down' },
      }),
      0,
    )

    expect(model.healthy).toBe(false)
    expect(model.lock).toEqual({ ok: false, label: 'lost', detail: 'lock renewal failed' })
    expect(model.telemetryFailure).toBe('redis is down')
  })

  it('surfaces a failed inspection instead of an empty queue list that looks healthy', () => {
    const model = buildOverview(
      report({ queues: { error: 'goHorizon:inspect failed', depths: null } }),
      0,
    )

    expect(model.queueError).toBe('goHorizon:inspect failed')
    expect(model.queues).toEqual([])
  })
})

describe('buildWorkers', () => {
  it('maps workers with their badges and last exit', () => {
    const rows = buildWorkers(
      report({
        supervisors: [
          {
            id: 'default',
            desired: 2,
            running: 2,
            stopping: 0,
            crashed: 1,
            adopted: 1,
            escalations: 0,
            workers: [
              { uuid: 'a', queue: 'default', pid: 10, state: 'crashed', restarts: 4, last_exit_code: 12, last_exit_reason: 'timeout', crash_looping: true, adopted: false },
              { uuid: 'b', queue: 'emails', pid: 11, state: 'running', restarts: 0, last_exit_code: 0, adopted: true, crash_looping: false },
            ],
          },
        ],
      }),
    )

    expect(rows).toHaveLength(1)
    expect(rows[0].id).toBe('default')
    expect(rows[0].workers[0]).toMatchObject({ uuid: 'a', state: 'crashed', reason: 'timeout', crashLooping: true })
    expect(rows[0].workers[1]).toMatchObject({ uuid: 'b', adopted: true, reason: '', lastExit: '0' })
  })

  it('reports no supervisor rather than inventing one', () => {
    expect(buildWorkers(report({ supervisors: null }))).toEqual([])
  })
})
