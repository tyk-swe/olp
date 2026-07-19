import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

async function emulateTwoHundredPercentZoom(page: Page) {
  const viewport = page.viewportSize();
  if (!viewport || viewport.width <= 480) return;
  await page.setViewportSize({
    width: Math.max(320, Math.floor(viewport.width / 2)),
    height: Math.max(480, Math.floor(viewport.height / 2))
  });
}

test('creates the first owner through the local setup contract', async ({ page }) => {
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  let setupComplete = false;
  let submittedBody: unknown;
  let idempotencyKey: string | null = null;
  let setupToken: string | null = null;

  await page.route('**/api/v1/setup/status', async (route) => {
    await route.fulfill({ json: { setup_required: !setupComplete } });
  });
  await page.route('**/api/v1/sessions/current', async (route) => {
    if (!setupComplete) {
      await route.fulfill({
        status: 401,
        contentType: 'application/problem+json',
        json: { type: 'about:blank', title: 'Authentication required', status: 401 }
      });
      return;
    }
    await route.fulfill({
      json: {
        user: {
          id: '01980000-0000-7000-8000-000000000001',
          email: 'owner@example.com',
          display_name: 'Ada Owner',
          role: 'owner'
        },
        csrf_token: 'test-csrf-token'
      }
    });
  });
  await page.route('**/api/v1/providers*', async (route) => route.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/routes*', async (route) => route.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/requests*', async (route) => route.fulfill({ json: { data: [], next_cursor: null } }));
  await page.route('**/api/v1/setup', async (route) => {
    if (route.request().method() !== 'POST') return route.fallback();
    submittedBody = route.request().postDataJSON();
    idempotencyKey = route.request().headers()['idempotency-key'] ?? null;
    setupToken = route.request().headers()['x-olp-setup-token'] ?? null;
    setupComplete = true;
    await route.fulfill({
      status: 201,
      json: {
        user: {
          id: '01980000-0000-7000-8000-000000000001',
          email: 'owner@example.com',
          display_name: 'Ada Owner',
          role: 'owner'
        },
        csrf_token: 'test-csrf-token'
      }
    });
  });

  await emulateTwoHundredPercentZoom(page);
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole('heading', { name: 'Create your account' })).toBeVisible();

  const accessibility = await new AxeBuilder({ page }).analyze();
  expect(accessibility.violations).toEqual([]);

  await page.getByLabel('Display name').fill('Ada Owner');
  await page.getByLabel('Work email').fill('owner@example.com');
  await page.getByLabel('Password', { exact: true }).fill('correct horse battery staple');
  await page.getByLabel('Confirm password').fill('correct horse battery staple');
  await page.getByLabel('Setup token').fill('test-bootstrap-token');
  await page.getByRole('button', { name: 'Create owner account' }).click();

  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole('heading', { name: 'Bring your first model route online.' })).toBeVisible();
  const overviewAccessibility = await new AxeBuilder({ page }).analyze();
  expect(overviewAccessibility.violations).toEqual([]);
  expect(submittedBody).toEqual({
    email: 'owner@example.com',
    password: 'correct horse battery staple',
    display_name: 'Ada Owner'
  });
  expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/);
  expect(setupToken).toBe('test-bootstrap-token');
});

test('setup form validation is keyboard-visible and specific', async ({ page }) => {
  await page.route('**/api/v1/setup/status', async (route) => {
    await route.fulfill({ json: { setup_required: true } });
  });

  await page.goto('/setup');
  await page.getByRole('button', { name: 'Create owner account' }).click();

  await expect(page.getByText('Enter your name.')).toBeVisible();
  await expect(page.getByText('Enter your email address.')).toBeVisible();
  await expect(page.getByText('Use at least 12 characters.')).toBeVisible();
  await expect(page.getByText('Enter the setup token.')).toBeVisible();
});
