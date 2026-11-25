import { useEffect, useState } from 'react'
import type { Route } from './routes'
import { parseRoute, routeHref, routeLabel } from './routes'
import { useHealth } from './useHealth'
import { useSession } from './useSession'
import SignIn from './components/SignIn'
import Overview from './pages/Overview'
import Queues from './pages/Queues'
import Workers from './pages/Workers'
import Jobs from './pages/Jobs'
import Metrics from './pages/Metrics'

const navigation: Route[] = ['overview', 'queues', 'workers', 'jobs', 'failed', 'metrics']

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
  const session = useSession()

  // The session is read before a page is drawn. A service that wants a cookie answers 401 to
  // every read, and rendering those as read failures would describe a sign-in as a fault.
  if (session.status === 'checking') {
    return (
      <main className="shell__main">
        <p className="muted">Checking your session…</p>
      </main>
    )
  }
  if (session.status === 'signed-out') {
    return <SignIn onSignedIn={session.signIn} />
  }
  return (
    <Dashboard
      route={route}
      user={session.user}
      onSignOut={session.signOut}
      onExpired={session.expire}
    />
  )
}

interface DashboardProps {
  route: Route
  // user is the signed-in operator, and null on a service started without users, where
  // there is nobody to sign out.
  user: string | null
  onSignOut: () => void
  // onExpired drops to the sign-in screen: the health poll answers 401 when the session
  // ended, and a stale report is worse than asking again.
  onExpired: () => void
}

function Dashboard({ route, user, onSignOut, onExpired }: DashboardProps) {
  const health = useHealth(onExpired)
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
        {user !== null && (
          <button className="shell__signout" type="button" onClick={() => void onSignOut()}>
            Sign out ({user})
          </button>
        )}
        <span className={warn ? 'shell__status pill pill--warn' : 'shell__status pill'}>
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
        ) : route === 'jobs' ? (
          <Jobs report={report} canAct={user !== null} />
        ) : route === 'failed' ? (
          <Jobs report={report} pinnedOutcome="failed" canAct={user !== null} />
        ) : route === 'metrics' ? (
          <Metrics report={report} />
        ) : (
          <Overview report={report} degraded={health.degraded} />
        )}
      </main>
    </div>
  )
}
