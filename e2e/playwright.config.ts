import { defineConfig, devices } from '@playwright/test'

// Where the server under test is. The default is the Go binary serving the
// embedded build, which is the thing that actually ships — the Vite dev server
// is a development convenience and is not what a customer runs. Point at
// http://localhost:5173 to drive the dev server instead.
const baseURL = process.env.BASE_URL ?? 'http://localhost:8080'

export default defineConfig({
  testDir: './tests',
  // These drive a real audit through DuckDB. It is fast on the fixtures, but
  // not instant, and a cold first run pays for the engine starting up.
  timeout: 60_000,
  expect: { timeout: 15_000 },

  // Deliberately serial. Every test shares one server and one data directory,
  // and the sidebar lists every dataset that exists — parallel uploads would
  // make "the dataset I just added" ambiguous.
  fullyParallel: false,
  workers: 1,

  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',

  use: {
    baseURL,
    headless: true,
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
  },

  projects: [
    // Chromium only, Playwright's own version-matched build. Firefox and WebKit
    // are not pulled: they would triple the download for no extra coverage of
    // what these tests check.
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
  ],
})
