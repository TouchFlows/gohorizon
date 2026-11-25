import type { ChartModel } from '../history'
import { formatClock } from '../history'
import type { Resolution } from '../api'

const width = 720
const height = 120
const padding = 28

// DurationChart is the other half of a bucket: the outcome columns say how many attempts
// finished, this says how long each one took. It draws an average because the store keeps
// the bucket's total, and an average is what a queue getting slower moves — a month of quiet
// minutes must not make one slow job look fast.
export default function DurationChart({ model, resolution }: { model: ChartModel; resolution: Resolution }) {
  const plotWidth = width - padding * 2
  const plotHeight = height - padding * 2
  const slot = plotWidth / model.buckets.length
  const barWidth = Math.max(1, slot - 1)
  // The axis label reports the slowest bucket, not the scale the bars are drawn against: a
  // store where every attempt finished in under a millisecond has no 1 ms peak, it has no
  // peak at all.
  const peakAverageMs = Math.max(0, ...model.buckets.map((bucket) => bucket.averageMs))

  return (
    <figure className="chart chart--duration">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={`Average duration per attempt over ${model.buckets.length} buckets, peak average ${peakAverageMs} ms`}
      >
        <line
          x1={padding}
          y1={padding + plotHeight}
          x2={padding + plotWidth}
          y2={padding + plotHeight}
          stroke="var(--line)"
        />
        {/* The label carries the number the caption used to, in the one place it reads as a
            scale: the top of the plot, which is what a full-height column means. */}
        <text className="chart__scale duration-chart__scale" x={padding} y={padding - 10}>
          {peakAverageMs} ms
        </text>
        {model.buckets.map((bucket, index) => {
          // A bucket that ran nothing has nothing to average; drawing it would claim a job
          // took no time at all.
          if (bucket.total === 0) {
            return null
          }
          const column = (bucket.averageMs / model.maxAverageMs) * plotHeight
          return (
            <rect
              key={bucket.start}
              x={padding + index * slot}
              y={padding + plotHeight - column}
              width={barWidth}
              height={column}
              fill="var(--chart-duration)"
            >
              <title>{`${formatClock(bucket.start, resolution)} — average ${bucket.averageMs} ms`}</title>
            </rect>
          )
        })}
      </svg>

      <figcaption className="chart__caption">
        <span className="chart__axis">Average duration per attempt</span>
      </figcaption>
    </figure>
  )
}
