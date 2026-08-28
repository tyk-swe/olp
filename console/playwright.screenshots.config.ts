import { defineConfig, devices } from '@playwright/test';
import { baseConfig } from './playwright.base';

/**
 * Captures the console overview published in README.md.
 * The suite mocks the management API with deterministic seed data, so it runs
 * without a backend and can be regenerated whenever the UI evolves:
 *
 *   pnpm screenshots
 *
 * PNGs are written to ../docs/assets/screenshots/.
 */
export default defineConfig({
  ...baseConfig,
  testDir: './tests/screenshots',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  // Captures are the deliverable, so nothing else is reported or traced.
  reporter: 'list',
  expect: { timeout: 15_000 },
  use: {
    ...baseConfig.use,
    // 4178: distinct from the integration config's gateway (4175) so the
    // suites cannot collide or silently reuse each other's servers.
    baseURL: 'http://127.0.0.1:4178',
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
    command: 'pnpm dev --host 127.0.0.1 --port 4178 --strictPort',
    url: 'http://127.0.0.1:4178',
    reuseExistingServer: !process.env.CI,
    timeout: 120_000
  }
});
