import { expect, test } from '@playwright/test'
import { readState } from './state'

// The whole point of this suite is that it is not a component test: the pages below are
// served by the compiled binary, from the embedded bundle, over a store the worker wrote.
const state = readState()

test.describe('the dashboard against the real binary', () => {
  test.skip(state.skipped, state.reason ?? 'set GOHORIZON_E2E=1 to run the browser smoke')

  const base = state.url ?? ''

  test('the overview renders the service and the queue it supervises', async ({ page }) => {
    await page.goto(`${base}/`)

    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()
    await expect(page.locator('.pill')).toHaveText('ok')
    await expect(page.getByRole('cell', { name: 'gohorizon-smoke', exact: true })).toBeVisible()
    await expect(page.getByText('EVENTS')).toBeVisible()
  })

  test('the jobs page renders the job the worker just ran', async ({ page }) => {
    await page.goto(`${base}/#/jobs`)

    await expect(page.getByRole('heading', { name: 'Jobs' })).toBeVisible()
    await expect(page.getByText('App\\Jobs\\RecordMarker')).toBeVisible()
    await expect(page.getByRole('cell', { name: 'processed', exact: true })).toBeVisible()
  })

  test('a deep link outside the navigation loads the shell instead of a 404', async ({ page }) => {
    const response = await page.goto(`${base}/queues/anything`)

    // The server answers the bundle's shell for a client route; it is the hash router, not
    // the server, that decides which screen that is.
    expect(response?.status()).toBe(200)
    await expect(page.getByRole('navigation')).toBeVisible()
    await expect(page.getByRole('link', { name: 'Jobs' })).toBeVisible()
  })
})
