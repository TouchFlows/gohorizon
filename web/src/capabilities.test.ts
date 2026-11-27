import { describe, expect, it } from 'vitest'
import type { Capabilities } from './api'
import {
  failedJobControls,
  mayReread,
  readOnlyFor,
  readResult,
  workerControls,
} from './capabilities'

function descriptor(overrides: Partial<Capabilities> = {}): Capabilities {
  return {
    available: true,
    operations: {
      inspect_queues: true,
      telemetry: true,
      failed_jobs_list: true,
      failed_job_retry: true,
      failed_job_delete: true,
      restart_workers: true,
    },
    ...overrides,
  }
}

describe('failedJobControls', () => {
  it('offers both actions when the application reports supporting them', () => {
    expect(failedJobControls(descriptor(), true)).toEqual({ retry: true, delete: true, reason: '' })
  })

  it('gates each action on its own capability', () => {
    const caps = descriptor()
    caps.operations.failed_job_delete = false

    expect(failedJobControls(caps, true)).toEqual({ retry: true, delete: false, reason: '' })
  })

  it('leaves both actions alone until the descriptor arrives', () => {
    // A transient read failure must not hide a control the service still offers: it refuses an
    // action it cannot take, and the screen asks again on the next load.
    expect(failedJobControls(null, true)).toEqual({ retry: true, delete: true, reason: '' })
  })

  it('offers nothing without users, whatever the application supports', () => {
    const controls = failedJobControls(descriptor(), false)

    expect(controls.retry).toBe(false)
    expect(controls.delete).toBe(false)
    expect(controls.reason).toContain('without users')
  })

  it('says why the actions are missing when the application cannot answer', () => {
    const caps = descriptor({
      available: false,
      error: 'gohorizon/laravel adapter is unavailable: could not open input file: artisan',
    })
    caps.operations.failed_job_retry = false
    caps.operations.failed_job_delete = false

    const controls = failedJobControls(caps, true)
    expect(controls.retry).toBe(false)
    expect(controls.delete).toBe(false)
    expect(controls.reason).toContain('could not open input file')
  })

  it("prefers the operation's own reason over the descriptor error", () => {
    // A readable descriptor can withhold one screen without the deployment being read-only, so
    // the reason names the fact that withheld it (docs/adr/0014).
    const caps = descriptor({
      operation_reasons: { failed_job_retry: 'the application reports no failed store' },
    })
    caps.operations.failed_job_retry = false
    caps.operations.failed_job_delete = false

    expect(failedJobControls(caps, true).reason).toBe(
      'Read-only: the application reports no failed store',
    )
  })

  it('explains an unsupported action without inventing a reason', () => {
    const caps = descriptor()
    caps.operations.failed_job_retry = false
    caps.operations.failed_job_delete = false

    expect(failedJobControls(caps, true).reason).toBe(
      'Read-only: the application does not report support for this action.',
    )
  })
})

describe('readOnlyFor', () => {
  it('counts minutes, hours and days, and never reports a negative age', () => {
    const start = 1_000_000

    expect(readOnlyFor(start, start + 30)).toBe('less than a minute')
    expect(readOnlyFor(start, start + 60)).toBe('1 minute')
    expect(readOnlyFor(start, start + 600)).toBe('10 minutes')
    expect(readOnlyFor(start, start + 3599)).toBe('59 minutes')
    expect(readOnlyFor(start, start + 3600)).toBe('1 hour')
    expect(readOnlyFor(start, start + 7200)).toBe('2 hours')
    expect(readOnlyFor(start, start + 86_400)).toBe('1 day')
    expect(readOnlyFor(start, start + 3 * 86_400)).toBe('3 days')
    // An instant from the future is a clock disagreement, not an age: the banner says the
    // shortest thing it can rather than a negative duration.
    expect(readOnlyFor(start, start - 600)).toBe('less than a minute')
  })
})

describe('the read-only banner', () => {
  it('says how long the deployment has been read-only, not only why', () => {
    // The descriptor publishes the instant the service recorded, so the banner needs no second
    // poll to say how long the controls have been missing (docs/adr/0014).
    const now = Math.floor(Date.now() / 1000)
    const caps = descriptor({
      available: false,
      error: 'the package is not installed',
      read_only_since: now - 900,
    })
    caps.operations.restart_workers = false

    expect(workerControls(caps, true).reason).toBe(
      'Read-only for 15 minutes: the package is not installed',
    )
  })

  it('leaves the age out when only one screen is withheld', () => {
    // A readable descriptor withholds one operation with a reason of its own; nothing went
    // read-only, so there is no instant and nothing to count from.
    const caps = descriptor({ operation_reasons: { failed_job_retry: 'the application reports no failed store' } })
    caps.operations.failed_job_retry = false
    caps.operations.failed_job_delete = false

    expect(failedJobControls(caps, true).reason).toBe(
      'Read-only: the application reports no failed store',
    )
  })
})

describe('workerControls', () => {
  it('offers a restart when the service advertises it', () => {
    expect(workerControls(descriptor(), true)).toEqual({ restart: true, reason: '' })
  })

  it('drops the restart and says why when the application cannot answer', () => {
    // A deployment whose descriptor cannot be read is read-only as a whole, restart included
    // (docs/adr/0014).
    const caps = descriptor({ available: false, error: 'the package is not installed' })
    caps.operations.restart_workers = false

    const controls = workerControls(caps, true)
    expect(controls.restart).toBe(false)
    expect(controls.reason).toBe('Read-only: the package is not installed')
  })

  it('disables the restart the descriptor does not advertise, and says so', () => {
    const caps = descriptor()
    caps.operations.restart_workers = false

    const controls = workerControls(caps, true)
    expect(controls.restart).toBe(false)
    expect(controls.reason).toContain('does not report support')
  })

  it('offers nothing without users, and leaves the descriptor alone until it arrives', () => {
    expect(workerControls(descriptor(), false).reason).toContain('without users')
    expect(workerControls(null, true).restart).toBe(true)
  })
})

describe('readResult', () => {
  it('says what a readable descriptor reports, so a repair is confirmed in words', () => {
    expect(
      readResult(
        descriptor({ schema: 1, package: 'gohorizon/laravel', package_version: '0.1.0' }),
      ),
    ).toBe('Readable: schema 1, package gohorizon/laravel 0.1.0')
  })

  it('names a package once when the application reports no version', () => {
    expect(readResult(descriptor({ schema: 1, package: 'gohorizon/laravel' }))).toBe(
      'Readable: schema 1, package gohorizon/laravel',
    )
  })

  it('repeats the reason when the read found the deployment still unreadable', () => {
    // A read that answered "still broken" is an answer about the deployment, not a failure of
    // the call: the control repeats the service's sentence rather than inventing a status.
    expect(
      readResult(descriptor({ available: false, error: 'could not open input file: artisan' })),
    ).toBe('Still read-only: could not open input file: artisan')
  })

  it('does not invent a reason the descriptor did not carry', () => {
    expect(readResult(descriptor({ available: false }))).toBe('Still read-only.')
  })
})

describe('mayReread', () => {
  it("is the administrator's control, the role the service requires of the call", () => {
    expect(mayReread('administrator')).toBe(true)
    expect(mayReread('operator')).toBe(false)
    expect(mayReread('viewer')).toBe(false)
    // A service started without users has nobody to hold the role, and an older service answers
    // no role at all.
    expect(mayReread('')).toBe(false)
    expect(mayReread(null)).toBe(false)
  })
})
