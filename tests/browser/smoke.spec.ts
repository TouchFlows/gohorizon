import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from '@playwright/test'

// The whole point of this suite is that it is not a component test: the pages below are
// served by the compiled binary, from the embedded bundle, over a store the worker wrote.
// This file starts that service in `beforeAll` and ends it in `afterAll`, so nothing has to
// travel between processes: the config runs one worker, and the worker owns the setup.

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const fixture = path.join(root, 'tests', 'fixtures', 'laravel-app')

// The acceptance suite's fixture talks to the same disposable Redis database, under its
// own application and queue names, so this smoke cannot collide with it or with a service
// an operator is running elsewhere.
const redis = { host: '127.0.0.1', port: 6379, db: 15 }
const application = 'gohorizon-smoke'
const queue = 'gohorizon-smoke'
const pollMs = 30_000

// The service and the fixture read the same environment: the worker it launches is the
// Laravel application, so both sides must agree on the connection and the queue.
const fixtureEnv: NodeJS.ProcessEnv = {
  APP_NAME: application,
  APP_ENV: 'e2e',
  APP_KEY: 'base64:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=',
  QUEUE_CONNECTION: 'redis',
  QUEUE_FAILED_DRIVER: 'null',
  REDIS_CLIENT: 'predis',
  REDIS_HOST: redis.host,
  REDIS_PORT: String(redis.port),
  REDIS_DB: String(redis.db),
  CACHE_STORE: 'file',
  SESSION_DRIVER: 'file',
  LOG_CHANNEL: 'errorlog',
  GOHORIZON_APPLICATION: application,
  GOHORIZON_ENVIRONMENT: 'e2e',
  GOHORIZON_REDIS_CONNECTION: 'default',
  GOHORIZON_TELEMETRY_BATCH: '1',
  REDIS_QUEUE_RETRY_AFTER: '10',
}

const php = commandOnPath('php')

// Why the smoke cannot run here, or null when it can. Everything is known when the file
// loads, so a machine without the flag, the fixture or PHP reports a skip instead of a
// failure — and CI fails on that skip rather than treating it as a pass.
const skipReason =
  process.env.GOHORIZON_E2E !== '1'
    ? 'set GOHORIZON_E2E=1 to run the browser smoke (see README)'
    : !fs.existsSync(path.join(fixture, 'vendor', 'autoload.php'))
      ? `fixture dependencies are not installed; run: cd ${fixture} && composer install`
      : php === null
        ? 'php is not on PATH'
        : null

let dir = ''
let base = ''
let service: ChildProcess | undefined
// failedAt is when the failing attempt finished, which is the minute the chart must draw it
// in: read from the API rather than from the clock the browser happens to see.
let failedAt = ''

