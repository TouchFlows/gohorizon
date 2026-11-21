import { useEffect, useState } from 'react'
import type { Route } from './routes'
import { parseRoute, routeHref, routeLabel } from './routes'
import { useHealth } from './useHealth'
import Overview from './pages/Overview'
import Queues from './pages/Queues'
import Workers from './pages/Workers'

const navigation: Route[] = ['overview', 'queues', 'workers']

function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => parseRoute(window.location.hash))

  useEffect(() => {
    const update = () => setRoute(parseRoute(window.location.hash))
    window.addEventListener('hashchange', update)
    return () => window.removeEventListener('hashchange', update)
  }, [])

  return route
}

export default function App() {
  const route = useRoute()
  const health = useHealth()
  const report = health.report
  const warn = report !== null && (health.degraded || report.status !== 'ok')

  return (
    <div className="shell">
      <header className="shell__header">
        <span className="shell__brand">GoHorizon</span>
        <nav className="nav">
          {navigation.map((name) => (
            <a
              key={name}
              className={name === route ? 'nav__link nav__link--active' : 'nav__link'}
              href={routeHref(name)}
            >
              {routeLabel(name)}
            </a>
          ))}
        </nav>
        <span className={warn ? 'pill pill--warn' : 'pill'}>
          {report === null ? 'connecting' : report.status}
        </span>
      </header>

      <main className="shell__main">
        {health.error !== null && (
          <p className="banner">
            Refresh failed: {health.error}
            {report !== null && ' Showing the last report.'}
          </p>
        )}

        {report === null ? (
          health.error === null && <p className="muted">Waiting for the first report…</p>
        ) : route === 'queues' ? (
          <Queues report={report} />
        ) : route === 'workers' ? (
          <Workers report={report} />
        ) : (
          <Overview report={report} degraded={health.degraded} />
        )}
      </main>
    </div>
  )
}
