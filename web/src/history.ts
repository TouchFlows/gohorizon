// The chart's data and its query, away from React so both can be tested directly.
import type { History, Resolution } from './api'

export interface ChartBucket {
  start: number
  processed: number
  released: number
  failed: number
  timedOut: number
  total: number
  // durationMs is every attempt's duration in the bucket added up; averageMs is that over
  // the attempts in it, which is the number a queue slowing down moves.
  durationMs: number
  averageMs: number
}

export interface ChartModel {
  buckets: ChartBucket[]
  // max is the tallest column, never zero: a chart needs a scale even when every column is.
  max: number
  // maxAverageMs scales the duration chart the same way, and is never zero either.
  maxAverageMs: number
  total: number
  // empty means the window held no buckets at all, which the page says plainly instead of
  // drawing a zero line that would read as "nothing ran" rather than "nothing recorded".
  empty: boolean
}

// The step of each resolution, in seconds.
const stepSeconds: Record<Resolution, number> = { minute: 60, hourly: 3600, daily: 86400 }

// maxBuckets bounds the fill: a client that asks for a year of minutes would otherwise draw
// half a million columns. The picker never offers a window that large.
const maxBuckets = 720

// buildChart fills the gaps between buckets. The store writes a row only for a period that
// had a job, so a missing minute is a minute with no jobs, not missing history — a chart of
// only the present rows would squeeze those quiet periods out of the timeline.
export function buildChart(history: History): ChartModel {
  const step = stepSeconds[history.resolution]
  const from = Math.floor(Date.parse(history.from) / 1000 / step) * step
  const to = Math.ceil(Date.parse(history.to) / 1000 / step) * step

  const byStart = new Map<number, ChartBucket>()
  for (const bucket of history.buckets) {
    const start = Math.floor(Date.parse(bucket.start) / 1000 / step) * step
    const total = bucket.processed + bucket.released + bucket.failed + bucket.timed_out
    byStart.set(start, {
      start,
      processed: bucket.processed,
      released: bucket.released,
      failed: bucket.failed,
      timedOut: bucket.timed_out,
      total,
      durationMs: bucket.duration_ms,
      // Nothing ran, so there is no duration to average. Zero is the absence here, and the
      // duration chart leaves those columns out rather than drawing instant jobs.
      averageMs: total === 0 ? 0 : Math.round(bucket.duration_ms / total),
    })
  }

  const buckets: ChartBucket[] = []
  const first = Math.max(from, to - step * maxBuckets)
  for (let start = first; start < to; start += step) {
    buckets.push(
      byStart.get(start) ?? {
        start,
        processed: 0,
        released: 0,
        failed: 0,
        timedOut: 0,
        total: 0,
        durationMs: 0,
        averageMs: 0,
      },
    )
  }

  return {
    buckets,
    max: Math.max(1, ...buckets.map((bucket) => bucket.total)),
    maxAverageMs: Math.max(1, ...buckets.map((bucket) => bucket.averageMs)),
    total: buckets.reduce((sum, bucket) => sum + bucket.total, 0),
    empty: history.buckets.length === 0,
  }
}

export interface HistoryRange {
  label: string
  hours: number
}

// ranges offers windows that stay legible at their resolution: minutes for a few hours,
// hours for a week, days for a year.
export const ranges: Record<Resolution, HistoryRange[]> = {
  minute: [
    { label: '30m', hours: 0.5 },
    { label: '1h', hours: 1 },
    { label: '6h', hours: 6 },
  ],
  hourly: [
    { label: '6h', hours: 6 },
    { label: '24h', hours: 24 },
    { label: '7d', hours: 168 },
  ],
  daily: [
    { label: '7d', hours: 168 },
    { label: '30d', hours: 720 },
    { label: '365d', hours: 8760 },
  ],
}

// defaultRange is the endpoint's own default window for each resolution.
export function defaultRange(resolution: Resolution): HistoryRange {
  const options = ranges[resolution]
  return options[Math.floor(options.length / 2)]
}

// buildHistoryQuery mirrors the endpoint's parameters, with the window explicit: the picker
// chooses it, so the server's defaults never decide what is on screen.
export function buildHistoryQuery(
  queue: string,
  resolution: Resolution,
  hours: number,
  nowMilliseconds: number,
): string {
  const parameters = new URLSearchParams({
    queue,
    resolution,
    from: new Date(nowMilliseconds - hours * 3600 * 1000).toISOString(),
    to: new Date(nowMilliseconds).toISOString(),
  })
  return parameters.toString()
}

// formatClock labels a bucket: a time for minutes and hours, a date for days.
export function formatClock(startSeconds: number, resolution: Resolution): string {
  const at = new Date(startSeconds * 1000)
  if (resolution === 'daily') {
    return at.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  }
  return at.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}
