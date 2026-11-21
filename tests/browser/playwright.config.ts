import os from 'node:os'
import path from 'node:path'
import { defineConfig, devices } from '@playwright/test'

// One project, one worker, one service: the smoke shares a store and a queue, and its spec
// starts that service in `beforeAll`, so a second worker would start a second one.
export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  fullyParallel: false,
  workers: 1,
  // The service is warm when the specs run, but building it and a cold Chromium on a loaded
  // CI runner are not instant.
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: [['list']],
  // Artifacts go to the temporary directory: the repository commits its dashboard bundle
  // and should not also collect test output.
  outputDir: path.join(os.tmpdir(), 'gohorizon-browser-results'),
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
