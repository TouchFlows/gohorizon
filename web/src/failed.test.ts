import { describe, expect, it } from 'vitest'
import type { FailedJob } from './api'
import {
  buildFailedJobsQuery,
  failedActionNotice,
  failedAtText,
  failedExceptionText,
  failedJobClass,
} from './failed'

function record(overrides: Partial<FailedJob> = {}): FailedJob {
  return {
    id: 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d',
    uuid: 'e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d',
    connection: 'redis',
    queue: 'default',
    job: 'App\\Jobs\\AlwaysFails',
    failed_at: '2026-11-26 09:30:00',
    exception: 'RuntimeException: boom',
    ...overrides,
  }
}

describe('buildFailedJobsQuery', () => {
  it('asks for the newest page without a cursor', () => {
    const parameters = new URLSearchParams(buildFailedJobsQuery(null))
    expect(parameters.get('limit')).toBe('50')
    expect(parameters.has('before')).toBe(false)
  })

  it('sends the opaque cursor of a later page', () => {
    const parameters = new URLSearchParams(buildFailedJobsQuery('eyJhdCI6MTcwMDAwMDMwMCwiaWQiOiI5In0'))
    expect(parameters.get('before')).toBe('eyJhdCI6MTcwMDAwMDMwMCwiaWQiOiI5In0')
    expect(parameters.get('limit')).toBe('50')
  })
})

describe('failed rows', () => {
  it('names the class the payload carried', () => {
    expect(failedJobClass(record())).toBe('App\\Jobs\\AlwaysFails')
  })

  it('says a record has no readable class rather than drawing a blank cell', () => {
    expect(failedJobClass(record({ job: '' }))).toBe('unknown class')
  })

  it('shows the stored failure time as the application wrote it', () => {
    expect(failedAtText('2026-11-26 09:30:00')).toBe('2026-11-26 09:30:00')
    expect(failedAtText('  ')).toBe('unknown')
  })

  it('shows the one sanitised exception line, and says when there is none', () => {
    expect(failedExceptionText(record())).toBe('RuntimeException: boom')
    expect(failedExceptionText(record({ exception: '' }))).toBe('no exception recorded')
  })
})

describe('failedActionNotice', () => {
  // History is pruned and can be disabled, so the answer carries no queue name in those
  // cases; the sentence must still read as a finished action, without a dangling preposition.
  it('says a retry happened without naming an unknown queue', () => {
    expect(failedActionNotice('retry', '')).toBe('queued again')
  })

  it('says a delete happened without naming an unknown queue', () => {
    expect(failedActionNotice('delete', '')).toBe('deleted')
  })

  it('names the queue when the answer carried one', () => {
    expect(failedActionNotice('retry', 'default')).toBe('queued again on default')
    expect(failedActionNotice('delete', 'default')).toBe('deleted from default')
  })
})
