// The dashboard has three routes and no router dependency: the hash is the route, and the
// server already serves the shell for any client path (docs/adr/0008).
export type Route = 'overview' | 'queues' | 'workers' | 'jobs' | 'failed' | 'metrics'

const routes: Route[] = ['overview', 'queues', 'workers', 'jobs', 'failed', 'metrics']

// parseRoute reads a location hash such as "#/queues". Anything unknown is the overview, so
// an old bookmark or a typo still lands somewhere useful.
export function parseRoute(hash: string): Route {
  const name = hash.replace(/^#\/?/, '').replace(/\/$/, '')
  return routes.find((route) => route === name) ?? 'overview'
}

export function routeHref(route: Route): string {
  return route === 'overview' ? '#/' : `#/${route}`
}

export function routeLabel(route: Route): string {
  switch (route) {
    case 'queues':
      return 'Queues'
    case 'workers':
      return 'Workers'
    case 'jobs':
      return 'Jobs'
    case 'failed':
      return 'Failed'
    case 'metrics':
      return 'Metrics'
    default:
      return 'Overview'
  }
}
