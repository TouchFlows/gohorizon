import type { Capabilities, HealthReport } from '../api'
import { workerControls } from '../capabilities'
import { buildWorkers, countNoun } from '../view'
import RestartWorker from '../components/RestartWorker'

// canAct is whether this deployment has administrative routes at all: a service started
// without users mounts none. capabilities is what the application supports, which decides
// whether a restart is offered at all (docs/adr/0014); while it is still being read, the
// session alone decides.
export default function Workers({
  report,
  canAct = false,
  capabilities = null,
}: {
  report: HealthReport
  canAct?: boolean
  capabilities?: Capabilities | null
}) {
  const supervisors = buildWorkers(report)
  const controls = workerControls(capabilities, canAct)

  return (
    <>
      <h1>Workers</h1>
      {supervisors.length === 0 && <p className="muted">No supervisor has reported yet.</p>}

      {supervisors.map((supervisor) => (
        <section key={supervisor.id} className="panel workers__supervisor">
          <h2>
            {supervisor.id}{' '}
            <span className="muted">
              {supervisor.running} of {countNoun(supervisor.desired, 'worker')} running
              {supervisor.crashed > 0 && `, ${supervisor.crashed} crashed`}
              {supervisor.escalations > 0 && `, ${supervisor.escalations} escalated`}
            </span>
          </h2>

          {supervisor.workers.length === 0 ? (
            <p className="muted">This supervisor has no workers yet.</p>
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>Worker</th>
                  <th>Queue</th>
                  <th className="num">PID</th>
                  <th>State</th>
                  <th className="num">Restarts</th>
                  <th>Last exit</th>
                  {controls.restart && <th>Action</th>}
                </tr>
              </thead>
              <tbody>
                {supervisor.workers.map((worker) => (
                  <tr key={worker.uuid}>
                    <td className="mono">{worker.uuid}</td>
                    <td>{worker.queue}</td>
                    <td className="num">{worker.pid}</td>
                    <td>
                      {worker.state}
                      {worker.crashLooping && <span className="badge badge--bad">crash looping</span>}
                      {worker.adopted && <span className="badge">adopted</span>}
                    </td>
                    <td className="num">{worker.restarts}</td>
                    <td>
                      {worker.lastExit}
                      {worker.reason !== '' && <span className="muted"> {worker.reason}</span>}
                    </td>
                    {controls.restart && (
                      <td className="workers__action">
                        <RestartWorker uuid={worker.uuid} />
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      ))}

      {!controls.restart && <p className="muted">{controls.reason}</p>}
    </>
  )
}
