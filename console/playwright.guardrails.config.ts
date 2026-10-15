import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests/guardrails',
  outputDir: 'test-results/guardrails',
  workers: 1,
  timeout: 90_000,
  retries: 0,
  forbidOnly: true,
  use: {
    ...devices['Desktop Chrome'],
    baseURL: 'http://localhost:4188',
    reducedMotion: 'reduce',
    trace: 'retain-on-failure'
  },
  webServer: {
    command: 'pnpm dev --host localhost --port 4188 --strictPort',
    url: 'http://localhost:4188',
    reuseExistingServer: false
  }
});
