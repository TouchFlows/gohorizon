import { describe, expect, it, vi } from 'vitest'
import { applyCapabilities, pollCapabilities, type CapabilityEvent } from './useCapabilities'
import { Unauthorized } from './api'
import type { Capabilities } from './api'

const operations = {
  inspect_queues: true,
  telemetry: true,
  failed_jobs_list: true,
  failed_job_retry: true,
  failed_job_delete: true,
  restart_workers: true,
}

const readable: Capabilities = {
  available: true,
  schema: 1,
  package: 'gohorizon/laravel',
  operations,
}

// What a deployment that cannot describe itself publishes: every operation withheld, and the
// reason in the service's own words.
const unreadable: Capabilities = {
  available: false,
  operations: {
    inspect_queues: false,
    telemetry: false,
    failed_jobs_list: false,
    failed_job_retry: false,
    failed_job_delete: false,
    restart_workers: false,
  },
  error: 'the application reports capabilities schema 2; this GoHorizon understands schema 1',
}

// The interval the hook promises, spelled out here rather than imported: a page that changed it
// should fail this test instead of silently moving it.
const interval = 5000

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

describe('applyCapabilities', () => {
  it('replaces the empty state with the first answer', () => {
    expect(applyCapabilities(null, { kind: 'descriptor', capabilities: unreadable })).toEqual(unreadable)
  })

  it('keeps the descriptor a failed refresh could not replace', () => {
    // A deployment that was read-only stays read-only through a blip: falling back to null
    // would put controls back that the service would refuse.
    expect(applyCapabilities(unreadable, { kind: 'failure' })).toEqual(unreadable)
  })

  it('stays empty when the first read fails', () => {
    expect(applyCapabilities(null, { kind: 'failure' })).toBeNull()
  })

  it('takes the repaired descriptor a later refresh brings back', () => {
    expect(applyCapabilities(unreadable, { kind: 'descriptor', capabilities: readable })).toEqual(readable)
  })
})

describe('pollCapabilities', () => {
  it('asks once on start and again when the interval elapses', async () => {
    vi.useFakeTimers()
    const fetcher = vi.fn(async () => readable)
    const events: CapabilityEvent[] = []
    const stop = pollCapabilities(fetcher, (event) => events.push(event))
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(fetcher).toHaveBeenCalledTimes(1)
      expect(events).toEqual([{ kind: 'descriptor', capabilities: readable }])

      await vi.advanceTimersByTimeAsync(interval - 1)
      expect(fetcher).toHaveBeenCalledTimes(1)

      await vi.advanceTimersByTimeAsync(1)
      expect(fetcher).toHaveBeenCalledTimes(2)
    } finally {
      stop()
      vi.useRealTimers()
    }
  })

  it('reports the repaired descriptor a later refresh brings back', async () => {
    // The whole point of refreshing: the service that answers after a restart is a different
    // snapshot, and the controls have to follow it without the page being reloaded.
    vi.useFakeTimers()
    const answers = [unreadable, readable]
    const fetcher = vi.fn(async () => answers.shift() ?? readable)
    const events: CapabilityEvent[] = []
    const stop = pollCapabilities(fetcher, (event) => events.push(event))
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(events).toEqual([{ kind: 'descriptor', capabilities: unreadable }])

      await vi.advanceTimersByTimeAsync(interval)
      expect(events).toEqual([
        { kind: 'descriptor', capabilities: unreadable },
        { kind: 'descriptor', capabilities: readable },
      ])
    } finally {
      stop()
      vi.useRealTimers()
    }
  })

  it('reports a failed refresh and keeps asking', async () => {
    vi.useFakeTimers()
    const fetcher = vi
      .fn<(signal: AbortSignal) => Promise<Capabilities>>()
      .mockRejectedValueOnce(new Error('fetch failed'))
      .mockResolvedValue(readable)
    const events: CapabilityEvent[] = []
    const stop = pollCapabilities(fetcher, (event) => events.push(event))
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(events).toEqual([{ kind: 'failure' }])

      // A failure is not the end of the loop: the next tick asks again.
      await vi.advanceTimersByTimeAsync(interval)
      expect(events).toEqual([{ kind: 'failure' }, { kind: 'descriptor', capabilities: readable }])
    } finally {
      stop()
      vi.useRealTimers()
    }
  })

  it('ends the session on a 401 and asks nothing more', async () => {
    vi.useFakeTimers()
    const fetcher = vi.fn(async (): Promise<Capabilities> => {
      throw new Unauthorized()
    })
    const events: CapabilityEvent[] = []
    const expired = vi.fn()
    const stop = pollCapabilities(fetcher, (event) => events.push(event), expired)
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(expired).toHaveBeenCalledTimes(1)
      // A 401 is not a failed read, so nothing is reported as one.
      expect(events).toEqual([])

      await vi.advanceTimersByTimeAsync(interval * 3)
      expect(fetcher).toHaveBeenCalledTimes(1)
    } finally {
      stop()
      vi.useRealTimers()
    }
  })

  it('aborts the request in flight and asks nothing more once stopped', async () => {
    vi.useFakeTimers()
    const pending = deferred<Capabilities>()
    let asked: AbortSignal | undefined
    const fetcher = vi.fn((signal: AbortSignal) => {
      asked = signal
      return pending.promise
    })
    const events: CapabilityEvent[] = []
    const stop = pollCapabilities(fetcher, (event) => events.push(event))
    try {
      expect(fetcher).toHaveBeenCalledTimes(1)

      stop()
      expect(asked?.aborted).toBe(true)

      // The request the component had already made fails after it is gone. The failure is
      // dropped rather than recorded: there is no screen left for it to describe.
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
})
