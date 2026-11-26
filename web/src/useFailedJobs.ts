import { useEffect, useState } from 'react'
import { FailedJobsUnavailable, fetchFailedJobs } from './api'
import type { FailedJobPage } from './api'

export interface FailedJobsState {
  page: FailedJobPage | null
  // unavailable is the 503: the application could not list its failed store, which is not an
  // empty store and must not be drawn as one.
  unavailable: boolean
  error: string | null
}

// useFailedJobs reads one page of the application's failed store for the cursor the trail
// holds. A new cursor drops the previous page rather than showing records that answer a
// different question; a new revision re-reads the same page after an action changed the store
// behind it.
export function useFailedJobs(query: string, revision: number): FailedJobsState {
  const [state, setState] = useState<FailedJobsState>({ page: null, unavailable: false, error: null })

  useEffect(() => {
    const controller = new AbortController()
    setState({ page: null, unavailable: false, error: null })

    fetchFailedJobs(query, controller.signal)
      .then((page) => {
        setState({ page, unavailable: false, error: null })
      })
      .catch((failure: unknown) => {
        if (controller.signal.aborted) {
          return
        }
        if (failure instanceof FailedJobsUnavailable) {
          setState({ page: null, unavailable: true, error: null })
          return
        }
        const message = failure instanceof Error ? failure.message : String(failure)
        setState({ page: null, unavailable: false, error: message })
      })

    return () => controller.abort()
  }, [query, revision])

  return state
}
