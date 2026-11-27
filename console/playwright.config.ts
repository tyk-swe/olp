import { defineConfig, devices } from '@playwright/test';
import { origin, session } from './tests/e2e/journey';

export default defineConfig({
  testDir: 'tests/e2e',
  fullyParallel: true,
  forbidOnly: true,
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  timeout: 30_000,
  use: {
    ...devices['Desktop Chrome'],
    baseURL: origin,
    contextOptions: { reducedMotion: 'reduce' },
    trace: 'retain-on-failure'
  },
  projects: [
    { name: 'setup', testMatch: 'setup.ts' },
    {
      name: 'journeys',
      dependencies: ['setup'],
      testIgnore: 'setup.ts',
      use: { storageState: session }
    }
  ]
});
