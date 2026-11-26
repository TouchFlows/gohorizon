import type { Job } from '../api'
import { formatDuration, formatFinished } from '../jobs'

export default function JobTable({
  jobs,
  onSelect,
}: {
  jobs: Job[]
  onSelect?: (job: Job) => void
}) {
  if (jobs.length === 0) {
    return <p className="muted">No attempts match this filter.</p>
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
          {onSelect !== undefined && <th />}
        </tr>
      </thead>
      <tbody>
        {jobs.map((job) => (
          <tr key={job.id}>
            <td>{formatFinished(job.finished_at)}</td>
            <td className="mono">{job.class}</td>
            <td>{job.queue}</td>
            <td>{job.outcome}</td>
            <td className="num">{job.attempts}</td>
            <td className="num">{formatDuration(job.duration_ms)}</td>
            {onSelect !== undefined && (
              <td>
                <button type="button" className="chip" onClick={() => onSelect(job)}>
                  details
                </button>
              </td>
            )}
          </tr>
        ))}
      </tbody>
    </table>
  )
}
