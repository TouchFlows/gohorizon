import { useMemo, useState } from 'react'
import type { HealthReport, Resolution } from '../api'
import { buildChart, buildHistoryQuery, defaultRange, ranges } from '../history'
import { useHistory } from '../useHistory'
import DurationChart from '../components/DurationChart'
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

  // The queues come from the polled report, so a selected one can disappear when the
  // configuration changes: fall back to the first listed queue rather than query a queue
  // nobody is watching.
  const chosenQueue = queues.includes(queue) ? queue : (queues[0] ?? '')
  const query = useMemo(
    () => (chosenQueue === '' ? null : buildHistoryQuery(chosenQueue, resolution, hours, now)),
    [chosenQueue, resolution, hours, now],
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
            value={hours}
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
          <>
            <OutcomeChart model={model} resolution={resolution} />
            <DurationChart model={model} resolution={resolution} />
          </>
        )
      )}

      {model === null && !unavailable && error === null && chosenQueue !== '' && (
        <p className="muted">Reading history…</p>
      )}

      <p className="muted">
        Outcomes per {resolution} for {chosenQueue === '' ? 'the queue' : chosenQueue}. The store
        writes a row only when something ran, so the chart fills the quiet periods with zero
        columns rather than closing the gap. The lower chart is the average duration of an
        attempt in each of those columns.
      </p>
    </>
  )
}
