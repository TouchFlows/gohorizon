import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test, type Page, type Route } from '@playwright/test'

// The whole point of this suite is that it is not a component test: the pages below are
// served by the compiled binary, from the embedded bundle, over a store the worker wrote.
// This file starts that service in `beforeAll` and ends it in `afterAll`, so nothing has to
// travel between processes: the config runs one worker, and the worker owns the setup.
//
// Hooks. An element the smoke has to find carries a second class beside its styling ones,
// named `<area>__<part>` and read as a class selector, as in `.jobs__detail` or
// `.outcome-chart__scale`. It is BEM-ish and appended rather than swapped in
// (`panel detail jobs__detail`, `shell__status pill pill--warn`), so the hook says what the
// element is and not how it is drawn, and restyling cannot move the smoke off it. There is
// no data-testid here: the class attribute is the only marker the pages carry, which is why
// a hook must not read like a style — a styling class can be renamed freely, a hook only
// with its reader.
//
// A hook exists exactly as long as a reader does. It is added when a test starts reading
// that element and deleted when the last one stops, so the name and the only test that reads
// it always land in the same commit; a hook nothing reads is dead markup that still looks
// load-bearing. The eight read today are .shell__status, .overview__workers,
// .panel__summary, .workers__supervisor, .jobs__detail, .outcome-chart__total,
// .outcome-chart__scale and .duration-chart__scale. Everything else in a className is
// styling. Where the assertion is about what a reader perceives — a heading, a labelled
// control, a field's label beside its value — the element is found by role, accessible name
// or structure instead, which is why most elements carry no hook at all.
//
// Coverage. This is not a checklist of every branch, and the cases below should not be read as
// though the suite drew all of them. Nothing here claims the suite is complete: it has claimed
// that three times and been wrong each time — the paragraph said nothing was left unread while
// the waiting lines on the Jobs and Metrics screens were drawn by the pages and read by nobody.
// What follows is where the work lives, not where it ends.
//
// A state that needs the store itself to break is dressed in the browser: a health answer is
// rewritten on its way through, /api/v1/jobs and /api/v1/metrics are answered on the way
// through, and the answers the two history screens wait on are held at the socket until the
// line they stand in for has been read. The pills, banners and waiting lines below are read
// against payloads this file wrote rather than phrases the page carries. All of it goes through
// the one `dress` helper at the bottom: a case names the endpoints it is dressing and turns the
// answer it wants read, so no case carries route plumbing of its own. The models behind them are
// read in web/src/view.test.ts, and the poll loop in web/src/useHealth.test.ts under fake timers,
// where its 5 s re-arm and the abort that ends it on unmount can be timed.
//
// Sign-in is the one state the whole suite needs rather than one case reaching for it: the API
// answers 401 without a session cookie, so every case is signed in from `beforeEach` and the
// one case that starts where an operator starts clears its own cookies, one ends its session
// under a loaded page to read the drop back to the form, one signs in as the viewer the
// configuration also carries to read the refusal a role below operator gets, and one reads the
// audit records a sign-out and a sign-in leave in the service log — the one durable trace of a
// session action that until now only the Go tests had read. The credential is a password written here that
// the binary's own `passwd` hashes in `beforeAll`; no hash and no session key belongs in this
// file.
//
// What that leaves is a rule rather than a list: a branch the pages draw and no case reaches
// belongs in this paragraph, named, because staying unnamed is how it goes unread.

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const fixture = path.join(root, 'tests', 'fixtures', 'laravel-app')

// The acceptance suite's fixture talks to the same disposable Redis database, under its
// own application and queue names, so this smoke cannot collide with it or with a service
// an operator is running elsewhere.
const redis = { host: '127.0.0.1', port: 6379, db: 15 }
const application = 'gohorizon-smoke'
const queue = 'gohorizon-smoke'
// A second queue, served by a two-worker supervisor: one page then carries a lone worker and
// a pool, which is the only way both ratio spellings reach a browser.
const extraQueue = 'gohorizon-smoke-extra'
// A third supervisor, one idle worker, whose worker this smoke kills: a SIGKILL is a crash
// to the supervisor, which replaces it with a growing backoff until the streak is a loop.
const crashy = 'crashy'
const crashyQueue = 'gohorizon-smoke-crashy'
const pollMs = 30_000

// The operator the smoke signs in as. The password is hashed by the binary itself before the
// service starts, and the key that signs the session cookie is an environment variable like
// the deployment's own; neither a hash nor a key is written into this file.
const smokeUser = 'smoke'
// A second user below the operator role the administrative routes require, so the refusal a
// role gets can be read from the page rather than only from the Go tests. One password is
// hashed once and serves both.
const viewerUser = 'viewer'
const smokePassword = 'smoke-password'
const sessionCookie = 'gohorizon_session'
// The double-submit token a mutation has to echo. It is not HttpOnly, because the page reads it
// from its own jar, and this file reads it the same way.
const csrfCookie = 'gohorizon_csrf'
// At least 32 bytes, which is the length the service refuses to start below.
const sessionKey = 'gohorizon-smoke-session-key-0123456789abcdef'