test.describe('the dashboard against the real binary', () => {
  test.skip(skipReason !== null, skipReason ?? '')

  test.beforeAll(async () => {
    if (skipReason !== null || php === null) {
      return
    }
    // Building, the worker's first poll and a cold Chromium all happen outside a test body.
    test.setTimeout(120_000)

    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gohorizon-smoke-'))
    const binary = path.join(dir, 'gohorizon')
    execFileSync('go', ['build', '-o', binary, './cmd/gohorizon'], { cwd: root, stdio: 'inherit' })

    const port = await freePort()
    base = `http://127.0.0.1:${port}`
    const configPath = path.join(dir, 'gohorizon.yaml')
    fs.writeFileSync(configPath, configYAML(port, dir, php))

    const log = fs.openSync(path.join(dir, 'gohorizon.log'), 'a')
    service = spawn(binary, ['run', '-config', configPath], {
      cwd: root,
      // Its own group, so afterAll can end the worker the supervisor started as well.
      detached: true,
      stdio: ['ignore', log, log],
      env: { ...process.env, ...fixtureEnv },
    })
    fs.closeSync(log)

    await waitFor('report a queue depth', async () => {
      const report = (await getJSON(`${base}/api/v1/health`)) as { queues?: { depths?: unknown[] } }
      return Array.isArray(report.queues?.depths) && report.queues.depths.length > 0
    })

    dispatch(php, 'marker', path.join(dir, 'smoke-marker'))
    // A second job throws on purpose, so the failed screen has an attempt of its own to show.
    dispatch(php, 'fail')

    // The worker has to pick the jobs up and the recorder has to store them before a browser
    // can be asked to show them.
    await waitForJob('record the job the worker ran', `queue=${queue}`, (job) =>
      job.class === 'App\\Jobs\\RecordMarker',
    )

    const failed = await waitForJob(
      'record the exception the failed job threw',
      `queue=${queue}&outcome=failed`,
      (job) =>
        job.class === 'App\\Jobs\\AlwaysFails' &&
        job.exception?.message === 'the acceptance fixture always fails',
    )
    if (failed.finished_at === undefined || failed.finished_at === '') {
      throw new Error('the failed attempt is recorded without a finished_at')
    }
    failedAt = failed.finished_at
  })

  test.afterAll(() => {
    if (skipReason !== null) {
      return
    }
    stopService()
    fs.rmSync(dir, { recursive: true, force: true })
  })

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

  test('the failed page renders the attempt with the exception it threw', async ({ page }) => {
    await page.goto(`${base}/#/failed`)

    await expect(page.getByRole('heading', { name: 'Failed jobs' })).toBeVisible()
    await expect(page.getByRole('cell', { name: 'App\\Jobs\\AlwaysFails', exact: true })).toBeVisible()
    await expect(
      page.getByRole('cell', { name: 'RuntimeException: the acceptance fixture always fails' }),
    ).toBeVisible()
  })

  test('the metrics chart draws the attempts in the minute they ran', async ({ page }) => {
    await page.goto(`${base}/#/metrics`)

    await expect(page.getByRole('heading', { name: 'Metrics' })).toBeVisible()
    await expect(page.getByLabel('Queue')).toHaveValue('gohorizon-smoke')
    await expect(page.getByRole('button', { name: 'minute' })).toHaveAttribute('aria-pressed', 'true')

    // One processed attempt and one failed one, and nothing else in the window: the store is
    // fresh for every run, so any third attempt would mean a job ran twice.
    await expect(page.getByRole('img', { name: /, 2 jobs$/ })).toBeVisible()

    // The column is named by the minute the failure landed in, formatted by the page itself,
    // so this still asserts the right bucket when a run straddles a minute boundary.
    const minute = await page.evaluate(
      (finishedAt) =>
        new Date(finishedAt).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }),
      failedAt,
    )
    await expect(page.locator('svg rect', { hasText: `${minute} — failed: 1` })).toBeVisible()
  })

  test('a daily window is empty, and the chip says which resolution is drawn', async ({ page }) => {
    await page.goto(`${base}/#/metrics`)
    await expect(page.getByLabel('Queue')).toHaveValue('gohorizon-smoke')

    // Nothing is folded into the daily table until an hour has closed, so a store this young
    // has no daily rows: the page has to say that rather than draw a flat line at zero.
    await page.getByRole('button', { name: 'daily' }).click()

    await expect(page.getByRole('button', { name: 'daily' })).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', { name: 'minute' })).toHaveAttribute('aria-pressed', 'false')
    await expect(page.getByText('No data in this window.')).toBeVisible()
  })

  test('the metrics screen draws the average duration the store returned', async ({ page }) => {
    // The payload the page charts is the only honest source for the number it should draw,
    // so the test reads the response the page itself made.
    const [response] = await Promise.all([
      page.waitForResponse((candidate) => candidate.url().includes('/api/v1/metrics') && candidate.ok()),
      page.goto(`${base}/#/metrics`),
    ])
    const history = (await response.json()) as {
      resolution: 'minute' | 'hourly' | 'daily'
      buckets: {
        start: string
        processed: number
        released: number
        failed: number
        timed_out: number
        duration_ms: number
      }[]
    }

    // The failure is the attempt this smoke knows the time of, so its bucket is the one that
    // has to carry a duration column.
    const finishedAt = Date.parse(failedAt) / 1000
    const step = { minute: 60, hourly: 3600, daily: 86400 }[history.resolution]
    const bucket = history.buckets.find((entry) => {
      const start = Date.parse(entry.start) / 1000
      return start <= finishedAt && finishedAt < start + step
    })
    if (bucket === undefined) {
      throw new Error('the window the page charted does not hold the failed attempt')
    }

    const attempts = bucket.processed + bucket.released + bucket.failed + bucket.timed_out
    const average = attempts === 0 ? 0 : Math.round(bucket.duration_ms / attempts)
    const label = await page.evaluate(
      (start) =>
        new Date(start * 1000).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }),
      Date.parse(bucket.start) / 1000,
    )

    const durationChart = page.getByRole('img', { name: /Average duration per attempt/ })
    await expect(durationChart).toBeVisible()
    // Counted rather than required visible: a job that finished in under a millisecond is a
    // zero-height column, and this assertion is about the number it labels.
    await expect(
      durationChart.locator('rect', { hasText: `${label} — average ${average} ms` }),
    ).toHaveCount(1)
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

