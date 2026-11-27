import type { Capabilities } from './api'
import { readResult } from './capabilities'

// RereadState is the control beside the read-only banner, kept out of the component so its rules
// can be read without a DOM: idle, reading while the request is in flight (which is when the
// button is disabled), and then holding what the read found.
export interface RereadState {
  reading: boolean
  outcome: string
}

export const idleReread: RereadState = { reading: false, outcome: '' }

// rereadStarted clears the last answer: what a previous read found is not what this one is
// looking at, and showing it beside a button that is already working would describe the wrong
// moment.
export function rereadStarted(): RereadState {
  return { reading: true, outcome: '' }
}

// rereadFinished holds what the read found — a descriptor that is still unreadable included,
// because that is an answer about the deployment rather than a failure of the call.
export function rereadFinished(capabilities: Capabilities): RereadState {
  return { reading: false, outcome: readResult(capabilities) }
}

// rereadFailed words a call that did not answer at all: the service's own refusal when it sent
// one (the role, the token), and this browser's failure otherwise.
export function rereadFailed(failure: unknown): RereadState {
  const detail = failure instanceof Error ? failure.message.trim() : ''
  return {
    reading: false,
    outcome:
      detail === ''
        ? 'Could not re-read the descriptor.'
        : `Could not re-read the descriptor: ${detail}`,
  }
}
