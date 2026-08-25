import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests/e2e',
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results',
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  expect: {
    // Browser point releases shift anti-aliasing on a handful of pixels
    // (WebKit drifted by 5 px on a 1280x815 page). Layout changes still fail:
    // a 4 px height change in mobile Chromium was ~12k differing pixels.
    toHaveScreenshot: { maxDiffPixels: 50 }
  },
  use: {
    baseURL: 'http://127.0.0.1:4174',
    reducedMotion: 'reduce',
    trace: 'retain-on-failure'
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'firefox', use: { ...devices['Desktop Firefox'] } },
    { name: 'webkit', use: { ...devices['Desktop Safari'] } },
    { name: 'mobile-chromium', use: { ...devices['Pixel 7'] } }
  ],
  webServer: {
    // CI serves the prebuilt adapter-static bundle (the console job's build
    // artifact) so browsers test what ships; local runs keep the dev server.
    command: process.env.CI
      ? 'pnpm exec vite preview --config vite.console-preview.config.ts --host 127.0.0.1 --port 4174 --strictPort'
      : 'pnpm dev --host 127.0.0.1 --port 4174 --strictPort',
    url: 'http://127.0.0.1:4174',
    reuseExistingServer: !process.env.CI,
    timeout: 120_000
  }
});