// dispatch queues one fixture job on the smoke's own queue, in the environment the service
// and its worker read.
function dispatch(phpPath: string, ...args: string[]): void {
  execFileSync(phpPath, ['dispatch.php', ...args], {
    cwd: fixture,
    env: { ...process.env, ...fixtureEnv, GOHORIZON_E2E_QUEUE: queue },
    stdio: 'inherit',
  })
}

function commandOnPath(name: string): string | null {
  try {
    return execFileSync('sh', ['-c', `command -v ${name}`], { encoding: 'utf8' }).trim() || null
  } catch {
    return null
  }
}

function configYAML(port: number, dir: string, php: string): string {
  return `version: 1

server:
  listen: "127.0.0.1:${port}"

storage:
  driver: sqlite
  path: ${JSON.stringify(path.join(dir, 'metrics.db'))}

redis:
  address: "${redis.host}:${redis.port}"
  password_env: GOHORIZON_REDIS_PASSWORD
  db: ${redis.db}

applications:
  - id: ${application}
    name: GoHorizon browser smoke fixture
    path: ${JSON.stringify(fixture)}
    php: ${JSON.stringify(php)}
    artisan: artisan
    environment: e2e
    redis_connection: redis
    supervisors:
      - id: smoke
        queues:
          - ${queue}
        balance:
          strategy: fixed
        workers:
          min: 1
          max: 1
        worker:
          sleep: 1
          tries: 1
          timeout: 6
          memory: 128
        shutdown_grace: 30
        restart_backoff: 1
`
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = net.createServer()
    server.on('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      const port = typeof address === 'object' && address !== null ? address.port : 0
      server.close(() => (port === 0 ? reject(new Error('no free port')) : resolve(port)))
    })
  })
}

async function getJSON(url: string): Promise<unknown> {
  const response = await fetch(url, { signal: AbortSignal.timeout(3_000) })
  if (!response.ok) {
    throw new Error(`${url} returned ${response.status}`)
  }
  return response.json()
}

// waitFor polls until the condition holds, so a slow worker or a slow first inspect is a
// longer wait rather than a flaky test. Its failure carries the service log, which is where
// a store it cannot read or a worker that never started says so.
async function waitFor(what: string, condition: () => Promise<boolean>): Promise<void> {
  const deadline = Date.now() + pollMs
  let last = ''
  while (Date.now() < deadline) {
    try {
      if (await condition()) {
        return
      }
    } catch (failure) {
      last = String(failure)
    }
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  throw new Error(`the service did not ${what} within ${pollMs} ms (${last})\n${readLog()}`)
}

// RecordedJob is the part of a stored attempt these waits read: that the worker ran this
// class at all, what it threw when it failed, and when it finished.
interface RecordedJob {
  class?: string
  finished_at?: string
  exception?: { message?: string } | null
}

// waitForJob polls the job endpoint until one recorded attempt matches and returns it,
// because a browser cannot be asked to show a row the recorder has not written yet.
async function waitForJob(
  what: string,
  query: string,
  matches: (job: RecordedJob) => boolean,
): Promise<RecordedJob> {
  await waitFor(what, async () => (await recordedJobs(query)).some(matches))

  const match = (await recordedJobs(query)).find(matches)
  if (match === undefined) {
    throw new Error(`the service did ${what}, but its page no longer lists it`)
  }
  return match
}

async function recordedJobs(query: string): Promise<RecordedJob[]> {
  const page = (await getJSON(`${base}/api/v1/jobs?${query}&limit=10`)) as { jobs?: RecordedJob[] }
  return page.jobs ?? []
}

function readLog(): string {
  try {
    return fs.readFileSync(path.join(dir, 'gohorizon.log'), 'utf8').split('\n').slice(-40).join('\n')
  } catch {
    return ''
  }
}

// stopService ends the service's process group: the supervisor handles SIGTERM by stopping
// its worker, and the group kill is what guarantees no worker of the smoke survives.
function stopService(): void {
  const pid = service?.pid
  if (pid === undefined) {
    return
  }
  try {
    process.kill(-pid, 'SIGTERM')
  } catch {
    return
  }
  const deadline = Date.now() + 20_000
  while (Date.now() < deadline) {
    try {
      process.kill(-pid, 0)
    } catch {
      return
    }
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 200)
  }
  try {
    process.kill(-pid, 'SIGKILL')
  } catch {
    // already gone
  }
}
