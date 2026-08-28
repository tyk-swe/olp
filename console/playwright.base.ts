import type { PlaywrightTestConfig } from '@playwright/test';

/**
 * Settings every Playwright config in this package shares. Each config
 * spreads this and then states only what makes it different: which tests it
 * runs, which servers it starts, and how strictly it serialises.
 */
export const baseConfig = {
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results',
  forbidOnly: Boolean(process.env.CI),
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    reducedMotion: 'reduce',
    trace: 'retain-on-failure'
  }
} satisfies PlaywrightTestConfig;
