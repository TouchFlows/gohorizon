import type { HealthReport } from '../api'
import { queueError, queueRows } from '../view'
import QueueTable from '../components/QueueTable'

export default function Queues({ report }: { report: HealthReport }) {
  const rows = queueRows(report.queues.depths, Date.now())
  const error = queueError(report.queues)

  return (
    <>
      <h1>Queues</h1>
      {report.queues.connection !== undefined && report.queues.connection !== '' && (
        <p className="muted">Inspected through the {report.queues.connection} connection.</p>
      )}
      {error !== null && <p className="banner">Queue inspection failed: {error}</p>}
      <section className="panel">
        <QueueTable rows={rows} />
      </section>
      <p className="muted">
        A count the driver could not report is shown as unknown, never as zero. The oldest
        pending age is measured against this browser's clock.
      </p>
    </>
  )
}
