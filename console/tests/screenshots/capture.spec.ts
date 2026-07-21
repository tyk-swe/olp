import { expect, test } from '@playwright/test';
import {
  mockApiKeys,
  mockPlaygroundRun,
  mockProviders,
  mockRecentRequests,
  mockRoutes,
  mockShell,
  mockUsage,
  SCREENSHOT_NOW
} from './fixtures';

// Playwright resolves screenshot paths against the process CWD, which is the
// console directory for `pnpm screenshots`.
const outputDir = '../docs/assets/screenshots';

async function waitForStableLayout(page: import('@playwright/test').Page) {
  await page.evaluate(async () => {
    await document.fonts.ready;
    await new Promise<void>((resolve) => {
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    });
  });
}

async function capture(page: import('@playwright/test').Page, name: string) {
  // Collapse the shell's min-height so the capture is exactly the content
  // height: short pages keep no empty canvas, and the sticky 100dvh sidebar
  // spans the full frame instead of stopping at one viewport.
  await page.addStyleTag({
    content: [
      'html, body, .shell { min-height: 0 !important; }',
      '.desktop-sidebar { height: auto !important; }'
    ].join('\n')
  });
  await waitForStableLayout(page);
  const height = await page.evaluate(() =>
    Math.ceil(document.querySelector('.shell')?.getBoundingClientRect().height ?? 900)
  );
  await page.setViewportSize({ width: 1440, height: Math.max(480, height) });
  await waitForStableLayout(page);
  const first = await page.screenshot({ animations: 'disabled' });
  await waitForStableLayout(page);
  const repeated = await page.screenshot({
    path: `${outputDir}/${name}.png`,
    animations: 'disabled'
  });
  expect(repeated).toEqual(first);
}

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(SCREENSHOT_NOW);
  await mockShell(page);
});

test('overview dashboard', async ({ page }) => {
  await mockProviders(page);
  await mockRoutes(page);
  await mockApiKeys(page);
  await mockRecentRequests(page);

  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Bring your first model route online.' })).toBeVisible();
  await expect(page.getByText('3 active').first()).toBeVisible();
  await expect(page.getByRole('region', { name: 'Five most recent requests' })).toContainText('support-chat');
  await capture(page, 'overview');
});

test('providers list', async ({ page }) => {
  await mockProviders(page);

  await page.goto('/providers');
  await expect(page.getByRole('heading', { name: 'Providers' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'openai-production' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'vertex-gemini' })).toBeVisible();
  await capture(page, 'providers');
});

test('routes list', async ({ page }) => {
  await mockRoutes(page);

  await page.goto('/routes');
  await expect(page.getByRole('heading', { name: 'Routes', exact: true })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'support-chat' })).toBeVisible();
  await expect(page.getByRole('cell', { name: 'vision-triage' })).toBeVisible();
  await capture(page, 'routes');
});

test('api keys list', async ({ page }) => {
  await mockApiKeys(page);

  await page.goto('/api-keys');
  await expect(page.getByRole('heading', { name: 'API Keys' })).toBeVisible();
  await expect(page.getByText('production-web-app')).toBeVisible();
  await expect(page.getByText('partner-catalog-readonly')).toBeVisible();
  await capture(page, 'api-keys');
});

test('usage dashboard', async ({ page }) => {
  await mockUsage(page);

  await page.goto('/usage');
  await expect(page.getByRole('heading', { name: 'Usage', exact: true })).toBeVisible();
  await expect(page.getByText('support-chat')).toBeVisible();
  await capture(page, 'usage');
});

test('playground with completed run', async ({ page }) => {
  await mockPlaygroundRun(page);

  await page.goto('/playground');
  await expect(page.getByRole('heading', { name: 'Playground' })).toBeVisible();
  await page.getByLabel('Route slug').fill('support-chat');
  await page
    .getByLabel('Prompt')
    .fill('A customer asks how to return an order. Reply as the support assistant.');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.getByRole('heading', { name: 'Result' })).toBeVisible();
  await expect(page.getByText('Welcome back!')).toBeVisible();
  await capture(page, 'playground');
});
