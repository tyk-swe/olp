import { readFileSync } from 'node:fs';
import { expect, test, type Page } from '@playwright/test';

const password = 'a long browser test password';

async function changeRemote(
  page: Page,
  path: string,
  method: 'PATCH' | 'PUT',
  body: Record<string, unknown>
) {
  const status = await page.evaluate(
    async ({ path, method, body }) => {
      const [resource, session] = await Promise.all([
        fetch(path).then((response) => response.json()),
        fetch('/api/v3/sessions/current').then((response) => response.json())
      ]);
      const response = await fetch(path, {
        method,
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': session.csrf_token,
          'If-Match': `"${resource.etag}"`
        },
        body: JSON.stringify(body)
      });
      return response.status;
    },
    { path, method, body }
  );
  expect(status).toBe(200);
}

async function signOut(page: Page) {
  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page
    .getByRole('banner')
    .getByRole('button', { name: 'Sign out', exact: true })
    .click();
  await expect(page).toHaveURL(/\/login/);
}

test('setup, invitations, key policy, profile, settings, audit, and OIDC work through this origin', async ({
  page,
  browser
}, info) => {
  const failures: string[] = [];
  page.on('pageerror', (error) => failures.push(error.message));
  page.on('response', (response) => {
    if (response.url().includes('/api/v3/') && response.status() >= 500)
      failures.push(`${response.status()} ${new URL(response.url()).pathname}`);
  });
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByLabel('Display name').fill('Owner');
  await page.getByLabel('Work email').fill('owner@example.com');
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByLabel('Confirm password').fill(password);
  await page
    .getByLabel('Setup token')
    .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
  await page.getByRole('button', { name: 'Create owner account' }).click();
  await expect(
    page.getByRole('heading', { name: 'Access and control' })
  ).toBeVisible();
  await page.screenshot({
    path: info.outputPath('overview.png'),
    fullPage: true
  });

  await page.goto('/access');
  await page.getByRole('button', { name: 'Invite member' }).click();
  await page.getByLabel('Email address').fill('viewer@example.com');
  await page.getByLabel('Role').selectOption('viewer');
  await page.getByRole('button', { name: 'Create invitation' }).click();
  const invitation = page.getByRole('dialog', {
    name: 'Copy the invitation link now.'
  });
  const link = (await invitation
    .locator('.invitation-token')
    .textContent())!.trim();
  await invitation.getByRole('button', { name: 'I have shared it' }).click();
  const invitedContext = await browser.newContext({
    baseURL: info.project.use.baseURL
  });
  const invited = await invitedContext.newPage();
  await invited.goto(link);
  await invited.getByLabel('Display name').fill('Viewer');
  await invited.getByLabel('Password', { exact: true }).fill(password);
  await invited.getByLabel('Confirm password').fill(password);
  await invited.getByRole('button', { name: 'Accept invitation' }).click();
  await expect(
    invited.getByRole('heading', { name: 'Access and control' })
  ).toBeVisible();
  expect(
    await invited.evaluate(async () => (await fetch('/api/v3/users')).status)
  ).toBe(403);
  await invitedContext.close();

  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill('Browser application');
  await page.getByLabel('Route slugs (comma separated)').fill('planned-route');
  await page.getByLabel('Requests per minute').fill('60');
  await page
    .getByRole('button', { name: 'Create and show key', exact: true })
    .click();
  const secret = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(secret).toBeVisible();
  await expect(
    secret.getByRole('button', { name: 'Run connection test' })
  ).toHaveCount(0);
  await secret.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(
    page.getByText('Browser application', { exact: true })
  ).toBeVisible();
  await expect(page.getByText('Saved policy · not enforced')).toBeVisible();
  await page.screenshot({ path: info.outputPath('keys.png'), fullPage: true });

  await page.goto('/settings/profile');
  await page.getByLabel('Display name').fill('Unsaved owner');
  await changeRemote(page, '/api/v3/profile', 'PATCH', {
    display_name: 'Remote Owner'
  });
  const staleProfile = page.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v3/profile') &&
      response.request().method() === 'PATCH'
  );
  await page.getByRole('button', { name: 'Save profile' }).click();
  expect((await staleProfile).status()).toBe(412);
  await expect(page.getByText('This item changed elsewhere.')).toBeVisible();
  await expect(page.getByLabel('Display name')).toHaveValue('Unsaved owner');
  await page.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(page.getByLabel('Display name')).toHaveValue('Remote Owner');
  await page.getByLabel('Display name').fill('Updated Owner');
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(
    page.getByRole('button', { name: 'Open account menu' })
  ).toContainText('Updated Owner');
  await page.goto('/settings');
  const retention = page
    .locator('.setting-row')
    .filter({ has: page.locator('[id="setting-retention.audit_days"]') });
  await retention.locator('input').fill('180');
  await changeRemote(page, '/api/v3/settings/retention.audit_days', 'PUT', {
    value: '190'
  });
  const staleSetting = page.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v3/settings/retention.audit_days') &&
      response.request().method() === 'PUT'
  );
  await retention.getByRole('button', { name: 'Save', exact: true }).click();
  expect((await staleSetting).status()).toBe(412);
  await expect(retention.locator('input')).toHaveValue('180');
  await page
    .getByRole('button', {
      name: 'Discard this edit and reload the current setting'
    })
    .click();
  await expect(retention.locator('input')).toHaveValue('190');
  await expect(
    page.getByRole('button', {
      name: 'Discard this edit and reload the current setting'
    })
  ).toHaveCount(0);
  await retention.locator('input').fill('180');
  await retention.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(
    retention.getByRole('button', { name: 'Save', exact: true })
  ).toBeDisabled();
  await page.screenshot({
    path: info.outputPath('settings.png'),
    fullPage: true
  });
  await page.goto('/audit');
  await expect(page.getByText('api_key.create', { exact: true })).toBeVisible();
  await page.screenshot({ path: info.outputPath('audit.png'), fullPage: true });

  await page.goto('/access');
  await page.getByRole('button', { name: 'OIDC', exact: true }).click();
  await page.getByLabel('Expected issuer').fill('http://127.0.0.1:4186');
  await page
    .getByLabel('Discovery URL')
    .fill('http://127.0.0.1:4186/.well-known/openid-configuration');
  await page.getByLabel('Client ID').fill('browser-client');
  await page.getByLabel('Client secret').fill('write-only-browser-secret');
  await page.getByLabel('Enabled', { exact: true }).check();
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(
    page.getByText('OIDC configuration validated and enabled.')
  ).toBeVisible();
  await page.getByLabel('Client ID').fill('unsaved-client');
  await changeRemote(page, '/api/v3/oidc/configuration', 'PUT', {
    issuer: 'http://127.0.0.1:4186',
    discovery_url: 'http://127.0.0.1:4186/.well-known/openid-configuration',
    client_id: 'remote-browser-client',
    enabled: true,
    default_role: 'viewer'
  });
  const staleOidc = page.waitForResponse(
    (response) =>
      response.url().endsWith('/api/v3/oidc/configuration') &&
      response.request().method() === 'PUT'
  );
  await page.getByRole('button', { name: 'Save and validate' }).click();
  expect((await staleOidc).status()).toBe(412);
  await expect(page.getByText('This item changed elsewhere.')).toBeVisible();
  await expect(page.getByLabel('Client ID')).toHaveValue('unsaved-client');
  await page.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(page.getByLabel('Client ID')).toHaveValue(
    'remote-browser-client'
  );
  await page.getByLabel('Client ID').fill('browser-client');
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(
    page.getByText('OIDC configuration validated and enabled.')
  ).toBeVisible();
  for (const returnTo of [
    '/\t/evil.example',
    '/\r/evil.example',
    '/\n/evil.example'
  ]) {
    const rejected = await page.evaluate(async (returnTo) => {
      // Chromium strips these controls and would navigate to another origin.
      const destination = new URL(returnTo, location.href);
      const post = await fetch('/api/v3/oidc/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ return_to: returnTo })
      });
      const get = await fetch(
        '/api/v3/oidc/login?return_to=' + encodeURIComponent(returnTo),
        {
          redirect: 'manual'
        }
      );
      return {
        crossOrigin: destination.origin !== location.origin,
        post: post.status,
        get: get.status
      };
    }, returnTo);
    expect(rejected).toEqual({ crossOrigin: true, post: 422, get: 422 });
  }
  await signOut(page);
  await page.getByRole('link', { name: /single sign-on|OIDC/ }).click();
  await expect(
    page.getByRole('heading', { name: 'Access and control' })
  ).toBeVisible();
  await page.goto('/settings/profile');
  await expect(page.getByLabel('Display name')).toHaveValue('SSO Member');
  await expect(page.getByText('Updated Owner', { exact: true })).toHaveCount(0);
  await page.screenshot({
    path: info.outputPath('oidc-profile.png'),
    fullPage: true
  });
  await signOut(page);
  await page.getByLabel('Email').fill('viewer@example.com');
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'Access and control' })
  ).toBeVisible();
  await page.goto('/api-keys');
  await expect(page.getByRole('link', { name: 'Create key' })).toHaveCount(0);
  expect(failures).toEqual([]);
});
