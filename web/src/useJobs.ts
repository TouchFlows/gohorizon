import { useEffect, useState } from 'react'
import { fetchJobs, HistoryUnavailable } from './api'
import type { JobPage } from './api'

export interface JobsState {
  page: JobPage | null
  // unavailable is the 503: job history shares the metrics database, so a service without
  // one has no rows to show.
  unavailable: boolean
  error: string | null
}

// useJobs reads one page of history for the query the filters and cursor build. A new query
// drops the previous page rather than showing rows that answer a different question.
export function useJobs(query: string): JobsState {
  const [state, setState] = useState<JobsState>({ page: null, unavailable: false, error: null })

  useEffect(() => {
    const controller = new AbortController()
    setState({ page: null, unavailable: false, error: null })

    fetchJobs(query, controller.signal)
      .then((page) => {
        if (controller.signal.aborted) {
          return
        }
        setState({ page, unavailable: false, error: null })
      })
      .catch((failure: unknown) => {
        if (controller.signal.aborted) {
          return
        }
        if (failure instanceof HistoryUnavailable) {
          setState({ page: null, unavailable: true, error: null })
          return
        }
        const message = failure instanceof Error ? failure.message : String(failure)
        setState({ page: null, unavailable: false, error: message })
      })

    return () => controller.abort()
  }, [query])

  return state
}
