import { describe, expect, it } from 'vitest'
import { AdminActionError } from './api'
import type { Capabilities } from './api'
import { idleReread, rereadFailed, rereadFinished, rereadStarted } from './reread'

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
  package_version: '0.1.0',
  operations,
}

// What a read of a deployment that is still broken answers: the descriptor, with every control
// withheld and the reason in the service's own words.
const stillReadOnly: Capabilities = {
  available: false,
  operations: {
    inspect_queues: false,
    telemetry: false,
    failed_jobs_list: false,
    failed_job_retry: false,
    failed_job_delete: false,
    restart_workers: false,
  },
  error: 'could not open input file: artisan',
}

describe('the re-read control', () => {
  it('starts idle, with nothing to report', () => {
    expect(idleReread).toEqual({ reading: false, outcome: '' })
  })

  it('is reading — and disabled — while the call is in flight', () => {
    // reading is what the button's disabled attribute follows, so a second click cannot start a
    // second command while the first is waiting on PHP.
    expect(rereadStarted()).toEqual({ reading: true, outcome: '' })
  })

  it('clears what the previous read found when a new one starts', () => {
    // The answer on screen belongs to the read that produced it; a button already working with
    // "Readable" beside it would describe a moment that has passed.
    expect(rereadStarted().outcome).toBe('')
  })

  it('says what a repaired descriptor reports', () => {
    expect(rereadFinished(readable)).toEqual({
      reading: false,
      outcome: 'Readable: schema 1, package gohorizon/laravel 0.1.0',
    })
  })

  it('repeats the reason when the deployment is still read-only', () => {
    expect(rereadFinished(stillReadOnly).outcome).toBe(
      'Still read-only: could not open input file: artisan',
    )
  })

  it("carries the service's refusal when the call itself was refused", () => {
    expect(rereadFailed(new AdminActionError(403, 'administrator role required'))).toEqual({
      reading: false,
      outcome: 'Could not re-read the descriptor: administrator role required',
    })
  })

  it('still words a failure that carried no sentence', () => {
    // A network failure has a message the browser chose; an empty one is no better than none,
    // and the control says what it was doing rather than showing a blank status.
    expect(rereadFailed(new Error('')).outcome).toBe('Could not re-read the descriptor.')
    expect(rereadFailed(undefined).outcome).toBe('Could not re-read the descriptor.')
  })
})
