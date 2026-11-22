import type { HealthReport } from '../api'
import { routeHref } from '../routes'
import { buildOverview, supervisorSummary } from '../view'
import QueueTable from '../components/QueueTable'

export default function Overview({ report, degraded }: { report: HealthReport; degraded: boolean }) {
  const model = buildOverview(report, Date.now())

  return (
    <>
      <h1>Overview</h1>

      <section className="cards">
        <div className={model.healthy && !degraded ? 'card' : 'card card--warn'}>
          <span className="card__label">Status</span>
          <span className="card__value">{model.status}</span>
          {degraded && <span className="card__note">the service answered 503</span>}
        </div>
        <div className="card">
          <span className="card__label">Version</span>
          <span className="card__value">{model.version}</span>
          {model.application !== '' && <span className="card__note">{model.application}</span>}
        </div>
        <div className="card">
          <span className="card__label">Uptime</span>
          <span className="card__value">{model.uptime}</span>
        </div>
        <div className={model.lock.ok ? 'card' : 'card card--warn'}>
          <span className="card__label">Supervisor lock</span>
          <span className="card__value">{model.lock.label}</span>
          {model.lock.detail !== '' && <span className="card__note">{model.lock.detail}</span>}
        </div>
      </section>

      <section className="panel overview__workers">
        <h2>Workers</h2>
        <p className="muted panel__summary">{supervisorSummary(model.supervisors)}</p>
        <p>
          <a href={routeHref('workers')}>All workers</a>
        </p>
      </section>

      <section className="panel overview__queues">
        <h2>Queues</h2>
        {model.queueError !== null && (
          <p className="banner">Queue inspection failed: {model.queueError}</p>
        )}
        <QueueTable rows={model.queues} />
        <p>
          <a href={routeHref('queues')}>All queues</a>
        </p>
      </section>

      <section className="panel overview__telemetry">
        <h2>Telemetry</h2>
        <ul className="counters">
          {model.telemetry.map((row) => (
            <li key={row.label}>
              <span className="card__label">{row.label}</span>
              <span className="counters__value">{row.value}</span>
            </li>
          ))}
        </ul>
        {model.telemetryFailure !== null && (
          <p className="banner">Ingestion is failing: {model.telemetryFailure}</p>
        )}
      </section>
    </>
  )
}
