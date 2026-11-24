import { describe, expect, it, vi } from 'vitest'
import { applyHealth, pollHealth, type HealthState } from './useHealth'
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

// The interval the hook promises, spelled out here rather than imported: a page that changed
// it should fail this test instead of silently moving it.
const interval = 5000

// HealthEvent is what the loop reports, taken from the decision it feeds so the two cannot
// drift apart.
type HealthEvent = Parameters<typeof applyHealth>[1]

// deferred is a request the test settles when it chooses, which is how a poll is still in
// flight at the moment the component goes away.
function deferred<T>(): {
  promise: Promise<T>
  resolve: (value: T) => void
  reject: (reason: unknown) => void
} {
  let resolve: (value: T) => void = () => {}
  let reject: (reason: unknown) => void = () => {}
  const promise = new Promise<T>((settle, fail) => {
    resolve = settle
    reject = fail
  })
  return { promise, resolve, reject }
}

describe('applyHealth', () => {
  it('replaces the empty state with the first answer', () => {
    expect(applyHealth(empty, { kind: 'report', report, degraded: false })).toEqual({
      report,
      degraded: false,
      error: null,
    })
  })

  it('records a first failure without inventing a report', () => {
    expect(applyHealth(empty, { kind: 'failure', message: '/api/v1/health returned 500' })).toEqual({
      report: null,
      degraded: false,
      error: '/api/v1/health returned 500',
    })
  })

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

describe('pollHealth', () => {
  it('asks once on start and again when the interval elapses', async () => {
    vi.useFakeTimers()
    const fetcher = vi.fn(async () => ({ report, degraded: false }))
    const events: HealthEvent[] = []
    const stop = pollHealth(fetcher, (event) => events.push(event))
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(fetcher).toHaveBeenCalledTimes(1)
      expect(events).toEqual([{ kind: 'report', report, degraded: false }])

      await vi.advanceTimersByTimeAsync(interval - 1)
      expect(fetcher).toHaveBeenCalledTimes(1)

      await vi.advanceTimersByTimeAsync(1)
      expect(fetcher).toHaveBeenCalledTimes(2)
    } finally {
      stop()
      vi.useRealTimers()
    }
  })

  it('aborts the request in flight and asks nothing more once stopped', async () => {
    vi.useFakeTimers()
    const pending = deferred<{ report: HealthReport; degraded: boolean }>()
    let asked: AbortSignal | undefined
    const fetcher = vi.fn((signal: AbortSignal) => {
      asked = signal
      return pending.promise
    })
    const events: HealthEvent[] = []
    const stop = pollHealth(fetcher, (event) => events.push(event))
    try {
      expect(fetcher).toHaveBeenCalledTimes(1)

      stop()
      expect(asked?.aborted).toBe(true)

      // The request the component had already made fails after it is gone. The failure is
      // dropped rather than recorded: there is no screen left for it to be old on.
      pending.reject(new Error('The operation was aborted.'))
      await vi.advanceTimersByTimeAsync(0)
      expect(events).toEqual([])

      await vi.advanceTimersByTimeAsync(interval * 3)
      expect(fetcher).toHaveBeenCalledTimes(1)
      expect(events).toEqual([])
    } finally {
      vi.useRealTimers()
    }
  })

  it('arms no further poll when the answer lands after the stop', async () => {
    vi.useFakeTimers()
    const pending = deferred<{ report: HealthReport; degraded: boolean }>()
    const fetcher = vi.fn(() => pending.promise)
    const events: HealthEvent[] = []
    const stop = pollHealth(fetcher, (event) => events.push(event))
    try {
      expect(fetcher).toHaveBeenCalledTimes(1)

      stop()
      // A request that had already arrived when the component went away: the answer is
      // reported, since it is not a failure and there is nothing to abort, but the loop
      // must not arm another poll behind it.
      pending.resolve({ report, degraded: false })
      await vi.advanceTimersByTimeAsync(0)
      expect(events).toEqual([{ kind: 'report', report, degraded: false }])

      await vi.advanceTimersByTimeAsync(interval * 3)
      expect(fetcher).toHaveBeenCalledTimes(1)
      expect(events).toHaveLength(1)
    } finally {
      vi.useRealTimers()
    }
  })
})
