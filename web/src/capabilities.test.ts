import { describe, expect, it } from 'vitest'
import type { Capabilities } from './api'
import { failedJobControls, workerControls } from './capabilities'

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

  it('explains an unsupported action without inventing a reason', () => {
    const caps = descriptor()
    caps.operations.failed_job_retry = false
    caps.operations.failed_job_delete = false

    expect(failedJobControls(caps, true).reason).toBe(
      'Read-only: the application does not report support for this action.',
    )
  })
})

describe('workerControls', () => {
  it('offers a restart when the service advertises it', () => {
    expect(workerControls(descriptor(), true)).toEqual({ restart: true, reason: '' })
  })

  it('keeps the restart when the application itself cannot answer', () => {
    // Restarting is the supervisor's own action, so an unavailable packages does not take it
    // away (docs/adr/0014).
    const caps = descriptor({ available: false, error: 'the package is not installed' })

    expect(workerControls(caps, true)).toEqual({ restart: true, reason: '' })
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
