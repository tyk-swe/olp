import { defineConfig, devices } from '@playwright/test';

/**
 * Captures the console screenshots published in README.md and docs/.
 * The suite mocks the management API with deterministic seed data, so it runs
 * without a backend and can be regenerated whenever the UI evolves:
 *
 *   pnpm screenshots
 *
 * PNGs are written to ../docs/assets/screenshots/.
 */
export default defineConfig({
  testDir: './tests/screenshots',
  fullyParallel: false,
  workers: 1,
  forbidOnly: Boolean(process.env.CI),
  retries: 0,
  reporter: 'list',
  expect: { timeout: 15_000 },
  use: {
    baseURL: 'http://127.0.0.1:4175',
    trace: 'off'
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        colorScheme: 'light',
        locale: 'en-US',
        timezoneId: 'UTC',
        viewport: { width: 1440, height: 900 },
        deviceScaleFactor: 2
      }
    }
  ],
  webServer: {
    command: 'pnpm dev --host 127.0.0.1 --port 4175',
    url: 'http://127.0.0.1:4175',
    reuseExistingServer: !process.env.CI,
    timeout: 120_000
  }
});
