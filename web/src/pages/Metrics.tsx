import { useMemo, useState } from 'react'
import type { HealthReport, Resolution } from '../api'
import { buildChart, buildHistoryQuery, defaultRange, ranges } from '../history'
import { useHistory } from '../useHistory'
import OutcomeChart from '../components/OutcomeChart'

const resolutions: Resolution[] = ['minute', 'hourly', 'daily']

export default function Metrics({ report }: { report: HealthReport }) {
  const queues = (report.queues.depths ?? []).map((depth) => depth.queue)
  const [queue, setQueue] = useState('')
  const [resolution, setResolution] = useState<Resolution>('minute')
  const [hours, setHours] = useState(() => defaultRange('minute').hours)
  // now only moves when the operator asks for something: a query that changed on every
  // render would refetch forever.
  const [now, setNow] = useState(() => Date.now())

  const range = ranges[resolution].find((option) => option.hours === hours) ?? defaultRange(resolution)
  const chosenQueue = queue === '' ? (queues[0] ?? '') : queue
  const query = useMemo(
    () => (chosenQueue === '' ? null : buildHistoryQuery(chosenQueue, resolution, range, now)),
    [chosenQueue, resolution, range, now],
  )
  const { history, unavailable, error } = useHistory(query)
  const model = history === null ? null : buildChart(history)

  const chooseResolution = (next: Resolution) => {
    setResolution(next)
    setHours(defaultRange(next).hours)
    setNow(Date.now())
  }

  return (
    <>
      <h1>Metrics</h1>

      <section className="controls">
        <label className="controls__field">
          Queue
          <select value={chosenQueue} onChange={(event) => setQueue(event.target.value)} disabled={queues.length === 0}>
            {queues.length === 0 && <option value="">none sampled</option>}
            {queues.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>

        <div className="controls__group" role="group" aria-label="Resolution">
          {resolutions.map((option) => (
            <button
              key={option}
              type="button"
              className={option === resolution ? 'chip chip--active' : 'chip'}
              aria-pressed={option === resolution}
              onClick={() => chooseResolution(option)}
            >
              {option}
            </button>
          ))}
        </div>

        <label className="controls__field">
          Window
          <select
            value={range.hours}
            onChange={(event) => {
              setHours(Number(event.target.value))
              setNow(Date.now())
            }}
          >
            {ranges[resolution].map((option) => (
              <option key={option.label} value={option.hours}>
                last {option.label}
              </option>
            ))}
          </select>
        </label>

        <button type="button" className="chip" onClick={() => setNow(Date.now())}>
          refresh
        </button>
      </section>

      {queues.length === 0 && <p className="muted">No queue has been sampled yet, so there is no history to chart.</p>}
      {unavailable && (
        <p className="banner">
          History is unavailable: the service was started without its metrics database. The rest of the
          dashboard keeps working.
        </p>
      )}
      {error !== null && <p className="banner">Could not read history: {error}</p>}

      {model !== null && !unavailable && error === null && (
        model.empty ? (
          <p className="muted">No data in this window.</p>
        ) : (
          <OutcomeChart model={model} resolution={resolution} />
        )
      )}

      {model === null && !unavailable && error === null && chosenQueue !== '' && (
        <p className="muted">Reading history…</p>
      )}

      <p className="muted">
        Outcomes per {resolution} for {chosenQueue === '' ? 'the queue' : chosenQueue}. A period with no
        jobs has a row only when something ran, so the chart fills the quiet periods with zero columns
        rather than closing the gap.
      </p>
    </>
  )
}
