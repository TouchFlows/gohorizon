import fs from 'node:fs'
import { stopGroup } from './global-setup'
import { readState, statePath } from './state'

export default async function globalTeardown(): Promise<void> {
  const state = readState()
  if (!state.skipped) {
    stopGroup(state.pid)
    if (state.dir !== undefined) {
      fs.rmSync(state.dir, { recursive: true, force: true })
    }
  }
  fs.rmSync(statePath, { force: true })
}
