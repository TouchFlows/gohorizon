import { describe, expect, it } from 'vitest'
import { AdminActionError } from './api'
import type { Capabilities } from './api'
import { idleReread, noticeFor, rereadFailed, rereadFinished, rereadStarted } from './reread'

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

describe('what the control area draws', () => {
  const banner = 'Read-only for 5 minutes: could not open input file: artisan'

  it('offers the button beside the banner while the controls are withheld', () => {
    expect(noticeFor(banner, idleReread, true)).toEqual({
      banner,
      button: true,
      outcome: '',
    })
  })

  it('keeps what the read found after the banner it was drawn beside is gone', () => {
    // The regression this guards: a successful read is what empties the banner, so an outcome
    // drawn only inside the banner would unmount at the moment it became the answer. The control
    // is mounted either way, and the notice still carries the outcome with no banner and no
    // button to press.
    expect(noticeFor('', rereadFinished(readable), true)).toEqual({
      banner: '',
      button: false,
      outcome: 'Readable: schema 1, package gohorizon/laravel 0.1.0',
    })
  })

  it('keeps the outcome beside the banner when the read found it still read-only', () => {
    const notice = noticeFor(banner, rereadFinished(stillReadOnly), true)

    expect(notice.banner).toBe(banner)
    expect(notice.button).toBe(true)
    expect(notice.outcome).toBe('Still read-only: could not open input file: artisan')
  })

  it('draws no control for a role that may not ask for the read', () => {
    // A viewer and an operator get the banner and nothing else: the outcome belongs to the read
    // they were never offered.
    expect(noticeFor(banner, idleReread, false)).toEqual({ banner, button: false, outcome: '' })
    expect(noticeFor('', rereadFinished(readable), false)).toEqual({
      banner: '',
      button: false,
      outcome: '',
    })
  })
})
