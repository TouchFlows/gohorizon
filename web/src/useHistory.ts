import { useEffect, useState } from 'react'
import { fetchHistory, HistoryUnavailable } from './api'
import type { History } from './api'

export interface HistoryState {
  history: History | null
  // unavailable is the 503: history was never opened, which the page says plainly while
  // the rest of the dashboard keeps working.
  unavailable: boolean
  error: string | null
}

// useHistory reads the window the metrics page asks for. A null query means there is
// nothing to ask yet, such as before a queue has been sampled.
export function useHistory(query: string | null): HistoryState {
  const [state, setState] = useState<HistoryState>({ history: null, unavailable: false, error: null })

  useEffect(() => {
    if (query === null) {
      setState({ history: null, unavailable: false, error: null })
      return
    }

    const controller = new AbortController()
    setState((current) => ({ ...current, error: null }))

    fetchHistory(query, controller.signal)
      .then((history) => setState({ history, unavailable: false, error: null }))
      .catch((failure: unknown) => {
        if (controller.signal.aborted) {
          return
        }
        if (failure instanceof HistoryUnavailable) {
          setState({ history: null, unavailable: true, error: null })
          return
        }
        const message = failure instanceof Error ? failure.message : String(failure)
        setState({ history: null, unavailable: false, error: message })
      })

    return () => controller.abort()
  }, [query])

  return state
}
