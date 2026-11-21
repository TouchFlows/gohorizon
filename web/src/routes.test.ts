import { describe, expect, it } from 'vitest'
import { parseRoute, routeHref, routeLabel } from './routes'

describe('parseRoute', () => {
  it('reads the three routes', () => {
    expect(parseRoute('#/')).toBe('overview')
    expect(parseRoute('#/queues')).toBe('queues')
    expect(parseRoute('#/workers')).toBe('workers')
  })

  it('accepts the shapes a browser produces', () => {
    expect(parseRoute('')).toBe('overview')
    expect(parseRoute('#')).toBe('overview')
    expect(parseRoute('#/queues/')).toBe('queues')
  })

  it('falls back to the overview for anything unknown', () => {
    expect(parseRoute('#/jobs')).toBe('overview')
    expect(parseRoute('#/queues/extra')).toBe('overview')
  })
})

describe('routeHref', () => {
  it('round-trips through parseRoute', () => {
    for (const route of ['overview', 'queues', 'workers'] as const) {
      expect(parseRoute(routeHref(route))).toBe(route)
    }
    expect(routeHref('overview')).toBe('#/')
  })
})

describe('routeLabel', () => {
  it('names each route for the navigation', () => {
    expect(routeLabel('overview')).toBe('Overview')
    expect(routeLabel('queues')).toBe('Queues')
    expect(routeLabel('workers')).toBe('Workers')
  })
})
