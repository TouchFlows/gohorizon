import type { ChartModel } from '../history'
import { formatClock, jobCount } from '../history'
import type { Resolution } from '../api'

// One colour per outcome, in the order a column stacks. The same order is the legend.
const series = [
  { key: 'processed', label: 'processed', colour: 'var(--chart-processed)' },
  { key: 'released', label: 'released', colour: 'var(--chart-released)' },
  { key: 'failed', label: 'failed', colour: 'var(--chart-failed)' },
  { key: 'timedOut', label: 'timed out', colour: 'var(--chart-timed-out)' },
] as const

const width = 720
const height = 240
const padding = 28

export default function OutcomeChart({ model, resolution }: { model: ChartModel; resolution: Resolution }) {
  const plotWidth = width - padding * 2
  const plotHeight = height - padding * 2
  const slot = plotWidth / model.buckets.length
  const barWidth = Math.max(1, slot - 1)

  return (
    <figure className="chart outcome-chart">
      <svg viewBox={`0 0 ${width} ${height}`} role="img" aria-label={`Outcomes over ${model.buckets.length} buckets, ${jobCount(model.total)}`}>
        <line
          x1={padding}
          y1={padding + plotHeight}
          x2={padding + plotWidth}
          y2={padding + plotHeight}
          stroke="var(--line)"
        />
        {/* The top of the scale, so a full-height column is a number and not a guess. It
            sits in the margin above the plot, where no column can reach it. */}
        <text className="chart__scale outcome-chart__scale" x={padding} y={padding - 10}>
          {jobCount(model.max)}
        </text>
        {model.buckets.map((bucket, index) => {
          const x = padding + index * slot
          let baseline = padding + plotHeight
          return (
            <g key={bucket.start}>
              {series.map((outcome) => {
                const value = bucket[outcome.key]
                if (value === 0) {
                  return null
                }
                const column = (value / model.max) * plotHeight
                baseline -= column
                return (
                  <rect
                    key={outcome.key}
                    x={x}
                    y={baseline}
                    width={barWidth}
                    height={column}
                    fill={outcome.colour}
                  >
                    <title>
                      {formatClock(bucket.start, resolution)} — {outcome.label}: {value}
                    </title>
                  </rect>
                )
              })}
            </g>
          )
        })}
      </svg>

      <figcaption className="chart__caption">
        <span className="chart__axis">
          {formatClock(model.buckets[0].start, resolution)} →{' '}
          {formatClock(model.buckets[model.buckets.length - 1].start, resolution)}
        </span>
        <span className="chart__legend">
          {series.map((outcome) => (
            <span key={outcome.key} className="chart__key">
              <span className="chart__swatch" style={{ background: outcome.colour }} />
              {outcome.label}
            </span>
          ))}
        </span>
        <span className="chart__total outcome-chart__total">{jobCount(model.total)}</span>
      </figcaption>
    </figure>
  )
}
