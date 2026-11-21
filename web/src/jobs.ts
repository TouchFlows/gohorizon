// Job history's filters, its cursor trail and the way a row reads, all away from React.
import type { Job } from './api'

export interface JobFilters {
  // An empty queue or outcome means "any": the endpoint treats the same absence the same way.
  queue: string
  outcome: string
}

// outcomes is the closed set the endpoint accepts, in its own order.
export const outcomes = ['processed', 'released', 'failed', 'timed_out'] as const

// pageSize is what the endpoint defaults to; sending it keeps the UI's idea of a page the
// same as the server's.
export const pageSize = 50

// buildJobsQuery mirrors the endpoint's parameters. A filter that is set to "any" is left
// out rather than sent empty, and the cursor is only sent once there is one.
export function buildJobsQuery(filters: JobFilters, cursor: string | null, limit = pageSize): string {
  const parameters = new URLSearchParams()
  if (filters.queue !== '') {
    parameters.set('queue', filters.queue)
  }
  if (filters.outcome !== '') {
    parameters.set('outcome', filters.outcome)
  }
  parameters.set('limit', String(limit))
  if (cursor !== null) {
    parameters.set('before', cursor)
  }
  return parameters.toString()
}

// Trail is the pages visited, oldest first: the last cursor is the one on screen and null
// is the newest page, which is read without a cursor. Keeping the trail is what lets
// "newer" return to a page already seen instead of re-reading from the top.
export interface Trail {
  cursors: (string | null)[]
}

export const firstTrail: Trail = { cursors: [null] }

export function trailCursor(trail: Trail): string | null {
  return trail.cursors[trail.cursors.length - 1]
}

export function trailOlder(trail: Trail, nextBefore: string): Trail {
  return { cursors: [...trail.cursors, nextBefore] }
}

// trailNewer is null on the newest page: there is nowhere newer to go.
export function trailNewer(trail: Trail): Trail | null {
  return trail.cursors.length <= 1 ? null : { cursors: trail.cursors.slice(0, -1) }
}

// exceptionText is the failure as one line, or null when the attempt threw nothing. The
// null is what the table renders as "none" — an empty string would look like a failure with
// no text instead of no failure at all.
export function exceptionText(job: Job): string | null {
  if (job.exception === null) {
    return null
  }
  return job.exception.message === '' ? job.exception.class : `${job.exception.class}: ${job.exception.message}`
}

// formatDuration reads an attempt's duration: milliseconds when it was quick, seconds when
// it was not.
export function formatDuration(milliseconds: number): string {
  if (milliseconds < 1000) {
    return `${milliseconds} ms`
  }
  return `${(milliseconds / 1000).toFixed(1)} s`
}

// formatFinished renders the attempt's finish time in the browser's zone, to the second.
export function formatFinished(finishedAt: string): string {
  const at = new Date(finishedAt)
  if (Number.isNaN(at.getTime())) {
    return finishedAt
  }
  return at.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}
