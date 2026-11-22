import { describe, expect, it } from 'vitest'
import type { Bucket, History } from './api'
import { buildChart, buildHistoryQuery, defaultRange, jobCount, ranges } from './history'

function history(overrides: Partial<History> = {}): History {
  return {
    queue: 'default',
    resolution: 'minute',
    from: '2026-11-21T12:00:00Z',
    to: '2026-11-21T12:05:00Z',
    buckets: [],
    ...overrides,
  }
}

function bucket(start: string, counts: Partial<Bucket> = {}): Bucket {
  return {
    start,
    processed: 0,
    released: 0,
    failed: 0,
    timed_out: 0,
    duration_ms: 0,
    ...counts,
  }
}

describe('buildChart', () => {
  it('fills the quiet periods between two buckets instead of closing the gap', () => {
    const model = buildChart(
      history({
        buckets: [bucket('2026-11-21T12:00:00Z', { processed: 3 }), bucket('2026-11-21T12:03:00Z', { failed: 1 })],
      }),
    )

    expect(model.buckets.map((entry) => entry.start)).toEqual([
      Date.parse('2026-11-21T12:00:00Z') / 1000,
      Date.parse('2026-11-21T12:01:00Z') / 1000,
      Date.parse('2026-11-21T12:02:00Z') / 1000,
      Date.parse('2026-11-21T12:03:00Z') / 1000,
      Date.parse('2026-11-21T12:04:00Z') / 1000,
    ])
    expect(model.buckets[0].total).toBe(3)
    expect(model.buckets[1]).toMatchObject({ processed: 0, failed: 0, total: 0 })
    expect(model.buckets[3]).toMatchObject({ failed: 1, total: 1 })
    expect(model.max).toBe(3)
    expect(model.total).toBe(4)
    expect(model.empty).toBe(false)
  })

  it('draws a single bucket in the same window as any other', () => {
    const model = buildChart(
      history({ buckets: [bucket('2026-11-21T12:01:00Z', { processed: 2, timed_out: 1 })] }),
    )

    expect(model.buckets).toHaveLength(5)
    expect(model.buckets[1]).toMatchObject({ processed: 2, timedOut: 1, total: 3 })
    expect(model.buckets.filter((entry) => entry.total > 0)).toHaveLength(1)
  })

  it('keeps a scale for a window of zeros', () => {
    const model = buildChart(history({ buckets: [bucket('2026-11-21T12:00:00Z')] }))

    expect(model.total).toBe(0)
    expect(model.max).toBe(1)
    expect(model.empty).toBe(false)
  })

  it('reports a window with no rows as empty rather than all zeros', () => {
    const model = buildChart(history({ buckets: [] }))

    expect(model.empty).toBe(true)
    expect(model.buckets.every((entry) => entry.total === 0)).toBe(true)
  })

  it('uses the resolution step for hours', () => {
    const model = buildChart(
      history({
        resolution: 'hourly',
        from: '2026-11-21T09:20:00Z',
        to: '2026-11-21T12:10:00Z',
        buckets: [bucket('2026-11-21T10:00:00Z', { processed: 7 })],
      }),
    )

    expect(model.buckets).toHaveLength(4)
    expect(model.buckets[0].start).toBe(Date.parse('2026-11-21T09:00:00Z') / 1000)
    expect(model.buckets[1]).toMatchObject({ processed: 7, total: 7 })
  })
})

describe('buildHistoryQuery', () => {
  it('mirrors the endpoint parameters and states the window explicitly', () => {
    const now = Date.parse('2026-11-21T12:00:00Z')
    const query = buildHistoryQuery('emails', 'hourly', 24, now)
    const parameters = new URLSearchParams(query)

    expect(parameters.get('queue')).toBe('emails')
    expect(parameters.get('resolution')).toBe('hourly')
    expect(parameters.get('to')).toBe('2026-11-21T12:00:00.000Z')
    expect(parameters.get('from')).toBe('2026-11-20T12:00:00.000Z')
  })

  it('encodes a queue name rather than splitting the query', () => {
    const parameters = new URLSearchParams(buildHistoryQuery('a&b=c', 'minute', 0.5, 0))
    expect(parameters.get('queue')).toBe('a&b=c')
    expect(parameters.get('resolution')).toBe('minute')
  })
})

describe('ranges', () => {
  it('defaults to the window the endpoint itself uses per resolution', () => {
    expect(defaultRange('minute').hours).toBe(1)
    expect(defaultRange('hourly').hours).toBe(24)
    expect(defaultRange('daily').hours).toBe(720)
  })

  it('offers only windows the resolution can draw', () => {
    for (const resolution of ['minute', 'hourly', 'daily'] as const) {
      const largest = ranges[resolution][ranges[resolution].length - 1]
      const stepHours = resolution === 'minute' ? 1 / 60 : resolution === 'hourly' ? 1 : 24
      expect(largest.hours / stepHours).toBeLessThanOrEqual(720)
    }
  })
})

describe('jobCount', () => {
  // The browser smoke always sees two attempts, so the singular caption has no other cover.
  it('words a one-job total in the singular, which is what the caption prints', () => {
    expect(jobCount(1)).toBe('1 job')
  })

  it('keeps the plural for no jobs and for a column worth more than one', () => {
    expect(jobCount(0)).toBe('0 jobs')
    expect(jobCount(2)).toBe('2 jobs')
  })
})

describe('buildChart duration', () => {
  it('averages a bucket duration over the attempts that ran in it', () => {
    const model = buildChart(
      history({
        buckets: [
          bucket('2026-11-21T12:00:00Z', { processed: 2, failed: 1, duration_ms: 90 }),
          bucket('2026-11-21T12:01:00Z', { failed: 1, duration_ms: 10 }),
        ],
      }),
    )

    expect(model.buckets[0]).toMatchObject({ durationMs: 90, averageMs: 30, total: 3 })
    expect(model.buckets[1]).toMatchObject({ durationMs: 10, averageMs: 10 })
    expect(model.maxAverageMs).toBe(30)
  })

  it('leaves a quiet minute without an average instead of claiming instant jobs', () => {
    const model = buildChart(
      history({ buckets: [bucket('2026-11-21T12:00:00Z', { processed: 1, duration_ms: 5 })] }),
    )

    expect(model.buckets[1]).toMatchObject({ total: 0, durationMs: 0, averageMs: 0 })
  })

  it('rounds to the millisecond, because the chart labels whole ones', () => {
    const model = buildChart(
      history({ buckets: [bucket('2026-11-21T12:00:00Z', { processed: 3, duration_ms: 10 })] }),
    )

    expect(model.buckets[0].averageMs).toBe(3)
  })

  it('scales the duration chart at one millisecond when there is nothing to draw', () => {
    expect(buildChart(history()).maxAverageMs).toBe(1)
  })
})
