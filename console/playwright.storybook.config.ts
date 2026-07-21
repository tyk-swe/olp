import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests/storybook',
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results',
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: 'http://127.0.0.1:6007',
    ...devices['Desktop Chrome'],
    reducedMotion: 'reduce',
    trace: 'retain-on-failure'
  },
  webServer: {
    command: 'pnpm exec vite preview --config vite.storybook-preview.config.ts --host 127.0.0.1 --port 6007 --strictPort',
    url: 'http://127.0.0.1:6007',
    reuseExistingServer: !process.env.CI,
    timeout: 120_000
  }
});
