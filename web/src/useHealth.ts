import { useEffect, useState } from 'react'
import { fetchHealth } from './api'
import type { HealthReport } from './api'

// pollInterval is how often the dashboard asks again. Five seconds keeps a queue depth
// honest without turning a browser tab into a load generator.
export const pollInterval = 5000

export interface HealthState {
  report: HealthReport | null
  // degraded is the 503 the service answers when its lock is not held. It is a state to
  // render, not an error: the body still explains it.
  degraded: boolean
  // error is the last failed refresh. The previous report is kept, because a blip should
  // not blank a screen an operator is watching.
  error: string | null
}

export function useHealth(): HealthState {
  const [state, setState] = useState<HealthState>({ report: null, degraded: false, error: null })

  useEffect(() => {
    const controller = new AbortController()
    let timer: number | undefined

    const load = async () => {
      try {
        const { report, degraded } = await fetchHealth(controller.signal)
        setState({ report, degraded, error: null })
      } catch (failure) {
        if (controller.signal.aborted) {
          return
        }
        const message = failure instanceof Error ? failure.message : String(failure)
        setState((current) => ({ ...current, error: message }))
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
