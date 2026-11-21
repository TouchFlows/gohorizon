import type { Job } from '../api'
import { exceptionText, formatDuration, formatFinished } from '../jobs'

export default function JobTable({
  jobs,
  showException,
  onSelect,
}: {
  jobs: Job[]
  // Failed jobs show the failure in the row: that is the reason the page exists, and it
  // should not need a click to read.
  showException: boolean
  onSelect?: (job: Job) => void
}) {
  if (jobs.length === 0) {
    return <p className="muted">No attempts in this window.</p>
  }

  return (
    <table className="table">
      <thead>
        <tr>
          <th>Finished</th>
          <th>Class</th>
          <th>Queue</th>
          <th>Outcome</th>
          <th className="num">Attempts</th>
          <th className="num">Duration</th>
          {showException && <th>Exception</th>}
          {onSelect !== undefined && <th />}
        </tr>
      </thead>
      <tbody>
        {jobs.map((job) => {
          const failure = exceptionText(job)
          return (
            <tr key={job.id}>
              <td>{formatFinished(job.finished_at)}</td>
              <td className="mono">{job.class}</td>
              <td>{job.queue}</td>
              <td>{job.outcome}</td>
              <td className="num">{job.attempts}</td>
              <td className="num">{formatDuration(job.duration_ms)}</td>
              {showException && (
                <td className="exception">
                  {failure === null ? <span className="muted">none</span> : failure}
                </td>
              )}
              {onSelect !== undefined && (
                <td>
                  <button type="button" className="chip" onClick={() => onSelect(job)}>
                    details
                  </button>
                </td>
              )}
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}
