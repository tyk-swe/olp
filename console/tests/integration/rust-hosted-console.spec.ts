import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

const owner = {
  name: 'Integration Owner',
  email: 'console-integration@example.com',
  password: 'correct horse battery staple'
};
// Test-only fixed material. The application still loads it from the mounted
// secret file, while the browser uses the same base64 token through the setup
// field without requiring Node globals in the Svelte typecheck.
const bootstrapToken = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=';

test('Rust serves the console and enforces the real setup/session/management boundary', async ({ page, context }) => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByLabel('Display name').fill(owner.name);
  await page.getByLabel('Work email').fill(owner.email);
  await page.getByLabel('Password', { exact: true }).fill(owner.password);
  await page.getByLabel('Confirm password').fill(owner.password);
  await page.getByLabel('Setup token').fill(bootstrapToken);
  await page.getByRole('button', { name: 'Create owner account' }).click();

  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole('heading', { name: 'Bring your first model route online.' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.getByRole('link', { name: 'Providers' }).click();
  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'No providers configured' })).toBeVisible();

  await page.getByRole('link', { name: 'Access', exact: true }).click();
  await page.getByRole('link', { name: 'Invite member' }).click();
  await page.getByLabel('Email address').fill('invited-integration@example.com');
  await page.getByLabel('Role').selectOption('developer');
  await page.getByRole('button', { name: 'Create invitation' }).click();
  const invitationDialog = page.getByRole('dialog', { name: 'Copy the invitation link now.' });
  const invitationToken = (await invitationDialog.locator('.invitation-token').textContent())?.trim();
  expect(invitationToken).toBeTruthy();
  await invitationDialog.getByRole('button', { name: 'I have shared it' }).click();

  await page.getByRole('button', { name: 'OIDC' }).click();
  await page.getByLabel('Expected issuer').fill('http://127.0.0.1:4176');
  await page
    .getByLabel('Discovery URL')
    .fill('http://127.0.0.1:4176/.well-known/openid-configuration');
  await page.getByLabel('Client ID').fill('console-browser-client');
  await page.getByLabel('Client secret').fill('write-only-browser-secret');
  await page.getByLabel('Enabled').check();
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(page.getByText('OIDC configuration validated and enabled.')).toBeVisible();

  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);

  await page.getByRole('link', { name: 'Continue with single sign-on' }).click();
  await expect(page).toHaveURL(/^http:\/\/127\.0\.0\.1:4176\/authorize\?/);
  const oidcCookies = (await context.cookies('http://localhost:4175')).filter((cookie) =>
    cookie.name.startsWith('__Host-olp_oidc_')
  );
  expect(oidcCookies.map((cookie) => cookie.name)).toEqual(['__Host-olp_oidc_login_flow']);
  for (const cookie of oidcCookies) {
    expect(cookie.domain).toBe('localhost');
    expect(cookie.path).toBe('/');
    expect(cookie.secure).toBe(true);
    expect(cookie.httpOnly).toBe(true);
    expect(cookie.sameSite).toBe('Lax');
  }

  await page.goto('/providers');
  await expect(page).toHaveURL(/\/login\?return_to=%2Fproviders$/);
  await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();

  await page.getByLabel('Email').fill(owner.email);
  await page.getByLabel('Password').fill(owner.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();

  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await page.goto(`/invitations/accept#token=${encodeURIComponent(invitationToken!)}`);
  await expect(page).toHaveURL(/\/invitations\/accept$/);
  await page.getByLabel('Display name').fill('Invited Integration User');
  await page.getByLabel('Password', { exact: true }).fill(owner.password);
  await page.getByLabel('Confirm password').fill(owner.password);
  await page.getByRole('button', { name: 'Accept invitation' }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByText('Invited Integration User')).toBeVisible();
});
