import os from 'node:os'
import path from 'node:path'
import { defineConfig, devices } from '@playwright/test'

// One project, one worker, one service: the smoke shares a store and a queue, so parallel
// browsers would be racing over the same page of history.
export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  // The service is already warm when the specs run, but a cold Chromium on a loaded CI
  // runner is not instant.
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: [['list']],
  // Artifacts go to the temporary directory: the repository commits its dashboard bundle
  // and should not also collect test output.
  outputDir: path.join(os.tmpdir(), 'gohorizon-browser-results'),
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
