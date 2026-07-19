import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000901',
    email: 'invited@example.com',
    display_name: 'Grace Operator',
    role: 'developer'
  },
  csrf_token: 'csrf-invited-user'
};

test('one-time fragment invitation creates a session without leaking its token', async ({ page }) => {
  let accepted = false;
  let acceptanceBody: unknown;
  await page.route('**/api/v1/invitations/accept', async (route) => {
    acceptanceBody = route.request().postDataJSON();
    accepted = true;
    await route.fulfill({ status: 201, json: session });
  });
  await page.route('**/api/v1/sessions/current', async (route) => {
    await route.fulfill(
      accepted
        ? { json: session }
        : {
            status: 401,
            contentType: 'application/problem+json',
            json: { type: 'about:blank', title: 'Authentication required', status: 401 }
          }
    );
  });
  await page.route('**/api/v1/setup/status', async (route) => {
    await route.fulfill({ json: { setup_required: false } });
  });
  await page.route('**/api/v1/providers*', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/api/v1/routes*', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/api/v1/api-keys*', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/api/v1/requests*', async (route) => {
    await route.fulfill({ json: { data: [], next_cursor: null } });
  });

  const token = 'invite-secret-that-must-not-enter-the-request-url';
  await page.goto(`/invitations/accept#token=${encodeURIComponent(token)}`);
  await expect(page).toHaveURL(/\/invitations\/accept$/);
  await expect(page.getByRole('heading', { name: 'Finish creating your account.' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.getByLabel('Display name').fill('Grace Operator');
  await page.getByLabel('Password', { exact: true }).fill('correct horse battery staple');
  await page.getByLabel('Confirm password').fill('correct horse battery staple');
  await page.getByRole('button', { name: 'Accept invitation' }).click();

  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole('heading', { name: 'Bring your first model route online.' })).toBeVisible();
  expect(acceptanceBody).toEqual({
    token,
    display_name: 'Grace Operator',
    password: 'correct horse battery staple'
  });
});

test('expired invitation has a recoverable public state', async ({ page }) => {
  await page.route('**/api/v1/invitations/accept', async (route) => {
    await route.fulfill({
      status: 410,
      contentType: 'application/problem+json',
      json: { type: 'about:blank', title: 'Invitation unavailable', status: 410 }
    });
  });
  await page.goto('/invitations/accept#token=expired-token');
  await page.getByLabel('Display name').fill('Grace Operator');
  await page.getByLabel('Password', { exact: true }).fill('correct horse battery staple');
  await page.getByLabel('Confirm password').fill('correct horse battery staple');
  await page.getByRole('button', { name: 'Accept invitation' }).click();
  await expect(page.getByRole('heading', { name: 'This invitation can no longer be used.' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Go to sign in' })).toBeVisible();
});
