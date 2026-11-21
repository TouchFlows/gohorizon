import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

// The setup runs in the Playwright process and the specs in workers, so the URL and the
// process to stop travel through a file rather than the environment.
export interface SmokeState {
  skipped: boolean
  reason?: string
  url?: string
  pid?: number
  dir?: string
}

export const statePath = path.join(os.tmpdir(), 'gohorizon-browser-smoke.json')

export function writeState(state: SmokeState): void {
  fs.writeFileSync(statePath, JSON.stringify(state, null, 2))
}

export function readState(): SmokeState {
  if (!fs.existsSync(statePath)) {
    return { skipped: true, reason: 'the browser smoke did not run its setup' }
  }
  return JSON.parse(fs.readFileSync(statePath, 'utf8')) as SmokeState
}
