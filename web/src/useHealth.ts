import { useEffect, useRef, useState } from 'react'
import { fetchHealth, Unauthorized } from './api'
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

// HealthFetcher is the one request the loop makes. It is a parameter rather than the import,
// so the loop can be run without a service.
type HealthFetcher = (signal: AbortSignal) => Promise<{ report: HealthReport; degraded: boolean }>

// applyHealth is the hook's state decision, kept out of React so both outcomes can be read
// without a DOM: an answer replaces the report, a failure only records why it is stale.
export function applyHealth(current: HealthState, event: HealthEvent): HealthState {
  if (event.kind === 'report') {
    return { report: event.report, degraded: event.degraded, error: null }
  }
  return { ...current, error: event.message }
}

// pollHealth asks once, reports what came back, and asks again on the interval until it is
// stopped, which is what the effect's cleanup does on unmount. Stopping aborts the request in
// flight, so a late failure cannot be reported for a screen nobody is watching any more, and
// clears the timer, so nothing is asked afterwards. Both are read in useHealth.test.ts on
// fake timers, since neither can be timed from a browser.
// onUnauthorized, when given, is told that the session ended rather than the read failing. It
// is the one failure that is not reported as one: a stale report on a screen nobody is signed
// in for describes a sign-in as a fault.
export function pollHealth(
  fetcher: HealthFetcher,
  onChange: (event: HealthEvent) => void,
  onUnauthorized?: () => void,
): () => void {
  const controller = new AbortController()
  let timer: ReturnType<typeof setTimeout> | undefined

  const load = async () => {
    try {
      const { report, degraded } = await fetcher(controller.signal)
      onChange({ kind: 'report', report, degraded })
    } catch (failure) {
      if (controller.signal.aborted) {
        return
      }
      if (failure instanceof Unauthorized) {
        onUnauthorized?.()
        return
      }
      const message = failure instanceof Error ? failure.message : String(failure)
      onChange({ kind: 'failure', message })
    }
    if (!controller.signal.aborted) {
      timer = setTimeout(load, pollInterval)
    }
  }

  void load()
  return () => {
    controller.abort()
    clearTimeout(timer)
  }
}

export function useHealth(onUnauthorized?: () => void): HealthState {
  const [state, setState] = useState<HealthState>({ report: null, degraded: false, error: null })

  // Held in a ref so the loop keeps one identity: a callback rebuilt on every render would
  // otherwise restart the poll, and its 5 s timer with it.
  const expired = useRef(onUnauthorized)
  expired.current = onUnauthorized

  useEffect(() => {
    return pollHealth(
      fetchHealth,
      (event) => {
        setState((current) => applyHealth(current, event))
      },
      () => expired.current?.(),
    )
  }, [])

  return state
}
