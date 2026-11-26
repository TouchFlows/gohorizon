import { useMemo, useState } from 'react'
import type { HealthReport, Job } from '../api'
import {
  buildJobsQuery,
  firstTrail,
  formatDuration,
  formatFinished,
  outcomes,
  trailCursor,
  trailNewer,
  trailOlder,
} from '../jobs'
import type { Trail } from '../jobs'
import { useJobs } from '../useJobs'
import JobTable from '../components/JobTable'

export default function Jobs({ report }: { report: HealthReport }) {
  const queues = (report.queues.depths ?? []).map((depth) => depth.queue)
  const [queue, setQueue] = useState('')
  const [outcome, setOutcome] = useState('')
  const [trail, setTrail] = useState<Trail>(firstTrail)
  const [selected, setSelected] = useState<Job | null>(null)

  // The queue list comes from the polled report, so a selected queue can disappear when the
  // configuration changes; the filter falls back to "any" instead of querying a queue the
  // select no longer offers.
  const chosenQueue = queues.includes(queue) ? queue : ''
  const cursor = trailCursor(trail)
  const query = useMemo(
    () => buildJobsQuery({ queue: chosenQueue, outcome }, cursor),
    [chosenQueue, outcome, cursor],
  )
  const { page, unavailable, error } = useJobs(query)

  // A filter is a new question, so it starts at the newest page with no row selected.
  const chooseQueue = (value: string) => {
    setQueue(value)
    setTrail(firstTrail)
    setSelected(null)
  }
  const chooseOutcome = (value: string) => {
    setOutcome(value)
    setTrail(firstTrail)
    setSelected(null)
  }

  // The panel describes one row, so leaving the page it is on closes it: otherwise the
  // detail sits under a table that no longer lists what it describes.
  const newer = trailNewer(trail)
  const goNewer = () => {
    if (newer !== null) {
      setTrail(newer)
      setSelected(null)
    }
  }
  const goOlder = () => {
    if (page !== null && page.next_before !== '') {
      setTrail(trailOlder(trail, page.next_before))
      setSelected(null)
    }
  }

  return (
    <>
      <h1>Jobs</h1>

      <section className="controls">
        <label className="controls__field">
          Queue
          <select value={chosenQueue} onChange={(event) => chooseQueue(event.target.value)}>
            <option value="">any</option>
            {queues.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>

        <label className="controls__field">
          Outcome
          <select value={outcome} onChange={(event) => chooseOutcome(event.target.value)}>
            <option value="">any</option>
            {outcomes.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>

        <div className="controls__group" role="group" aria-label="Paging">
          <button type="button" className="chip" disabled={newer === null} onClick={goNewer}>
            newer
          </button>
          <button
            type="button"
            className="chip"
            disabled={page === null || page.next_before === ''}
            onClick={goOlder}
          >
            older
          </button>
          <span className="muted">page {trail.cursors.length}</span>
        </div>
      </section>

      {unavailable && (
        <p className="banner">
          Job history is unavailable: the service was started without its metrics database. The
          rest of the dashboard keeps working.
        </p>
      )}
      {error !== null && <p className="banner">Could not read job history: {error}</p>}
      {page === null && !unavailable && error === null && <p className="muted">Reading job history…</p>}

      {page !== null && <JobTable jobs={page.jobs} onSelect={(job) => setSelected(job)} />}

      {selected !== null && <JobDetail job={selected} onClose={() => setSelected(null)} />}

      <p className="muted">
        Read-only: retry and delete act on a failed job, from its detail panel on the failed jobs
        screen.
      </p>
    </>
  )
}

function JobDetail({ job, onClose }: { job: Job; onClose: () => void }) {
  return (
    <section className="panel detail jobs__detail" aria-label="Job detail">
      <div className="detail__head">
        <h2>{job.class}</h2>
        <button type="button" className="chip" onClick={onClose}>
          close
        </button>
      </div>

      <dl className="detail__fields">
        <dt>Outcome</dt>
        <dd>{job.outcome}</dd>
        <dt>Queue</dt>
        <dd>{job.queue}</dd>
        <dt>Attempts</dt>
        <dd>{job.attempts}</dd>
        <dt>Duration</dt>
        <dd>{formatDuration(job.duration_ms)}</dd>
        <dt>Finished</dt>
        <dd>{formatFinished(job.finished_at)}</dd>
        <dt>UUID</dt>
        <dd className="mono">{job.uuid === '' ? 'none published' : job.uuid}</dd>
        <dt>Job id</dt>
        <dd className="mono">{job.job_id}</dd>
        <dt>Event id</dt>
        <dd className="mono">{job.id}</dd>
      </dl>

      {job.exception === null ? (
        <p className="muted">No exception was recorded for this attempt.</p>
      ) : (
        <div className="detail__exception">
          <h3>{job.exception.class}</h3>
          <pre>{job.exception.message === '' ? 'no message' : job.exception.message}</pre>
        </div>
      )}
    </section>
  )
}
