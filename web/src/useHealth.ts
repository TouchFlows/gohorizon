import { useEffect, useState } from 'react'
import { fetchHealth } from './api'
import type { HealthReport } from './api'

// pollInterval is how often the dashboard asks again. Five seconds keeps a queue depth
// honest without turning a browser tab into a load generator.
const pollInterval = 5000

export interface HealthState {
  report: HealthReport | null
  // degraded is the 503 the service answers when its lock is not held. It is a state to
  // render, not an error: the body still explains it.
  degraded: boolean
  // error is the last failed refresh. The previous report is kept, because a blip should
  // not blank a screen an operator is watching.
  error: string | null
}

// HealthEvent is one settled refresh: what the service answered with, or why it did not.
type HealthEvent =
  | { kind: 'report'; report: HealthReport; degraded: boolean }
  | { kind: 'failure'; message: string }

// applyHealth is the hook's state decision, kept out of React so both outcomes can be read
// without a DOM: an answer replaces the report, a failure only records why it is stale.
export function applyHealth(current: HealthState, event: HealthEvent): HealthState {
  if (event.kind === 'report') {
    return { report: event.report, degraded: event.degraded, error: null }
  }
  return { ...current, error: event.message }
}

export function useHealth(): HealthState {
  const [state, setState] = useState<HealthState>({ report: null, degraded: false, error: null })

  useEffect(() => {
    const controller = new AbortController()
    let timer: number | undefined

    const load = async () => {
      try {
        const { report, degraded } = await fetchHealth(controller.signal)
        setState((current) => applyHealth(current, { kind: 'report', report, degraded }))
      } catch (failure) {
        if (controller.signal.aborted) {
          return
        }
        const message = failure instanceof Error ? failure.message : String(failure)
        setState((current) => applyHealth(current, { kind: 'failure', message }))
      }
      if (!controller.signal.aborted) {
        timer = window.setTimeout(load, pollInterval)
      }
    }

    void load()
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [])

  return state
}
