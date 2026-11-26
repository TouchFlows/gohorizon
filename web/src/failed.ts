// The failed store's query and the way one of its records reads, away from React.
import type { FailedJob } from './api'

// failedPageSize is what the endpoint defaults to; sending it keeps the UI's idea of a page
// the same as the server's.
export const failedPageSize = 50

// buildFailedJobsQuery mirrors the endpoint's two parameters: the page size and, once there
// is one, the opaque cursor the previous page ended on.
export function buildFailedJobsQuery(cursor: string | null, limit = failedPageSize): string {
  const parameters = new URLSearchParams()
  parameters.set('limit', String(limit))
  if (cursor !== null) {
    parameters.set('before', cursor)
  }
  return parameters.toString()
}

// failedAtText is the time the application stored for the failure. Laravel writes it in the
// application's zone and without one, so it is shown as stored rather than shifted into the
// browser's zone as if it had been UTC.
export function failedAtText(value: string): string {
  const trimmed = value.trim()
  return trimmed === '' ? 'unknown' : trimmed
}

// failedJobClass is the class name from the record's payload, or a word for a record whose
// payload was unreadable rather than a blank cell.
export function failedJobClass(job: FailedJob): string {
  return job.job === '' ? 'unknown class' : job.job
}

// failedActionNotice is what the panel says after a retry or a delete. The queue is detail
// history may not have — it is pruned, and a deployment without it names none — so an empty
// one is dropped from the sentence rather than printed as "queued again on ".
export function failedActionNotice(action: 'retry' | 'delete', queue: string): string {
  const done = action === 'retry' ? 'queued again' : 'deleted'
  if (queue === '') {
    return done
  }
  return action === 'retry' ? `${done} on ${queue}` : `${done} from ${queue}`
}

// failedExceptionText is the one sanitised line the service sends. A record stored without an
// exception says so instead of leaving the cell empty.
export function failedExceptionText(job: FailedJob): string {
  return job.exception === '' ? 'no exception recorded' : job.exception
}
