import { useMemo, useState } from 'react'
import type { Capabilities, FailedJob } from '../api'
import { failedJobControls } from '../capabilities'
import type { FailedJobControls } from '../capabilities'
import { buildFailedJobsQuery, failedAtText, failedExceptionText, failedJobClass } from '../failed'
import { firstTrail, trailCursor, trailNewer, trailOlder } from '../jobs'
import type { Trail } from '../jobs'
import { useFailedJobs } from '../useFailedJobs'
import FailedJobActions from '../components/FailedJobActions'
import RereadCapabilities from '../components/RereadCapabilities'

// canAct is whether this deployment has administrative routes at all: a service started
// without users mounts none. capabilities is what the application supports, which decides
// whether the two actions are offered; while it is still being read, the session alone
// decides (docs/adr/0014).
export default function Failed({
  canAct,
  capabilities = null,
  reread,
  canReread = false,
}: {
  canAct: boolean
  capabilities?: Capabilities | null
  // reread asks the service for a descriptor read now; the control beside the banner draws it
  // for administrators only (docs/adr/0014).
  reread: () => Promise<Capabilities>
  canReread?: boolean
}) {
  const controls = failedJobControls(capabilities, canAct)
  const [trail, setTrail] = useState<Trail>(firstTrail)
  const [selected, setSelected] = useState<FailedJob | null>(null)
  // A retry or a delete changes the store, not the question the page asks, so the page is
  // re-read at the same cursor rather than the trail being reset.
  const [revision, setRevision] = useState(0)

  const cursor = trailCursor(trail)
  const query = useMemo(() => buildFailedJobsQuery(cursor), [cursor])
  const { page, unavailable, error } = useFailedJobs(query, revision)

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
      <h1>Failed jobs</h1>

      <section className="controls">
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
          Failed jobs are unavailable: the application could not list its failed store. The rest
          of the dashboard keeps working.
        </p>
      )}
      {error !== null && <p className="banner">Could not read failed jobs: {error}</p>}
      {page === null && !unavailable && error === null && <p className="muted">Reading failed jobs…</p>}

      {page !== null && (
        <table className="table">
          <thead>
            <tr>
              <th>Failed at</th>
              <th>Class</th>
              <th>Queue</th>
              <th>Exception</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {page.failed_jobs.length === 0 ? (
              <tr>
                <td colSpan={5} className="muted">
                  No failed jobs are stored by the application.
                </td>
              </tr>
            ) : (
              page.failed_jobs.map((job) => (
                <tr key={job.id}>
                  <td>{failedAtText(job.failed_at)}</td>
                  <td className="mono">{failedJobClass(job)}</td>
                  <td>{job.queue}</td>
                  <td className="exception">{failedExceptionText(job)}</td>
                  <td>
                    <button type="button" className="chip" onClick={() => setSelected(job)}>
                      details
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      )}

      {selected !== null && (
        <FailedJobDetail
          job={selected}
          onClose={() => setSelected(null)}
          controls={controls}
          onStoreChanged={() => setRevision((current) => current + 1)}
        />
      )}

      {!controls.retry && !controls.delete && (
        <div className="banner__row">
          <p className="muted">{controls.reason}</p>
          <RereadCapabilities reread={reread} canReread={canReread} />
        </div>
      )}
    </>
  )
}

// FailedJobDetail is the record as the application stores it. There is no outcome, attempt
// count or duration here: those belong to the attempt history, and this panel describes the
// record the two actions act on.
function FailedJobDetail({
  job,
  onClose,
  controls,
  onStoreChanged,
}: {
  job: FailedJob
  onClose: () => void
  controls: FailedJobControls
  onStoreChanged: () => void
}) {
  return (
    <section className="panel detail jobs__detail" aria-label="Failed job detail">
      <div className="detail__head">
        <h2>{failedJobClass(job)}</h2>
        <button type="button" className="chip" onClick={onClose}>
          close
        </button>
      </div>

      <dl className="detail__fields">
        <dt>Queue</dt>
        <dd>{job.queue}</dd>
        <dt>Connection</dt>
        <dd>{job.connection === '' ? 'unknown' : job.connection}</dd>
        <dt>Failed</dt>
        <dd>{failedAtText(job.failed_at)}</dd>
        <dt>UUID</dt>
        <dd className="mono">{job.uuid === '' ? 'none published' : job.uuid}</dd>
        <dt>Failed job id</dt>
        <dd className="mono">{job.id}</dd>
      </dl>

      {job.exception === '' ? (
        <p className="muted">No exception was recorded for this failed job.</p>
      ) : (
        <div className="detail__exception">
          <pre>{job.exception}</pre>
        </div>
      )}

      {(controls.retry || controls.delete) &&
        (job.uuid === '' ? (
          <p className="muted">
            This failed record carries no uuid the application knows, so there is nothing to retry
            or delete.
          </p>
        ) : (
          // Keyed by the record, so opening another row does not inherit the answer the
          // previous one got.
          <FailedJobActions
            key={job.id}
            job={job}
            canRetry={controls.retry}
            canDelete={controls.delete}
            reason={controls.reason}
            onStoreChanged={onStoreChanged}
          />
        ))}
    </section>
  )
}