// The service and the fixture read the same environment: the worker it launches is the
// Laravel application, so both sides must agree on the connection and the queue.
const fixtureEnv: NodeJS.ProcessEnv = {
  APP_NAME: application,
  APP_ENV: 'e2e',
  APP_KEY: 'base64:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=',
  QUEUE_CONNECTION: 'redis',
  // The failing fixture job is retried through the API, so the application has to keep it: a
  // `null` failer discards it and `queue:retry` would have nothing to push back. The file
  // failer is Laravel's own, in the fixture's storage, and no other suite shares this file.
  QUEUE_FAILED_DRIVER: 'file',
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
// session is the token the suite's direct calls to the API carry. The browser gets its own
// cookie of the same shape, set on its context before each case.
let session = ''
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
    const passwordHash = execFileSync(binary, ['passwd', '-password', smokePassword], {
      encoding: 'utf8',
    }).trim()
    fs.writeFileSync(configPath, configYAML(port, dir, php, passwordHash))

    const log = fs.openSync(path.join(dir, 'gohorizon.log'), 'a')
    service = spawn(binary, ['run', '-config', configPath], {
      cwd: root,
      // Its own group, so afterAll can end the worker the supervisor started as well.
      detached: true,
      stdio: ['ignore', log, log],
      env: { ...process.env, ...fixtureEnv, GOHORIZON_SMOKE_SESSION_KEY: sessionKey },
    })
    fs.closeSync(log)

    // The listener is up before anything can be read, and every read below needs a session, so
    // the sign-in is what the first wait is for: it retries until the service answers it.
    await waitFor('accept a sign-in', async () => {
      session = await signIn()
      return true
    })

    await waitFor('report a queue depth', async () => {
      const report = (await getJSON(`${base}/api/v1/health`)) as { queues?: { depths?: unknown[] } }
      return Array.isArray(report.queues?.depths) && report.queues.depths.length > 0
    })

    // Both supervisors at their configured size before anything is read back: the ratios and
    // the aggregation word the pool, not a pool still starting.
    await waitFor('start every worker', async () => {
      const report = (await getJSON(`${base}/api/v1/health`)) as HealthReport
      const supervisors = report.supervisors ?? []
      return (
        supervisors.length === 3 &&
        supervisors.every((supervisor) => supervisor.running === supervisor.desired)
      )
    })

    // A previous run's failed jobs would be leftovers from another suite; this run starts
    // with an empty file so the job it retries is the one this setup dispatched.
    fs.rmSync(path.join(fixture, 'storage', 'framework', 'cache', 'failed-jobs.json'), {
      force: true,
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

  // Every case runs signed in, which is what the pages need from the API they read. A fresh
  // session per case is also what keeps the case that signs out from signing the rest out:
  // signing out revokes the token itself, not only the browser's copy of it.
  test.beforeEach(async ({ context }) => {
    if (skipReason !== null) {
      return
    }
    session = await signIn()
    await context.addCookies([{ name: sessionCookie, value: session, url: base }])
  })

  test('a dashboard with no session asks for a sign-in and takes one', async ({ page, context }) => {
    // Every other case is signed in by the hook above; this one starts where an operator with
    // no cookie starts, which is the screen the service's 401s lead to.
    await context.clearCookies()

    await page.goto(`${base}/`)
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible()
    // Nothing of the dashboard is drawn behind the form: its navigation and its status pill
    // are what a signed-out browser must not be given.
    await expect(page.getByRole('navigation')).toHaveCount(0)
    await expect(page.locator('.shell__status')).toHaveCount(0)

    // A wrong password is refused with the service's own answer, and the form stays where it
    // was rather than claiming a session it did not get.
    await page.getByLabel('Name').fill(smokeUser)
    await page.getByLabel('Password').fill('not-the-password')
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page.getByText('That name and password do not match.')).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible()
    expect((await page.request.get(`${base}/api/v1/health`)).status()).toBe(401)

    // The right one opens the dashboard on the report the service hands the page after it.
    await expect(page.getByLabel('Password')).toHaveValue('')
    await page.getByLabel('Password').fill(smokePassword)
    const [health] = await Promise.all([
      page.waitForResponse((candidate) => candidate.url().includes('/api/v1/health') && candidate.ok()),
      page.getByRole('button', { name: 'Sign in', exact: true }).click(),
    ])
    const report = (await health.json()) as HealthReport

    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()
    await expect(page.locator('.shell__status')).toHaveText(report.status)
    expect((await page.request.get(`${base}/api/v1/health`)).status()).toBe(200)

    // Signing out ends it on both sides: the pages give way to the form again, and the token
    // that was in the browser's jar no longer answers anything even if it is presented again.
    await page.getByRole('button', { name: /^Sign out/ }).click()
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible()
    expect((await page.request.get(`${base}/api/v1/health`)).status()).toBe(401)
  })

  test('a session that ends mid-watch drops back to the sign-in screen', async ({ page }) => {
    const report = await readHealth(page, `${base}/`)
    await expect(page.getByRole('navigation')).toBeVisible()
    await expect(page.locator('.shell__status')).toHaveText(report.status)

    // The session ends under the page: signing out revokes the token, so the cookie the
    // browser holds stops being honoured while the screen is still polling. The token is the
    // one `beforeEach` put on this context, presented here directly, so the revocation is the
    // service's own act rather than an answer dressed in the browser.
    const ended = await fetch(`${base}/api/v1/session`, {
      method: 'DELETE',
      headers: { Cookie: `${sessionCookie}=${session}` },
    })
    expect(ended.status).toBe(204)

    // The token is dead before the page asks again, so the 401 its next poll reads is the
    // service refusing a session that ended, not a request this file broke.
    expect((await page.request.get(`${base}/api/v1/health`)).status()).toBe(401)

    // The next poll drops the dashboard instead of reporting a failed read: the page does not
    // claim the service is unreachable, which is the difference between this and a 500.
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible({
      timeout: 20_000,
    })
    await expect(page.getByRole('navigation')).toHaveCount(0)
    await expect(page.getByText('Refresh failed:')).toHaveCount(0)

    // The way back is the same door: signing in again restores the pages the drop removed.
    await page.getByLabel('Name').fill(smokeUser)
    await page.getByLabel('Password').fill(smokePassword)
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()
    await expect(page.getByRole('navigation')).toBeVisible()
  })

  test('signing out and back in leaves an audit record of each', async ({ page }) => {
    // The record is the only durable trace a session action leaves, so this reads it where an
    // operator would: the service log. Both actions go through the page, because the record
    // being checked is the one an operator's own action left rather than one a direct call in
    // this file made.
    await readHealth(page, `${base}/`)
    const before = auditLines().length

    await page.getByRole('button', { name: /^Sign out/ }).click()
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible()

    await page.getByLabel('Name').fill(smokeUser)
    await page.getByLabel('Password').fill(smokePassword)
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()

    // The service writes the record before it answers, so both are in the file by now; the
    // wait covers reading a file another process writes, not a missing write.
    await waitFor('write both audit records', async () => auditLines().length >= before + 2)

    const written = auditLines().slice(before)
    const signOut = written[written.length - 2]
    const signIn = written[written.length - 1]

    // The role is the one the configuration gives this user, read at the moment of the action
    // rather than remembered from whenever the session started.
    expect(signOut).toMatchObject({
      action: 'sign-out',
      outcome: 'allowed',
      user: smokeUser,
      role: 'operator',
    })
    expect(signIn).toMatchObject({
      action: 'sign-in',
      outcome: 'allowed',
      user: smokeUser,
      role: 'operator',
    })
    // The address is the client the service resolves, which on loopback is the browser itself.
    expect(signIn.address).toBe('127.0.0.1')
  })

  test('an operator restarts a worker and the restart is recorded', async ({ page }) => {
    // The target is a worker on the idle pool rather than the one the smoke's jobs run
    // through, so the restart interrupts nothing.
    const { pool, target } = idlePoolWorker(await readHealth(page, `${base}/#/workers`))

    const token = await csrfToken(page)
    const before = auditLines().length

    // The request carries the browser's session cookie and the token from its jar: the two
    // things the endpoint asks for, presented by the browser rather than borrowed here.
    const response = await page.request.post(`${base}/api/v1/workers/${target.uuid}/restart`, {
      headers: { 'X-CSRF-Token': token },
    })
    expect(response.status()).toBe(202)
    expect(await response.json()).toEqual({ worker: target.uuid, supervisor: pool.id })

    await waitFor('replace the restarted worker', () => recycled(pool, target))

    // The record is the one durable trace the action leaves, read from the service log where an
    // operator would find it.
    await waitFor('write the restart audit record', async () => restartRecord(before) !== undefined)
    const record = restartRecord(before)
    expect(record).toMatchObject({
      action: 'worker-restart',
      outcome: 'allowed',
      user: smokeUser,
      role: 'operator',
      detail: `supervisor ${pool.id}`,
    })
    expect(record?.address).toBe('127.0.0.1')
  })

  test('an operator restarts a worker from its row', async ({ page }) => {
    const { pool, target } = idlePoolWorker(await readHealth(page, `${base}/#/workers`))

    // The row is found by the uuid it prints and the control by its label, so neither needs a
    // hook: a role and a name already pick them out.
    const row = page.getByRole('row', { name: new RegExp(target.uuid) })
    await expect(row).toBeVisible()

    const before = auditLines().length
    await row.getByRole('button', { name: 'Restart', exact: true }).click()
    await expect(row.getByText('restart requested')).toBeVisible()

    await waitFor('replace the restarted worker', () => recycled(pool, target))

    // The record is the one durable trace the button leaves, written by the service the page
    // asked rather than by this file.
    await waitFor('write the restart audit record', async () => restartRecord(before) !== undefined)
    expect(restartRecord(before)).toMatchObject({
      action: 'worker-restart',
      outcome: 'allowed',
      user: smokeUser,
      role: 'operator',
      detail: `supervisor ${pool.id}`,
    })
  })

  test('a viewer is refused a restart with the service wording', async ({ page, context }) => {
    // The role gates the call rather than the screen, so a viewer is offered the control and
    // gets the service's own refusal; the page has to repeat that wording, not invent one. This
    // case signs in as the viewer instead of using the session the hook put on the context.
    await context.clearCookies()
    await page.goto(`${base}/`)
    await page.getByLabel('Name').fill(viewerUser)
    await page.getByLabel('Password').fill(smokePassword)
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()

    const { target } = idlePoolWorker(await readHealth(page, `${base}/#/workers`))
    const row = page.getByRole('row', { name: new RegExp(target.uuid) })
    await row.getByRole('button', { name: 'Restart', exact: true }).click()

    // `operator role required` is the wording the route's minimum produces. It arrives in place
    // of an answer, and the control is offered again because nothing was done to the worker.
    await expect(row.getByText('operator role required')).toBeVisible()
    await expect(row.getByText('restart requested')).toHaveCount(0)
    await expect(row.getByRole('button', { name: 'Restart', exact: true })).toBeEnabled()
  })

  test('the overview renders the service, its queues and its telemetry', async ({ page }) => {
    // Every number below comes from the health payload the page itself fetched, so this reads
    // the page against its own response instead of against literals. Both panels are found by
    // their heading rather than by a hook: an operator reads them by that heading too, and a
    // hook is only for what a heading or a role cannot pick out.
    const report = await readHealth(page, `${base}/`)

    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()
    // The status is read through the header's own hook, not through the pill's styling class.
    await expect(page.locator('.shell__status')).toHaveText(report.status)

    // One row per depth the driver inspected, with the counts it reported. A count it could
    // not sample reads "unknown", which is why the cells go through numberCell. The oldest
    // pending age is formatted from the browser's clock, so its shape is asserted on the
    // queues screen, where this same table is drawn, rather than twice here.
    const depths = report.queues.depths ?? []
    expect(depths.map((depth) => depth.queue).sort()).toEqual([crashyQueue, extraQueue, queue].sort())
    const queues = page.locator('section', {
      has: page.getByRole('heading', { name: 'Queues', exact: true }),
    })
    await expect(queues.locator('tbody tr')).toHaveCount(depths.length)
    for (const depth of depths) {
      const cells = queues
        .getByRole('row')
        .filter({ has: page.getByRole('cell', { name: depth.queue, exact: true }) })
        .getByRole('cell')
      await expect(cells.nth(0)).toHaveText(depth.queue)
      await expect(cells.nth(1)).toHaveText(numberCell(depth.pending))
      await expect(cells.nth(2)).toHaveText(numberCell(depth.reserved))
      await expect(cells.nth(3)).toHaveText(numberCell(depth.delayed))
    }

    // The counters are the ingestion tallies from that same payload, one row per label the
    // page words. The failure banner beside them is read in `the overview and queues screens
    // name what could not report` below, from an answer that reports ingestion failing.
    const counters = page
      .locator('section', { has: page.getByRole('heading', { name: 'Telemetry', exact: true }) })
      .locator('li')
    const tallies = [
      ['events', report.telemetry.events],
      ['duplicates', report.telemetry.duplicates],
      ['invalid', report.telemetry.invalid],
      ['errors', report.telemetry.errors],
    ] as const
    await expect(counters).toHaveCount(tallies.length)
    for (const [label, value] of tallies) {
      // The label and its value are separate spans, so the row is picked out by the label and
      // the value is then read on its own rather than as part of the row's text.
      const row = counters.filter({ hasText: label })
      await expect(row).toHaveCount(1)
      await expect(row.getByText(String(value), { exact: true })).toBeVisible()
    }
  })

  test('a failed refresh keeps the last report and says why it is old', async ({ page }) => {
    const report = await readHealth(page, `${base}/`)
    const depths = report.queues.depths ?? []
    if (depths.length === 0) {
      throw new Error('the health report listed no queues for the failed poll to keep on screen')
    }
    await expect(page.locator('.shell__status')).toHaveText(report.status)

    // The service is healthy for every other case, so the poll is broken from the browser
    // side: the page's next health request answers 500, and the polls after it go through.
    // Intercepting the request the page itself makes is the only way to fail one without
    // breaking the store for the cases that follow.
    const health = await dress(page, '**/api/v1/health')
    health.answer({ kind: 'status', status: 500 })

    // The reason is the page's own wording for a status it will not read, and what stays on
    // screen is the report whose numbers were just read: a blip may not blank the page.
    const banner = page.getByText('Refresh failed: /api/v1/health returned 500')
    await expect(banner).toBeVisible()
    await expect(banner).toContainText('Showing the last report.')
    await expect(page.locator('.shell__status')).toHaveText(report.status)
    const kept = page
      .locator('section', { has: page.getByRole('heading', { name: 'Queues', exact: true }) })
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: depths[0].queue, exact: true }) })
    await expect(kept).toHaveCount(1)

    // The banner describes the refresh, not the report, so the next answer ends it.
    health.answer({ kind: 'through' })
    await expect(banner).toHaveCount(0)
  })

  test('the status pill warns when the service reports trouble', async ({ page }) => {
    const report = await readHealth(page, `${base}/`)
    const pill = page.locator('.shell__status')
    await expect(pill).toHaveText(report.status)
    await expect(pill).not.toHaveClass(/pill--warn/)

    // The service this suite starts is healthy for every other case, so the answers that make
    // the pill warn are dressed in the browser: each poll is forwarded and its own body comes
    // back with a different status line. The pill's text stays the status the page was told,
    // so both arms are read against the payload served here rather than a phrase written into
    // the page.
    const health = await dress(page, '**/api/v1/health')
    // A degraded lock answers 503 with its report still in the body, so the page has to warn
    // without any status string changing.
    health.answer({ kind: 'report', status: 503, edit: (body) => body })

    // One poll answers 503: the pill warns while its text is still the report's own status.
    await expect(pill).toHaveClass(/pill--warn/)
    await expect(pill).toHaveText(report.status)

    // The next answers 200 with a status that is not ok: the warning comes with the text.
    health.answer({ kind: 'report', edit: (body) => ({ ...body, status: 'shaky' }) })
    await expect(pill).toHaveText('shaky')
    await expect(pill).toHaveClass(/pill--warn/)

    // A healthy answer again drops the warning rather than leaving it stuck on.
    health.answer({ kind: 'through' })
    await expect(pill).toHaveText(report.status)
    await expect(pill).not.toHaveClass(/pill--warn/)
  })

  test('the shell says it is connecting until the first report arrives', async ({ page }) => {
    // The service is healthy and answers in milliseconds, so the state the shell draws without
    // a report would be gone before a reader saw it. The page's own first request is held at
    // the socket instead: nothing sleeps, and the test releases the answer once it has read
    // what is on screen.
    let releaseFirst!: () => void
    const firstAnswer = new Promise<void>((resolve) => {
      releaseFirst = resolve
    })
    // Every answer fails until the case is ready for one to succeed: a state that only lasts
    // until the next poll would be read on a timer, not by this test. The first request is
    // held, and it is the failure it is released into.
    const health = await dress(page, '**/api/v1/health')
    health.answer({ kind: 'status', status: 500, until: firstAnswer })

    const pill = page.locator('.shell__status')
    const waiting = page.getByText('Waiting for the first report…')
    await page.goto(`${base}/`)

    // Nothing has answered: the pill is the word for that, without the warn class, and the
    // shell says what it is waiting for.
    await expect(pill).toHaveText('connecting')
    await expect(pill).not.toHaveClass(/pill--warn/)
    await expect(waiting).toBeVisible()

    releaseFirst()

    // The refresh failed and there is still no report: the reason takes the waiting line's
    // place, the banner carries no suffix because there is no report to keep, and the pill is
    // still waiting for a first answer rather than naming one.
    await expect(page.getByText('Refresh failed: /api/v1/health returned 500', { exact: true })).toBeVisible()
    await expect(waiting).toHaveCount(0)
    await expect(pill).toHaveText('connecting')

    // One poll is let through, and the status the pill then names is the one that answer
    // carried.
    health.answer({ kind: 'through' })
    const response = await page.waitForResponse(
      (candidate) => candidate.url().includes('/api/v1/health') && candidate.ok(),
    )
    const report = (await response.json()) as HealthReport
    await expect(pill).toHaveText(report.status)
    await expect(pill).not.toHaveClass(/pill--warn/)
    await expect(waiting).toHaveCount(0)
  })

  test('the overview and queues screens name what could not report', async ({ page }) => {
    await readHealth(page, `${base}/`)

    // Both reasons are written into the answer the page is given below, so the banners are
    // read against this file's words rather than a phrase the page carries.
    const ingestion = 'redis is down'
    const inspection = 'goHorizon:inspect failed'

    // Nothing in the report this run just rendered has failed, so neither line is drawn, not
    // even with an empty reason: what appears below appears because the payload changed.
    await expect(page.getByText('Ingestion is failing:')).toHaveCount(0)
    await expect(page.getByText('Queue inspection failed:')).toHaveCount(0)

    // The service is healthy for every other case, so the failure is dressed in the browser:
    // each poll is forwarded and its own report comes back with ingestion failing and with the
    // inspector reporting an error, in the shape the API sends them. The rest of the report is
    // left as the service wrote it, so only these two lines change on screen.
    const failing = (body: HealthReport): HealthReport => ({
      ...body,
      telemetry: { ...body.telemetry, failing: true, last_error: ingestion },
      queues: { ...body.queues, error: inspection },
    })
    const unexplained = (body: HealthReport): HealthReport => ({
      ...body,
      telemetry: { ...body.telemetry, failing: true },
      queues: { ...body.queues, error: inspection },
    })
    const health = await dress(page, '**/api/v1/health')
    health.answer({ kind: 'report', edit: failing })

    const telemetryPanel = page.locator('section', {
      has: page.getByRole('heading', { name: 'Telemetry', exact: true }),
    })
    const queuesPanel = page.locator('section', {
      has: page.getByRole('heading', { name: 'Queues', exact: true }),
    })

    // The next poll brings both lines, each under the panel it belongs to and each naming the
    // reason the payload carried.
    await expect(telemetryPanel.getByText(`Ingestion is failing: ${ingestion}`)).toBeVisible()
    await expect(queuesPanel.getByText(`Queue inspection failed: ${inspection}`)).toBeVisible()

    // A failing ingestion with no reason in it has its own wording, rather than an empty line
    // where the reason would have been.
    health.answer({ kind: 'report', edit: unexplained })
    await expect(
      telemetryPanel.getByText('Ingestion is failing: ingestion is failing right now'),
    ).toBeVisible()

    // Queues draws the inspection banner from the same report, so the screen that lists the
    // queues says why it could not inspect them, and the ingestion line stays on Overview,
    // which is the only page with a telemetry panel.
    await page.getByRole('link', { name: 'All queues' }).click()
    await expect(page.getByRole('heading', { name: 'Queues', exact: true })).toBeVisible()
    await expect(page.getByText(`Queue inspection failed: ${inspection}`)).toBeVisible()
    // The prefix, not the wording it carried on Overview: a line drawn here with any reason at
    // all is a line this screen should not have.
    await expect(page.getByText('Ingestion is failing:')).toHaveCount(0)

    // A healthy answer again drops the banner instead of leaving it stuck on the screen.
    health.answer({ kind: 'through' })
    await expect(page.getByText(`Queue inspection failed: ${inspection}`)).toHaveCount(0)

    await page.getByRole('link', { name: 'Overview', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible()
    await expect(page.getByText('Ingestion is failing: ingestion is failing right now')).toHaveCount(0)
  })

  test('the jobs and metrics screens say when their history cannot be read', async ({ page }) => {
    // Both screens read their own endpoint, so one knob decides what each of them is told: 503
    // is the answer for a service started without its metrics database, any other failure is
    // the one that names the status. Neither needs the store to break, which is why both
    // lines are read here rather than only in the models that word them.
    const history = await dress(page, '**/api/v1/jobs*', '**/api/v1/metrics*')
    history.answer({ kind: 'status', status: 503 })

    const jobUnavailable = page.getByText('Job history is unavailable:')
    const jobFailure = page.getByText('Could not read job history: /api/v1/jobs returned 500')
    const historyUnavailable = page.getByText('History is unavailable:')
    const historyFailure = page.getByText('Could not read history: /api/v1/metrics returned 500')

    // A 503 is not an error: the screen says the service was started without the database the
    // history lives in and keeps working around it, without claiming a failed read.
    await page.goto(`${base}/#/jobs`)
    await expect(jobUnavailable).toBeVisible()
    await expect(jobFailure).toHaveCount(0)

    // Any other status is a failure, and the line carries the status the page was answered
    // with. This screen does not poll, so it has to be asked again: it is left and re-entered
    // rather than waited on.
    history.answer({ kind: 'status', status: 500 })
    await page.getByRole('link', { name: 'Overview', exact: true }).click()
    await page.getByRole('link', { name: 'Jobs', exact: true }).click()
    await expect(jobFailure).toBeVisible()
    await expect(jobUnavailable).toHaveCount(0)

    // The metrics screen is told the same two ways and words its own lines from the same
    // statuses: the failure first, then the 503 it reaches by asking a different question.
    await page.getByRole('link', { name: 'Metrics', exact: true }).click()
    await expect(historyFailure).toBeVisible()
    await expect(historyUnavailable).toHaveCount(0)

    history.answer({ kind: 'status', status: 503 })
    await page.getByRole('button', { name: 'hourly', exact: true }).click()
    await expect(historyUnavailable).toBeVisible()
    await expect(historyFailure).toHaveCount(0)

    // A read that answers again clears the line rather than leaving it behind, and the jobs
    // screen does the same when it is next asked.
    history.answer({ kind: 'through' })
    await page.getByRole('button', { name: 'daily', exact: true }).click()
    await expect(historyUnavailable).toHaveCount(0)
    await expect(historyFailure).toHaveCount(0)

    await page.getByRole('link', { name: 'Jobs', exact: true }).click()
    await expect(jobUnavailable).toHaveCount(0)
    await expect(jobFailure).toHaveCount(0)
  })

  test('the jobs and metrics screens say they are still reading', async ({ page }) => {
    // A local service answers in milliseconds, so the line each screen draws while its read is
    // in flight would be gone before a reader saw it. Both answers are held at the socket and
    // released once the line has been read: nothing sleeps, and the line is read on the test's
    // terms rather than a timer's.
    // One screen at a time: leaving a screen aborts the read it had in flight, so a held answer
    // is released while the page that asked for it is still there to receive it.
    let releaseJobs!: () => void
    const jobsHeld = new Promise<void>((resolve) => {
      releaseJobs = resolve
    })
    const jobsRead = await dress(page, '**/api/v1/jobs*')
    jobsRead.answer({ kind: 'through', until: jobsHeld })

    const readingJobs = page.getByText('Reading job history…')

    // Nothing has answered the jobs screen: it says what it is waiting for, and no table is
    // drawn empty in the meantime.
    await page.goto(`${base}/#/jobs`)
    await expect(readingJobs).toBeVisible()

    // A request is parked at the gate, so the answer this line waits for has provably not been
    // sent: the line is read against an outstanding read rather than against the milliseconds
    // a local service leaves. A case that lost its hold cannot park, and fails here.
    await expect.poll(() => jobsRead.parked()).toBe(1)
    await expect(readingJobs).toBeVisible()

    // The answer is let through, and the line gives way to the screen it was standing in for.
    const jobsAnswered = page.waitForResponse(
      (candidate) => candidate.url().includes('/api/v1/jobs') && candidate.ok(),
    )
    releaseJobs()
    await jobsAnswered
    await expect(readingJobs).toHaveCount(0)

    // The metrics screen holds the same kind of line for its own read.
    let releaseHistory!: () => void
    const historyHeld = new Promise<void>((resolve) => {
      releaseHistory = resolve
    })
    const historyRead = await dress(page, '**/api/v1/metrics*')
    historyRead.answer({ kind: 'through', until: historyHeld })

    const readingHistory = page.getByText('Reading history…')
    await page.getByRole('link', { name: 'Metrics', exact: true }).click()
    await expect(readingHistory).toBeVisible()

    // Parked here too, so the same claim holds on this screen.
    await expect.poll(() => historyRead.parked()).toBe(1)
    await expect(readingHistory).toBeVisible()

    const historyAnswered = page.waitForResponse(
      (candidate) => candidate.url().includes('/api/v1/metrics') && candidate.ok(),
    )
    releaseHistory()
    await historyAnswered
    await expect(readingHistory).toHaveCount(0)
    await expect(page.getByText(/^Outcomes per minute for /)).toBeVisible()
  })

  test('leaving a screen mid-read leaves no error on the page', async ({ page }) => {
    // A screen aborts its read when it unmounts, and an aborted read is this file's question
    // withdrawn, not the service refusing to answer. A held answer is the only way to reach
    // that state on purpose: the read is parked, the screen goes away beneath it, and the
    // answer is released into a request the page has already given up on.
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    const jobs = await dress(page, '**/api/v1/jobs*')
    jobs.answer({ kind: 'through', until: held })

    const readingJobs = page.getByText('Reading job history…')
    const jobError = page.getByText('Could not read job history:')

    await page.goto(`${base}/#/jobs`)
    await expect.poll(() => jobs.parked()).toBe(1)
    await expect(readingJobs).toBeVisible()

    // Away while the read is outstanding: the jobs screen unmounts and aborts its request.
    const withdrawn = page.waitForEvent('requestfailed', {
      predicate: (request) => request.url().includes('/api/v1/jobs'),
      timeout: 10_000,
    })
    await page.getByRole('link', { name: 'Metrics', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Metrics', exact: true })).toBeVisible()

    // The read really was withdrawn rather than merely left outstanding: the page aborted it,
    // which is the only reason the answer released below has nobody left to reach.
    expect((await withdrawn).failure()?.errorText).toContain('ABORTED')
    release()

    // Nothing about the aborted read is reported on the screen the reader moved to, and the
    // read that screen makes for itself is asked and answered as usual.
    await expect(page.getByText(/^Outcomes per minute for /)).toBeVisible()
    await expect(jobError).toHaveCount(0)

    // Back on the jobs screen the abandoned read left nothing behind: no failure line, no
    // unavailable line, and a read of its own drawn in their place.
    await page.getByRole('link', { name: 'Jobs', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Jobs', exact: true })).toBeVisible()
    await expect(page.locator('tbody tr').first()).toBeVisible()
    await expect(jobError).toHaveCount(0)
    await expect(page.getByText('Job history is unavailable:')).toHaveCount(0)
    await expect(readingJobs).toHaveCount(0)
  })

  test('a filter that replaces a read in flight leaves only the answer it asked for', async ({ page }) => {
    // A screen that stays mounted still replaces its read when the question changes: a filter
    // is a new query, so the screen aborts the read for the old one and asks again from where
    // it is. Leaving the screen hides that — the answer lands on a screen nobody is on — while
    // here the old read's answer lands on the screen that is still drawing the new one.
    //
    // Both reads are held at the socket, because the state under test only lasts while the
    // superseding read is outstanding: an aborted read is only able to write over the screen
    // until the read that replaced it answers.
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    const jobs = await dress(page, '**/api/v1/jobs*')
    jobs.answer({ kind: 'through', until: held })

    const readingJobs = page.getByText('Reading job history…')
    const jobError = page.getByText('Could not read job history:')

    await page.goto(`${base}/#/jobs`)
    await expect.poll(() => jobs.parked()).toBe(1)
    await expect(readingJobs).toBeVisible()

    // The filter is changed while the first read is parked, so the screen is still mounted when
    // its own read is withdrawn: the queue the first read asked for is not the one the second
    // does, which is what tells the two apart in the browser's own events.
    const withdrawn = page.waitForEvent('requestfailed', {
      predicate: (request) => request.url().includes('/api/v1/jobs?') && !request.url().includes('queue='),
      timeout: 10_000,
    })
    const superseding = page.waitForRequest(
      (request) => request.url().includes('/api/v1/jobs?') && request.url().includes(`queue=${queue}`),
    )
    await page.getByLabel('Queue').selectOption(queue)

    // The first read really was aborted rather than merely left outstanding, and the second was
    // asked for, so the two parked below are the replaced answer and its replacement.
    expect((await withdrawn).failure()?.errorText).toContain('ABORTED')
    await superseding

    // Both are at the gate now. The replaced answer can only reach this screen here, with the
    // read that replaced it still outstanding, and an aborted read treated as a failure would
    // take the waiting line's place with an error the service never reported.
    await expect.poll(() => jobs.parked()).toBe(2)
    await expect(readingJobs).toBeVisible()
    await expect(jobError).toHaveCount(0)

    release()

    // The answer the screen asked for last is the one it draws, and the read it gave up on
    // leaves nothing behind: no failure line, no unavailable line, no waiting line.
    await expect(page.getByRole('cell', { name: 'App\\Jobs\\RecordMarker', exact: true })).toBeVisible()
    await expect(readingJobs).toHaveCount(0)
    await expect(jobError).toHaveCount(0)
    await expect(page.getByText('Job history is unavailable:')).toHaveCount(0)
  })

  test('a filter that replaces a metrics read in flight leaves only the answer it asked for', async ({ page }) => {
    // The jobs screen proved the shape of this; the metrics screen holds the same pair of abort
    // guards, and until now only the unmount case reached them. A resolution is a new question,
    // so the chip re-runs the read from the screen the reader is still on. It is the chip rather
    // than the queue select because the queue the screen opens on is whichever the report lists
    // first, while the resolution it opens on is its own.
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    const metrics = await dress(page, '**/api/v1/metrics*')
    metrics.answer({ kind: 'through', until: held })

    const readingHistory = page.getByText('Reading history…')
    const historyError = page.getByText('Could not read history:')

    await page.goto(`${base}/#/metrics`)
    await expect.poll(() => metrics.parked()).toBe(1)
    await expect(readingHistory).toBeVisible()

    // The first read asks for the resolution the screen opens on; the chip asks for another, and
    // those two windows are what tell the replaced read from its replacement in the browser's
    // own events rather than by the order they happen to arrive in.
    const withdrawn = page.waitForEvent('requestfailed', {
      predicate: (request) =>
        request.url().includes('/api/v1/metrics?') && request.url().includes('resolution=minute'),
      timeout: 10_000,
    })
    const superseding = page.waitForRequest(
      (request) =>
        request.url().includes('/api/v1/metrics?') && request.url().includes('resolution=daily'),
    )
    await page.getByRole('button', { name: 'daily', exact: true }).click()

    // The first read really was aborted rather than merely left outstanding, and the second was
    // asked for, so the two parked below are the replaced answer and its replacement.
    expect((await withdrawn).failure()?.errorText).toContain('ABORTED')
    await superseding

    // Both are at the gate now. The replaced answer can only reach this screen here, with the
    // read that replaced it still outstanding, and an aborted read treated as a failure would
    // take the waiting line's place with an error the service never reported.
    await expect.poll(() => metrics.parked()).toBe(2)
    await expect(readingHistory).toBeVisible()
    await expect(historyError).toHaveCount(0)

    release()

    // The answer the screen asked for last is the one it draws: a daily window this young store
    // has no rows for, said plainly rather than as a read that failed.
    await expect(page.getByText('No data in this window.')).toBeVisible()
    await expect(readingHistory).toHaveCount(0)
    await expect(historyError).toHaveCount(0)
    await expect(page.getByText('History is unavailable:')).toHaveCount(0)
  })

  test('the metrics screen says when no queue has been sampled', async ({ page }) => {
    // The service samples the queues it is configured with, so a store with none is dressed:
    // every health answer comes back with an empty depth list, which is what the screen reads
    // to decide it has nothing to chart. The lines it draws then are its own.
    await readHealth(page, `${base}/#/metrics`)
    const health = await dress(page, '**/api/v1/health')
    health.answer({
      kind: 'report',
      edit: (body) => ({ ...body, queues: { ...body.queues, depths: [] } }),
    })

    const nothingYet = page.getByText('No queue has been sampled yet, so there is no history to chart.')
    const queueChoice = page.getByLabel('Queue')

    // The next poll carries the dressed answer: the screen says there is nothing to chart and
    // offers no queue to choose, rather than charting an empty window for a queue nobody named.
    await expect(nothingYet).toBeVisible()
    await expect(queueChoice).toBeDisabled()
    await expect(queueChoice.locator('option:checked')).toHaveText('none sampled')

    // A real answer again puts the queues back, and the line goes with the state it described.
    health.answer({ kind: 'through' })
    await expect(nothingYet).toHaveCount(0)
    await expect(queueChoice).toBeEnabled()
  })

  test('the supervisor ratios read singular for a lone worker and plural for a pool', async ({ page }) => {
    // Two supervisors on purpose: one worker on the queue the jobs run through, two on the
    // idle one. The Overview then aggregates a pool of three, while the workers screen keeps
    // the ratios apart and words each one for its own size.
    const report = await readHealth(page, `${base}/`)
    const supervisors = report.supervisors ?? []
    expect(supervisors).toHaveLength(3)
    expect(supervisors.map((supervisor) => supervisor.desired).sort()).toEqual([1, 1, 2])
    const running = supervisors.reduce((sum, supervisor) => sum + supervisor.running, 0)
    const desired = supervisors.reduce((sum, supervisor) => sum + supervisor.desired, 0)

    // The tally and the ratio it leads into are one sentence, worded by the same helper. It
    // grows a suffix when a worker has crashed or is crash looping, so the ratio is read out
    // of the sentence rather than required to be the whole of it. The panel and the paragraph
    // it holds are found by their own hooks rather than by their styling classes, so restyling
    // either one does not take the smoke with it.
    const sentence = page.locator('.overview__workers').locator('.panel__summary')
    await expect(sentence).toContainText(
      `3 supervisors, ${running} of ${countNoun(desired, 'worker')} running`,
    )

    // Each supervisor's own header on the workers screen repeats its ratio, with its own
    // crashed and escalated suffixes behind it. Both spellings have to be on the page.
    await page.goto(`${base}/#/workers`)
    const ratios: string[] = []
    for (const supervisor of supervisors) {
      const ratio = `${supervisor.running} of ${countNoun(supervisor.desired, 'worker')} running`
      ratios.push(ratio)
      await expect(page.getByRole('heading').filter({ hasText: supervisor.id })).toContainText(ratio)
    }
    expect(ratios).toContain('1 of 1 worker running')
    expect(ratios).toContain('2 of 2 workers running')
  })

  test('the workers screen lists every worker the supervisors started', async ({ page }) => {
    const report = await readHealth(page, `${base}/#/workers`)
    const supervisors = report.supervisors ?? []
    expect(supervisors).toHaveLength(3)
    const workers = supervisors.flatMap((supervisor) => supervisor.workers ?? [])
    // Two on the idle queue, one on the queue the jobs run through, one on the crashy one.
    expect(workers).toHaveLength(4)

    // One row per process, across both supervisors' tables.
    await expect(page.locator('tbody tr')).toHaveCount(workers.length)

    // Every cell is the report's own field, so a row shows the process the supervisor is
    // running rather than a placeholder that merely looks like one.
    for (const worker of workers) {
      const row = page.getByRole('row', { name: new RegExp(worker.uuid) })
      await expect(row.getByRole('cell')).toHaveText([
        worker.uuid,
        worker.queue,
        String(worker.pid),
        stateCell(worker),
        String(worker.restarts),
        String(worker.last_exit_code) + (worker.last_exit_reason ? ` ${worker.last_exit_reason}` : ''),
        // The last cell is the control rather than a field, so it carries the button's label.
        'Restart',
      ])
    }
  })

  test('a supervisor with no workers says so instead of an empty table', async ({ page }) => {
    // Every worker is up, so a supervisor with none is a state the service does not reach on its
    // own: the report is dressed with one supervisor's worker list emptied, which is what a
    // supervisor that has not started its pool yet hands the page. Which supervisor that is is
    // read from the report, by the queue its worker runs, rather than assumed from the config.
    const report = (await getJSON(`${base}/api/v1/health`)) as HealthReport
    const supervisors = report.supervisors ?? []
    const idle = supervisors.find((supervisor) =>
      (supervisor.workers ?? []).some((worker) => worker.queue === extraQueue),
    )
    if (idle === undefined) {
      throw new Error(`the report does not list a worker on the ${extraQueue} queue`)
    }
    const idleWorkers = (idle.workers ?? []).length

    const health = await dress(page, '**/api/v1/health')
    health.answer({
      kind: 'report',
      edit: (body) => ({
        ...body,
        supervisors: (body.supervisors ?? []).map((supervisor) =>
          supervisor.id === idle.id ? { ...supervisor, workers: [] } : supervisor,
        ),
      }),
    })

    await page.goto(`${base}/#/workers`)
    await expect(page.getByRole('heading', { name: 'Workers', exact: true })).toBeVisible()

    // The line belongs to the emptied supervisor and stands in place of its table: the section
    // is found by its own hook, and only the heading picks it out of the three.
    const section = page.locator('.workers__supervisor', {
      has: page.getByRole('heading').filter({ hasText: idle.id }),
    })
    await expect(section.getByText('This supervisor has no workers yet.')).toBeVisible()
    await expect(section.locator('tbody tr')).toHaveCount(0)

    // The other supervisors still list theirs, so the empty state is one report's rather than
    // the screen's: the rows drawn are every worker minus the emptied supervisor's own.
    const rows = supervisors.reduce((sum, supervisor) => sum + (supervisor.workers ?? []).length, 0)
    await expect(page.locator('tbody tr')).toHaveCount(rows - idleWorkers)
  })

  test('the workers screen says when no supervisor has reported', async ({ page }) => {
    // The service always reports the supervisors it starts, so their absence only comes from the
    // answer: the report is dressed with none, which is what a service with no supervisors would
    // hand the page. Nothing is drawn in their place but the line that says so.
    const health = await dress(page, '**/api/v1/health')
    health.answer({ kind: 'report', edit: (body) => ({ ...body, supervisors: [] }) })

    await page.goto(`${base}/#/workers`)
    await expect(page.getByRole('heading', { name: 'Workers', exact: true })).toBeVisible()
    await expect(page.getByText('No supervisor has reported yet.')).toBeVisible()
    await expect(page.locator('.workers__supervisor')).toHaveCount(0)
    await expect(page.locator('tbody tr')).toHaveCount(0)
  })

  test('the queues screen shows the depth it inspected for each queue', async ({ page }) => {
    const report = await readHealth(page, `${base}/#/queues`)
    const depths = report.queues.depths ?? []
    // Both configured queues are sampled, not only the one that carried the jobs.
    expect(depths.map((depth) => depth.queue).sort()).toEqual([crashyQueue, extraQueue, queue].sort())

    await expect(page.getByRole('heading', { name: 'Queues' })).toBeVisible()
    await expect(page.locator('tbody tr')).toHaveCount(depths.length)

    // The counts are the driver's own numbers. The age is formatted by the page from the
    // browser's clock, so only the shape of it is asserted here.
    const oldest = /^(?:unknown|just now|\d+[sm]|\d+h \d+m|\d+d(?: \d+h)?)(?: — .*)?$/
    for (const depth of depths) {
      const row = page
        .getByRole('row')
        .filter({ has: page.getByRole('cell', { name: depth.queue, exact: true }) })
      await expect(row.getByRole('cell')).toHaveText([
        depth.queue,
        numberCell(depth.pending),
        numberCell(depth.reserved),
        numberCell(depth.delayed),
        oldest,
      ])
    }
  })

  test('a job row opens the detail its payload describes', async ({ page }) => {
    const payload = await readJobs(page, `${base}/#/jobs`)
    const job = payload.jobs.find((candidate) => candidate.class === 'App\\Jobs\\RecordMarker')
    if (job === undefined) {
      throw new Error('the jobs page was not given the attempt the worker ran')
    }

    await expect(page.getByRole('heading', { name: 'Jobs' })).toBeVisible()

    // The finish time is the cell's own text, formatted by the browser with the page's own
    // options, so the row is found by what the payload says rather than by its position: an
    // attempt that lands while the smoke runs shifts the rows but not this one's finish.
    const finished = await page.evaluate(
      (finishedAt) =>
        new Date(finishedAt).toLocaleString(undefined, {
          year: 'numeric',
          month: 'short',
          day: 'numeric',
          hour: '2-digit',
          minute: '2-digit',
          second: '2-digit',
        }),
      job.finished_at,
    )
    const row = page
      .getByRole('row')
      .filter({ has: page.getByRole('cell', { name: job.class, exact: true }) })
      .filter({ has: page.getByRole('cell', { name: finished, exact: true }) })
    await expect(row).toHaveCount(1)

    // The pane is opened from the row itself, so the detail is the one that row describes.
    await row.getByRole('button', { name: 'details' }).click()

    // Every field is the attempt's own value, in the order the pane lists them.
    const detail = page.locator('.jobs__detail')
    await expect(detail).toBeVisible()
    await expect(detail.getByRole('heading', { name: job.class, exact: true })).toBeVisible()
    await expect(detail.locator('dd')).toHaveText([
      job.outcome,
      job.queue,
      String(job.attempts),
      durationText(job.duration_ms),
      finished,
      job.uuid === '' ? 'none published' : job.uuid,
      job.job_id,
      job.id,
    ])

    // A processed attempt threw nothing, and the pane says so rather than naming an
    // exception it does not have.
    await expect(detail).toContainText('No exception was recorded for this attempt.')

    // Close belongs to the pane, not the page: the detail goes away and the table that
    // described it stays, still listing the row the payload named.
    await detail.getByRole('button', { name: 'close' }).click()
    await expect(detail).toHaveCount(0)
    await expect(row).toBeVisible()
  })

  test('a filter that matches no attempt says so instead of drawing an empty table', async ({ page }) => {
    // The page opens on every attempt; the extra queue's worker has run none, so filtering to it
    // is a question with an empty answer rather than a screen that has not been read yet. The
    // answer is read for itself before the line is, so an empty page and an empty screen are not
    // mistaken for one another.
    await page.goto(`${base}/#/jobs`)
    await expect(page.getByRole('heading', { name: 'Jobs', exact: true })).toBeVisible()
    await expect(page.locator('tbody tr').first()).toBeVisible()

    const [response] = await Promise.all([
      page.waitForResponse(
        (candidate) =>
          candidate.url().includes('/api/v1/jobs?') &&
          candidate.url().includes(`queue=${extraQueue}`) &&
          candidate.ok(),
      ),
      page.getByLabel('Queue').selectOption(extraQueue),
    ])
    const payload = (await response.json()) as JobsPayload
    expect(payload.jobs).toHaveLength(0)

    // The line is the table's answer to that empty page, not a failure and not a table with no
    // rows in it: no header row, no body row.
    await expect(page.getByText('No attempts match this filter.')).toBeVisible()
    await expect(page.locator('tbody tr')).toHaveCount(0)
    await expect(page.locator('thead')).toHaveCount(0)
    await expect(page.getByText('Could not read job history:')).toHaveCount(0)
    await expect(page.getByText('Job history is unavailable:')).toHaveCount(0)
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
    const history = await readHistory(page, `${base}/#/metrics`, queue)

    await expect(page.getByRole('heading', { name: 'Metrics' })).toBeVisible()
    await expect(page.getByLabel('Queue')).toHaveValue(queue)
    await expect(page.getByRole('button', { name: 'minute' })).toHaveAttribute('aria-pressed', 'true')

    // One processed attempt and one failed one, and nothing else in the window: the store is
    // fresh for every run, so any third attempt would mean a job ran twice.
    const total = history.buckets.reduce((sum, bucket) => sum + attemptCount(bucket), 0)
    expect(total).toBe(2)

    // The chart names that total twice: in the figure's accessible name, and in its caption,
    // which sits in the figcaption beside the plot rather than in the svg, so it is read from
    // the page instead of from the image.
    const outcomes = page.getByRole('img', { name: new RegExp(`, ${countNoun(total, 'job')}$`) })
    await expect(outcomes).toBeVisible()
    await expect(page.locator('.outcome-chart__total')).toHaveText(countNoun(total, 'job'))

    // The label at the top of the plot is the scale the columns are drawn against, so it is the
    // tallest column rather than the window's two attempts: a run that straddles a minute
    // boundary draws each attempt in its own minute, and neither column is worth 2.
    const tallest = Math.max(1, ...history.buckets.map(attemptCount))
    await expect(outcomes.locator('.outcome-chart__scale')).toHaveText(countNoun(tallest, 'job'))

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
    // Two queues are configured now, so the window under test is chosen rather than assumed.
    await page.getByLabel('Queue').selectOption(queue)
    await expect(page.getByLabel('Queue')).toHaveValue(queue)

    // Nothing is folded into the daily table until an hour has closed, so a store this young
    // has no daily rows: the page has to say that rather than draw a flat line at zero.
    await page.getByRole('button', { name: 'daily' }).click()

    await expect(page.getByRole('button', { name: 'daily' })).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', { name: 'minute' })).toHaveAttribute('aria-pressed', 'false')
    await expect(page.getByText('No data in this window.')).toBeVisible()
  })

  test('the metrics screen draws the average duration the store returned', async ({ page }) => {
    const history = await readHistory(page, `${base}/#/metrics`, queue)

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

    const average = averageMs(bucket)
    const label = await page.evaluate(
      (start) =>
        new Date(start * 1000).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }),
      Date.parse(bucket.start) / 1000,
    )

    const durationChart = page.getByRole('img', { name: /Average duration per attempt/ })
    await expect(durationChart).toBeVisible()

    // The label at the top of the plot is what a full-height column is worth: the slowest
    // average in the window, which is the bucket's own average and not the scale's floor.
    const peak = Math.max(0, ...history.buckets.map(averageMs))
    await expect(durationChart.locator('.duration-chart__scale')).toHaveText(`${peak} ms`)
    // Counted rather than required visible: a job that finished in under a millisecond is a
    // zero-height column, and this assertion is about the number it labels.
    await expect(
      durationChart.locator('rect', { hasText: `${label} — average ${average} ms` }),
    ).toHaveCount(1)
  })

  test('an operator retries a failed job and the retry is recorded', async ({ page }) => {
    // The failing attempt the setup dispatched is the one retried, so the job has to be one the
    // application still stores — which is why the fixture keeps failed jobs in its own file
    // rather than discarding them.
    const failed = await waitForJob(
      'still list the failed attempt',
      `queue=${queue}&outcome=failed`,
      (job) => job.class === 'App\\Jobs\\AlwaysFails',
    )
    if (failed.uuid === undefined || failed.uuid === '') {
      throw new Error('the failed attempt carries no uuid to retry')
    }

    const token = await csrfToken(page)
    const before = auditLines().length

    // The request carries the browser's session cookie and the token from its jar: the two
    // things the endpoint asks for, presented by the browser rather than borrowed here.
    const response = await page.request.post(`${base}/api/v1/failed/${failed.uuid}/retry`, {
      headers: { 'X-CSRF-Token': token },
    })
    expect(response.status()).toBe(202)
    expect(await response.json()).toEqual({ job: failed.uuid, queue })

    // The application forgot the failed record and pushed the payload back, so the worker ran
    // the job again. It fails every time, which is what makes the proof readable: the same
    // identity gains an attempt newer than the one that was retried.
    await waitFor('run the retried job', async () => {
      const jobs = await recordedJobs(`queue=${queue}&outcome=failed`)
      return jobs.some(
        (job) => job.uuid === failed.uuid && job.finished_at !== failed.finished_at,
      )
    })

    // The record is the one durable trace the action leaves, read from the service log where an
    // operator would find it.
    await waitFor('write the retry audit record', async () => auditLines().length >= before + 1)
    const record = auditLines()
      .slice(before)
      .find((line) => line.action === 'failed-job-retry')
    expect(record).toMatchObject({
      action: 'failed-job-retry',
      outcome: 'allowed',
      user: smokeUser,
      role: 'operator',
      detail: `queue ${queue}`,
    })
    expect(record?.address).toBe('127.0.0.1')
  })

  test('an operator retries and deletes a failed job from its detail panel', async ({ page }) => {
    // The panel is the row's own view and it prints the uuid the two actions take, so the case
    // opens it from the newest failed attempt and checks it describes the attempt the API lists.
    const newest = (await recordedJobs(`queue=${queue}&outcome=failed&limit=1`))[0]
    if (newest?.uuid === undefined || newest.uuid === '') {
      throw new Error('the failed history lists no attempt with a uuid')
    }

    await page.goto(`${base}/#/failed`)
    const detail = page.locator('.jobs__detail')
    await page.getByRole('button', { name: 'details' }).first().click()
    await expect(detail).toBeVisible()
    // The panel prints the uuid twice — as the UUID and as the job id the recorder stores — so
    // the first of the two is what is read here.
    await expect(detail.getByText(newest.uuid, { exact: true }).first()).toBeVisible()

    // Retrying from the panel is an operator's press rather than this file's request: the page
    // echoes the token its own jar holds, which is the half a direct call here would supply.
    const attempts = (await recordedJobs(`queue=${queue}&outcome=failed&limit=10`)).filter(
      (job) => job.uuid === newest.uuid,
    ).length
    const before = auditLines().length

    await detail.getByRole('button', { name: 'Retry', exact: true }).click()
    await expect(detail.getByText(`queued again on ${queue}`)).toBeVisible()

    // The application forgot the failed record and pushed the payload back, so the worker runs
    // the job again; it fails every time, so the same uuid gains an attempt.
    await waitFor(
      'run the job the panel retried',
      async () =>
        (await recordedJobs(`queue=${queue}&outcome=failed&limit=10`)).filter(
          (job) => job.uuid === newest.uuid,
        ).length > attempts,
    )

    await waitFor('write the retry audit record', async () =>
      auditLines()
        .slice(before)
        .some((line) => line.action === 'failed-job-retry' && line.outcome === 'allowed'),
    )
    const retried = auditLines()
      .slice(before)
      .find((line) => line.action === 'failed-job-retry')
    expect(retried).toMatchObject({
      action: 'failed-job-retry',
      outcome: 'allowed',
      user: smokeUser,
      role: 'operator',
      detail: `queue ${queue}`,
    })

    // The attempt the retried run left behind is the newest row, so the panel describes it:
    // deleting needs a record the application still stores, and this is the one it just wrote.
    // The reload re-reads the table, because the attempt landed after the first read.
    await page.reload()
    await page.getByRole('button', { name: 'details' }).first().click()
    await expect(detail.getByText(newest.uuid, { exact: true }).first()).toBeVisible()

    const beforeDelete = auditLines().length
    await detail.getByRole('button', { name: 'Delete', exact: true }).click()
    await expect(detail.getByText(`deleted from ${queue}`)).toBeVisible()
    // The record is gone, so the controls go quiet rather than asking again.
    await expect(detail.getByRole('button', { name: 'Delete', exact: true })).toBeDisabled()

    await waitFor('write the delete audit record', async () =>
      auditLines()
        .slice(beforeDelete)
        .some((line) => line.action === 'failed-job-delete' && line.outcome === 'allowed'),
    )
    const deleted = auditLines()
      .slice(beforeDelete)
      .find((line) => line.action === 'failed-job-delete')
    expect(deleted).toMatchObject({
      action: 'failed-job-delete',
      outcome: 'allowed',
      user: smokeUser,
      role: 'operator',
      detail: `queue ${queue}`,
    })
  })

  test('a crash-looping worker is named on both supervisor lines', async ({ page }) => {
    // Nothing in the fixture kills its worker, so this case is the crash: a SIGKILL is a
    // crash to the supervisor, which replaces the worker with a growing backoff. Three of
    // those in a row cross its crash-loop threshold. The counts are polled rather than slept
    // through, and the last crash leaves a four-second window for the page to fetch in.
    test.setTimeout(180_000)
    for (let crashes = 0; crashes < 3; ) {
      const victim = await waitForCrashyWorker(
        'report a worker that can be killed',
        (worker) => worker.pid > 0 && worker.state === 'running',
      )
      if (!killWorker(victim.pid)) {
        // A reap won the race: the supervisor is already replacing it, so this kill is not
        // one of the three, and the replacement is what the next pass targets.
        await waitForCrashyWorker(
          'replace a worker that was already gone',
          (worker) => worker.pid !== victim.pid,
        )
        continue
      }
      await waitForCrashyWorker(
        'record the crash',
        (worker) => worker.state === 'crashed' || worker.pid !== victim.pid,
      )
      crashes++
    }

    const looping = await waitForCrashyWorker('be flagged crash looping', (worker) => worker.crash_looping)
    // A crashed slot is what the sentence counts in `crashed`; the streak is what it counts
    // in `crash looping`. Both are on the page together for the length of the backoff.
    expect(looping.state).toBe('crashed')

    // One fetch feeds both screens: the hash navigation re-renders the report this smoke
    // captured, so the sentence, the header and the row are all read out of that payload
    // rather than out of a state the test hoped was still there.
    const looped = await readHealth(page, `${base}/`)
    const loopedSupervisors = looped.supervisors ?? []
    const loopedCrashy = loopedSupervisors.find((supervisor) => supervisor.id === crashy)
    const loopedWorker = loopedCrashy?.workers?.[0]
    if (loopedCrashy === undefined || loopedWorker === undefined) {
      throw new Error(`the report does not list the ${crashy} supervisor's worker`)
    }
    expect(loopedCrashy.crashed).toBe(1)
    expect(loopedWorker.crash_looping).toBe(true)

    const running = loopedSupervisors.reduce((sum, supervisor) => sum + supervisor.running, 0)
    const desired = loopedSupervisors.reduce((sum, supervisor) => sum + supervisor.desired, 0)
    const sentence = page.locator('.overview__workers').locator('.panel__summary')
    await expect(sentence).toHaveText(
      `${countNoun(loopedSupervisors.length, 'supervisor')}, ${running} of ${countNoun(desired, 'worker')} running, 1 crashed, 1 crash looping.`,
    )

    // The crashing supervisor's own header repeats the suffix, and its worker's row carries
    // the badge of its own: the header is the tally, the state cell is that worker's flag.
    await page.goto(`${base}/#/workers`)
    await expect(page.getByRole('heading').filter({ hasText: crashy })).toHaveText(
      `${crashy} ${loopedCrashy.running} of ${countNoun(loopedCrashy.desired, 'worker')} running, 1 crashed`,
    )
    // The section is found by its own hook; the heading only picks the crashing supervisor out
    // of the three, so a restyled panel keeps the badge assertion where it belongs.
    const crashySection = page.locator('.workers__supervisor', {
      has: page.getByRole('heading').filter({ hasText: crashy }),
    })
    const badge = crashySection.getByText('crash looping', { exact: true })
    await expect(badge).toHaveCount(1)
    // Exactly one row wears it, and it is the row whose state and badge the report gave.
    await expect(crashySection.locator('tbody tr td').nth(3)).toHaveText(stateCell(loopedWorker))

    // The flag belongs to the streak, and the streak ends only once a replacement outlives
    // the supervisor's stable window. Nothing sleeps for it: this waits until the report
    // itself says the flag is gone.
    await waitFor(
      'clear the crash loop after a stable run',
      async () => {
        const worker = await crashyWorker()
        return worker !== undefined && worker.state === 'running' && !worker.crash_looping
      },
      90_000,
    )

    const cleared = await readHealth(page, `${base}/`)
    const steady = cleared.supervisors ?? []
    const steadyCrashy = steady.find((supervisor) => supervisor.id === crashy)
    if (steadyCrashy === undefined) {
      throw new Error(`the report does not list the ${crashy} supervisor`)
    }
    expect(steadyCrashy.crashed).toBe(0)
    const steadyWorker = steadyCrashy.workers?.[0]
    if (steadyWorker === undefined) {
      throw new Error(`the report does not list the ${crashy} supervisor's worker`)
    }
    expect(steadyWorker.crash_looping).toBe(false)

    // A healthy supervisor carries neither suffix, and the sentence is exactly the ratio again.
    const steadyRunning = steady.reduce((sum, supervisor) => sum + supervisor.running, 0)
    const steadyDesired = steady.reduce((sum, supervisor) => sum + supervisor.desired, 0)
    await expect(sentence).toHaveText(
      `3 supervisors, ${steadyRunning} of ${countNoun(steadyDesired, 'worker')} running.`,
    )
    await page.goto(`${base}/#/workers`)
    await expect(page.getByRole('heading').filter({ hasText: crashy })).toHaveText(
      `${crashy} ${steadyCrashy.running} of ${countNoun(steadyCrashy.desired, 'worker')} running`,
    )

    // The badge was the streak's, so it leaves with it: on the row it was asserted on there is
    // no badge left, and the state cell is the bare state the report now gives, with nothing
    // appended to it.
    const steadyRow = page.getByRole('row', { name: new RegExp(steadyWorker.uuid) })
    await expect(steadyRow.getByText('crash looping', { exact: true })).toHaveCount(0)
    await expect(steadyRow.getByRole('cell').nth(3)).toHaveText(steadyWorker.state)
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

function configYAML(port: number, dir: string, php: string, passwordHash: string): string {
  return `version: 1

server:
  listen: "127.0.0.1:${port}"
  auth:
    session_key_env: GOHORIZON_SMOKE_SESSION_KEY
    session_hours: 1
    users:
      - name: ${smokeUser}
        password_hash: ${JSON.stringify(passwordHash)}
        role: operator
      - name: ${viewerUser}
        password_hash: ${JSON.stringify(passwordHash)}
        role: viewer

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
      - id: extra
        queues:
          - ${extraQueue}
        balance:
          strategy: fixed
        workers:
          min: 2
          max: 2
        worker:
          sleep: 1
          tries: 1
          timeout: 6
          memory: 128
        shutdown_grace: 30
        restart_backoff: 1
      - id: ${crashy}
        queues:
          - ${crashyQueue}
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
        # The backoff is both the window a crash leaves the page to fetch in and, doubled plus
        # thirty seconds, the stable run that clears the loop: the lowest keeps both shortest.
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

// signIn asks the service for a session and returns the token its cookie carries, which is
// what both the browser and the direct calls below present.
async function signIn(): Promise<string> {
  const response = await fetch(`${base}/api/v1/session`, {
    method: 'POST',
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: smokeUser, password: smokePassword }),
    signal: AbortSignal.timeout(3_000),
  })
  if (!response.ok) {
    throw new Error(`sign-in returned ${response.status}`)
  }
  const cookie = response.headers
    .getSetCookie()
    .find((header) => header.startsWith(`${sessionCookie}=`))
  if (cookie === undefined) {
    throw new Error('sign-in answered without the session cookie')
  }
  return cookie.slice(sessionCookie.length + 1).split(';')[0]
}

// csrfToken reads the double-submit token the dashboard echoes on a mutation. The session route
// issues one when the jar has none, and the browser's jar is where the page reads it, so this
// asks through the browser rather than minting a token of its own.
async function csrfToken(page: Page): Promise<string> {
  const response = await page.request.get(`${base}/api/v1/session`)
  if (!response.ok()) {
    throw new Error(`session read returned ${response.status()}`)
  }
  const cookie = (await page.context().cookies(base)).find(
    (candidate) => candidate.name === csrfCookie,
  )
  if (cookie === undefined) {
    throw new Error('the session route answered without a CSRF cookie')
  }
  return cookie.value
}

// getJSON reads the API the way the pages do, cookie and all: every endpoint below the session
// routes answers 401 without one.
async function getJSON(url: string): Promise<unknown> {
  const response = await fetch(url, {
    signal: AbortSignal.timeout(3_000),
    headers: { Cookie: `${sessionCookie}=${session}` },
  })
  if (!response.ok) {
    throw new Error(`${url} returned ${response.status}`)
  }
  return response.json()
}

// waitFor polls until the condition holds, so a slow worker or a slow first inspect is a
// longer wait rather than a flaky test. Its failure carries the service log, which is where
// a store it cannot read or a worker that never started says so.
async function waitFor(
  what: string,
  condition: () => Promise<boolean>,
  timeoutMs = pollMs,
): Promise<void> {
  const deadline = Date.now() + timeoutMs
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
  throw new Error(`the service did not ${what} within ${timeoutMs} ms (${last})\n${readLog()}`)
}

// HistoryBucket is the part of the metrics payload the charts are drawn from, in the shape the
// API sends it; the page converts it, and so does this file.
interface HistoryBucket {
  start: string
  processed: number
  released: number
  failed: number
  timed_out: number
  duration_ms: number
}

// History is the window a chart covers: the resolution the store answered at, and its buckets.
interface History {
  queue: string
  resolution: 'minute' | 'hourly' | 'daily'
  buckets: HistoryBucket[]
}

// HealthReport is the part of the report the supervisor and queue screens are read against,
// in the shape the API sends it; the pages convert it, and so does this file.
interface HealthReport {
  status: string
  telemetry: {
    events: number
    duplicates: number
    invalid: number
    errors: number
    failing: boolean
    last_error?: string
  }
  queues: { connection?: string; error?: string; depths: QueueDepth[] | null }
  supervisors?: SupervisorReport[] | null
}

interface QueueDepth {
  queue: string
  pending: number | null
  reserved: number | null
  delayed: number | null
  oldest_pending_at: number | null
  oldest_pending_error?: string
}

interface SupervisorReport {
  id: string
  running: number
  desired: number
  crashed: number
  escalations: number
  workers: WorkerReport[] | null
}

interface WorkerReport {
  uuid: string
  queue: string
  pid: number
  state: string
  restarts: number
  last_exit_code: number
  last_exit_reason?: string
  adopted: boolean
  crash_looping: boolean
}

// JobsPayload is a page of job history as the API sends it: the page turns it into rows and
// a detail pane, and so does this file.
interface JobsPayload {
  jobs: JobAttempt[]
  next_before: string
}

interface JobAttempt {
  id: string
  job_id: string
  uuid: string
  class: string
  queue: string
  outcome: string
  attempts: number
  duration_ms: number
  finished_at: string
  exception: { class: string; message: string } | null
}

// readHealth opens a screen and returns the report it rendered, because the response the page
// itself made is the only honest source for the numbers it should show.
async function readHealth(page: Page, url: string): Promise<HealthReport> {
  const [response] = await Promise.all([
    page.waitForResponse((candidate) => candidate.url().includes('/api/v1/health') && candidate.ok()),
    page.goto(url),
  ])
  return (await response.json()) as HealthReport
}

// idlePoolWorker picks the pool the smoke's jobs do not run through, and one of its workers: a
// restart there interrupts nothing, because the pool keeps its other worker while this one is
// stopped and replaced.
function idlePoolWorker(report: HealthReport): { pool: SupervisorReport; target: WorkerReport } {
  const pool = (report.supervisors ?? []).find((supervisor) =>
    (supervisor.workers ?? []).some((worker) => worker.queue === extraQueue),
  )
  const target = (pool?.workers ?? []).find((worker) => worker.queue === extraQueue)
  if (pool === undefined || target === undefined) {
    throw new Error(`the report lists no worker on the ${extraQueue} queue`)
  }
  return { pool, target }
}

// recycled reports whether the pool is whole again with the restarted slot running on a new
// pid. A restart is a recycle: the worker keeps its uuid, which names the slot, and the pid is
// what changes.
async function recycled(pool: SupervisorReport, target: WorkerReport): Promise<boolean> {
  const next = (await getJSON(`${base}/api/v1/health`)) as HealthReport
  const supervisor = (next.supervisors ?? []).find((candidate) => candidate.id === pool.id)
  const workers = (supervisor?.workers ?? []).filter((worker) => worker.queue === target.queue)
  const slot = workers.find((worker) => worker.uuid === target.uuid)
  return (
    workers.length === pool.desired &&
    slot !== undefined &&
    slot.pid !== target.pid &&
    workers.every((worker) => worker.state === 'running')
  )
}

// restartRecord is the restart audit record written since `since`, which is the one durable
// trace the action leaves.
function restartRecord(since: number): AuditRecord | undefined {
  return auditLines()
    .slice(since)
    .find((line) => line.action === 'worker-restart')
}

// readJobs opens the jobs screen and returns the page it was given, because the response the
// page itself made is the only honest source for the attempt its detail should describe.
async function readJobs(page: Page, url: string): Promise<JobsPayload> {
  const [response] = await Promise.all([
    page.waitForResponse((candidate) => candidate.url().includes('/api/v1/jobs') && candidate.ok()),
    page.goto(url),
  ])
  return (await response.json()) as JobsPayload
}

// readHistory opens the metrics screen, selects the queue the smoke ran on, and returns the
// payload the page charted, because the response the page itself made is the only honest
// source for the numbers it should draw. With two queues configured the page's default is not
// the smoke's queue by definition, so the selection is made here rather than assumed.
async function readHistory(page: Page, url: string, queueName: string): Promise<History> {
  const metricsResponse = (candidate: { url(): string; ok(): boolean }) =>
    candidate.url().includes('/api/v1/metrics') && candidate.ok()

  const [initial] = await Promise.all([
    page.waitForResponse(metricsResponse),
    page.goto(url),
  ])
  let history = (await initial.json()) as History
  if (history.queue !== queueName) {
    const [selected] = await Promise.all([
      page.waitForResponse(metricsResponse),
      page.getByLabel('Queue').selectOption(queueName),
    ])
    history = (await selected.json()) as History
  }
  return history
}

// An answer the page can be given in place of the service's own. Every failure below is
// produced this way, because the service the suite starts is healthy for all of them: the
// request the page itself makes is intercepted and answered from a knob the case turns.
//   through — the service's own answer, untouched: the state a case was dressing is over.
//   status  — a status with no report in it, worded by whatever the page says for that status.
//   report  — the service's own report with these edits, so only what the case names differs on
//             screen. A degraded lock answers 503 with its report still in the body, so that
//             status is the answer's, not the body's.
// until holds any of them at the socket, because some states only last while a read is in
// flight. A held request must be released while the page that asked for it is still mounted:
// leaving a screen aborts its read. While a request is parked the helper says so, which is how
// a case knows its waiting line is drawn against an outstanding read rather than a slow one.
type Gate = { until?: Promise<void> }
type Answer =
  | (Gate & { kind: 'through' })
  | (Gate & { kind: 'status'; status: number })
  | (Gate & { kind: 'report'; edit: (report: HealthReport) => HealthReport; status?: number })

// dress answers every endpoint it is given from one knob, so a case reads as the answers it
// turns through rather than as route plumbing, and a state that lasts until the case says it
// is over does not depend on a poll landing while an assertion is reading.
async function dress(page: Page, ...endpoints: string[]) {
  let answer: Answer = { kind: 'through' }
  // How many requests are waiting at the gate right now. A waiting line is only evidence of an
  // outstanding read while something is actually parked, so cases read this rather than assume
  // the answer has not come back yet; a screen that replaces its read parks two at once.
  let waiting = 0
  for (const endpoint of endpoints) {
    await page.route(endpoint, async (route: Route) => {
      // The gate is read first and the answer after it: a case that holds a request switches
      // the knob and then releases, so the released request gets the answer it turned to.
      const gate = answer.until
      if (gate !== undefined) {
        waiting++
        try {
          await gate
        } finally {
          waiting--
        }
      }
      const answering = answer
      if (answering.kind === 'through') {
        await route.continue()
        return
      }
      if (answering.kind === 'status') {
        await route.fulfill({
          status: answering.status,
          contentType: 'application/json',
          body: '{}',
        })
        return
      }
      // The service's own report, so nothing but what the case edits changes on screen.
      const response = await route.fetch()
      const report = (await response.json()) as HealthReport
      await route.fulfill({
        status: answering.status ?? 200,
        contentType: 'application/json',
        body: JSON.stringify(answering.edit(report)),
      })
    })
  }
  return {
    answer(next: Answer) {
      answer = next
    },
    // A parked request has not been answered, so anything the page draws while this is above
    // zero was drawn against a read that is genuinely outstanding, and the number says how many.
    parked() {
      return waiting
    },
  }
}

// attemptCount is every outcome a bucket holds, which is what the page stacks a column from.
function attemptCount(bucket: HistoryBucket): number {
  return bucket.processed + bucket.released + bucket.failed + bucket.timed_out
}

// countNoun is the page's rule for a tally — one is singular, everything else including zero
// takes the plural — and the one place a noun's wording lives on this side. It is mirrored
// here rather than imported so the smoke can disagree with the page instead of sharing its
// answer; the page's own countNoun words the same three nouns.
function countNoun(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? '' : 's'}`
}

// killWorker sends the crash and reports whether the process was still there. A worker that
// exited between the report and the signal is the supervisor's to replace, so that race is a
// retry rather than a test failure.
function killWorker(pid: number): boolean {
  try {
    process.kill(pid, 'SIGKILL')
    return true
  } catch (failure) {
    if ((failure as NodeJS.ErrnoException).code === 'ESRCH') {
      return false
    }
    throw failure
  }
}

// crashyWorker reads the third supervisor's worker, which is the one this smoke kills.
async function crashyWorker(): Promise<WorkerReport | undefined> {
  const report = (await getJSON(`${base}/api/v1/health`)) as HealthReport
  return (report.supervisors ?? []).find((supervisor) => supervisor.id === crashy)?.workers?.[0]
}

// waitForCrashyWorker polls that worker until it matches, so a crash, a restart and a state
// the manager has not written yet are all a longer wait rather than a flake.
async function waitForCrashyWorker(
  what: string,
  matches: (worker: WorkerReport) => boolean,
): Promise<WorkerReport> {
  await waitFor(what, async () => {
    const worker = await crashyWorker()
    return worker !== undefined && matches(worker)
  })

  const worker = await crashyWorker()
  if (worker === undefined) {
    throw new Error(`the ${crashy} supervisor did not ${what}`)
  }
  return worker
}

// stateCell is the workers table's state column: the state, then the badges the report asked
// for, which the page renders against it with no separator between them.
function stateCell(worker: WorkerReport): string {
  return worker.state + (worker.crash_looping ? 'crash looping' : '') + (worker.adopted ? 'adopted' : '')
}

// numberCell is the page's wording for a sampled depth: a count the driver could not report
// is "unknown", never a zero it did not sample.
function numberCell(value: number | null): string {
  return value === null ? 'unknown' : String(value)
}

// durationText is the jobs table's and the detail pane's wording for an attempt: milliseconds
// when it was quick, tenths of a second when it was not.
function durationText(milliseconds: number): string {
  return milliseconds < 1000 ? `${milliseconds} ms` : `${(milliseconds / 1000).toFixed(1)} s`
}

// averageMs is what the page divides a bucket's duration by, and what it writes in a column's
// title: nothing at all for a bucket that ran nothing, rather than an instant job.
function averageMs(bucket: HistoryBucket): number {
  const attempts = attemptCount(bucket)
  return attempts === 0 ? 0 : Math.round(bucket.duration_ms / attempts)
}

// RecordedJob is the part of a stored attempt these waits read: that the worker ran this
// class at all, what it threw when it failed, and when it finished.
interface RecordedJob {
  class?: string
  uuid?: string
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

// AuditRecord is the part of an audit line this file reads, in the shape the service writes
// it: one JSON object per line under msg=audit (docs/adr/0010).
interface AuditRecord {
  msg?: string
  action?: string
  outcome?: string
  user?: string
  role?: string
  address?: string
  detail?: string
}

// auditLines reads the audit records the service has written so far. The file is the one the
// service's own stderr was redirected to, so it also holds ordinary log lines and a line that
// is still being written: neither parses as a record, and both are skipped.
function auditLines(): AuditRecord[] {
  let text = ''
  try {
    text = fs.readFileSync(path.join(dir, 'gohorizon.log'), 'utf8')
  } catch {
    return []
  }
  const records: AuditRecord[] = []
  for (const line of text.split('\n')) {
    if (line.trim() === '') {
      continue
    }
    try {
      const record = JSON.parse(line) as AuditRecord
      if (record.msg === 'audit') {
        records.push(record)
      }
    } catch {
      // Not a line this file wrote.
    }
  }
  return records
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
