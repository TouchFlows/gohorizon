import { execFileSync, spawn } from 'node:child_process'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { writeState } from './state'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const fixture = path.join(root, 'tests', 'fixtures', 'laravel-app')

// The acceptance suite's fixture talks to the same disposable Redis database, under its
// own application and queue names, so this smoke cannot collide with it or with a service
// an operator is running elsewhere.
const redis = { host: '127.0.0.1', port: 6379, db: 15 }
const application = 'gohorizon-smoke'
const queue = 'gohorizon-smoke'

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

export default async function globalSetup(): Promise<void> {
  if (process.env.GOHORIZON_E2E !== '1') {
    writeState({ skipped: true, reason: 'the browser smoke needs GOHORIZON_E2E=1 (see README)' })
    return
  }
  if (!fs.existsSync(path.join(fixture, 'vendor', 'autoload.php'))) {
    writeState({
      skipped: true,
      reason: `fixture dependencies are not installed; run: cd ${fixture} && composer install`,
    })
    return
  }

  const php = resolvePHP()
  if (php === null) {
    writeState({ skipped: true, reason: 'php is not on PATH' })
    return
  }
  if (!(await redisAnswers())) {
    writeState({ skipped: true, reason: `the browser smoke needs Redis at ${redis.host}:${redis.port}` })
    return
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gohorizon-smoke-'))
  const binary = path.join(dir, 'gohorizon')
  const port = await freePort()
  const url = `http://127.0.0.1:${port}`
  let pid: number | undefined

  try {
    execFileSync('go', ['build', '-o', binary, './cmd/gohorizon'], { cwd: root, stdio: 'inherit' })

    const configPath = path.join(dir, 'gohorizon.yaml')
    fs.writeFileSync(configPath, configYAML(port, dir, php))

    const log = fs.openSync(path.join(dir, 'gohorizon.log'), 'a')
    const service = spawn(binary, ['run', '-config', configPath], {
      cwd: root,
      // Its own group, so teardown can end the worker the supervisor started as well.
      detached: true,
      stdio: ['ignore', log, log],
      env: { ...process.env, ...fixtureEnv },
    })
    fs.closeSync(log)
    if (service.pid === undefined) {
      throw new Error('the service did not start')
    }
    pid = service.pid
    writeState({ skipped: false, url, pid, dir })

    await waitFor(url, 30_000, async () => {
      const report = await getJSON(`${url}/api/v1/health`)
      const depths = (report as { queues?: { depths?: unknown[] } }).queues?.depths
      return Array.isArray(depths) && depths.length > 0
    }, () => readLog(dir))

    execFileSync(php, ['dispatch.php', 'marker', path.join(dir, 'smoke-marker')], {
      cwd: fixture,
      env: { ...process.env, ...fixtureEnv, GOHORIZON_E2E_QUEUE: queue },
      stdio: 'inherit',
    })

    // The worker has to pick the job up and the recorder has to store it before a browser
    // can be asked to show it.
    await waitFor(`${url}/api/v1/jobs?queue=${queue}&limit=10`, 30_000, async () => {
      const page = await getJSON(`${url}/api/v1/jobs?queue=${queue}&limit=10`)
      const jobs = (page as { jobs?: { class?: string }[] }).jobs ?? []
      return jobs.some((job) => job.class === 'App\\Jobs\\RecordMarker')
    }, () => readLog(dir))
  } catch (failure) {
    stopGroup(pid)
    fs.rmSync(dir, { recursive: true, force: true })
    writeState({ skipped: true, reason: `the smoke service did not come up: ${String(failure)}` })
    throw failure
  }
}

function resolvePHP(): string | null {
  try {
    return execFileSync('sh', ['-c', 'command -v php'], { encoding: 'utf8' }).trim() || null
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

// redisAnswers only checks that something accepts a connection: the service's own health
// read is what proves the client can talk to it.
async function redisAnswers(): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = net.connect(redis.port, redis.host)
    socket.setTimeout(1_000)
    socket.on('connect', () => {
      socket.destroy()
      resolve(true)
    })
    socket.on('timeout', () => {
      socket.destroy()
      resolve(false)
    })
    socket.on('error', () => resolve(false))
  })
}

async function getJSON(url: string, timeout = 3_000): Promise<unknown> {
  const response = await fetch(url, { signal: AbortSignal.timeout(timeout) })
  if (!response.ok) {
    throw new Error(`${url} returned ${response.status}`)
  }
  return response.json()
}

// waitFor polls until the condition holds, so a slow worker or a slow first inspect is a
// longer wait rather than a flaky test.
async function waitFor(
  url: string,
  timeout: number,
  condition: () => Promise<boolean>,
  log: () => string,
): Promise<void> {
  const deadline = Date.now() + timeout
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
  throw new Error(`${url} did not answer in ${timeout} ms (${last})\n${log()}`)
}

function readLog(dir: string): string {
  try {
    return fs.readFileSync(path.join(dir, 'gohorizon.log'), 'utf8').split('\n').slice(-40).join('\n')
  } catch {
    return ''
  }
}

// stopGroup ends the service's process group: the supervisor handles SIGTERM by stopping
// its worker, and the group kill is what guarantees no worker of the smoke survives.
export function stopGroup(pid: number | undefined): void {
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
    sleepSync(200)
  }
  try {
    process.kill(-pid, 'SIGKILL')
  } catch {
    // already gone
  }
}

function sleepSync(milliseconds: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, milliseconds)
}
