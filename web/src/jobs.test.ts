import { describe, expect, it } from 'vitest'
import type { Job } from './api'
import {
  buildJobsQuery,
  exceptionText,
  firstTrail,
  formatDuration,
  trailCursor,
  trailNewer,
  trailOlder,
} from './jobs'

function job(overrides: Partial<Job> = {}): Job {
  return {
    id: '1774191600000-0',
    job_id: '7',
    uuid: '0b9b6f0a-1c1a-4c8a-9f0a-3f8b1d2e4c55',
    class: 'App\\Jobs\\Marker',
    queue: 'default',
    connection: 'default',
    outcome: 'processed',
    attempts: 1,
    duration_ms: 12,
    finished_at: '2026-11-21T20:40:00Z',
    exception: null,
    ...overrides,
  }
}

describe('buildJobsQuery', () => {
  it('leaves out a filter that is set to any', () => {
    const parameters = new URLSearchParams(buildJobsQuery({ queue: '', outcome: '' }, null))
    expect(parameters.has('queue')).toBe(false)
    expect(parameters.has('outcome')).toBe(false)
    expect(parameters.has('before')).toBe(false)
    expect(parameters.get('limit')).toBe('50')
  })

  it('sends the filter that is set and the cursor the page was read with', () => {
    const parameters = new URLSearchParams(buildJobsQuery({ queue: 'emails', outcome: 'failed' }, '2026-11-21T20:40:00Z'))
    expect(parameters.get('queue')).toBe('emails')
    expect(parameters.get('outcome')).toBe('failed')
    expect(parameters.get('before')).toBe('2026-11-21T20:40:00Z')
  })

  it('encodes a queue that looks like query syntax instead of splitting the query', () => {
    const parameters = new URLSearchParams(buildJobsQuery({ queue: 'a&b=c', outcome: '' }, null))
    expect(parameters.get('queue')).toBe('a&b=c')
  })
})

describe('trail', () => {
  it('starts on the newest page, which is read without a cursor', () => {
    expect(trailCursor(firstTrail)).toBeNull()
  })

  it('walks older and back to the cursor each page was read with', () => {
    const second = trailOlder(firstTrail, 'c1')
    expect(trailCursor(second)).toBe('c1')

    const third = trailOlder(second, 'c2')
    expect(trailCursor(third)).toBe('c2')

    const back = trailNewer(third)
    expect(back).not.toBeNull()
    expect(trailCursor(back as typeof third)).toBe('c1')

    const newest = trailNewer(back as typeof third)
    expect(newest).not.toBeNull()
    expect(trailCursor(newest as typeof third)).toBeNull()
  })

  it('has nowhere newer to go from the newest page', () => {
    expect(trailNewer(firstTrail)).toBeNull()
  })

  it('does not mutate the trail it extends', () => {
    const trail = firstTrail
    trailOlder(trail, 'c1')
    expect(trail.cursors).toEqual([null])
  })
})

describe('exceptionText', () => {
  it('is null when the attempt threw nothing, so the table can say none', () => {
    expect(exceptionText(job())).toBeNull()
  })

  it('keeps the class when there is no message', () => {
    expect(exceptionText(job({ exception: { class: 'RuntimeException', message: '' } }))).toBe('RuntimeException')
  })

  it('reads class and message as one line', () => {
    expect(exceptionText(job({ exception: { class: 'RuntimeException', message: 'boom' } }))).toBe(
      'RuntimeException: boom',
    )
  })
})

describe('formatDuration', () => {
  it('reads quick attempts in milliseconds and slower ones in seconds', () => {
    expect(formatDuration(0)).toBe('0 ms')
    expect(formatDuration(120)).toBe('120 ms')
    expect(formatDuration(999)).toBe('999 ms')
    expect(formatDuration(1500)).toBe('1.5 s')
    expect(formatDuration(12_345)).toBe('12.3 s')
  })
})
