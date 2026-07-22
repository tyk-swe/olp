import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

test('profile updates identity, changes password, and revokes another session', async ({ page }) => {
  const userId = '01980000-0000-7000-8000-000000000001';
  let displayName = 'Ada Owner';
  let sessions = [
    { id: '01980000-0000-7000-8000-000000000010', user_id: userId, current: true, expires_at: '2026-08-12T12:00:00Z', last_seen_at: '2026-07-12T12:00:00Z', created_at: '2026-07-01T12:00:00Z' },
    { id: '01980000-0000-7000-8000-000000000011', user_id: userId, current: false, expires_at: '2026-08-12T12:00:00Z', last_seen_at: '2026-07-11T12:00:00Z', created_at: '2026-07-01T12:00:00Z' }
  ];
  let identities = [{ id: '01980000-0000-7000-8000-000000000030', issuer: 'https://id.example.test', email_at_link: 'owner@example.com', last_login_at: '2026-07-12T11:00:00Z', created_at: '2026-07-01T12:00:00Z', can_unlink: true }];
  await page.route('**/api/v1/sessions/current', async (route) => route.fulfill({ json: { user: { id: userId, email: 'owner@example.com', display_name: displayName, role: 'owner' }, csrf_token: 'csrf-test-token' } }));
  await page.route('**/api/v1/profile', async (route) => {
    if (route.request().method() === 'PATCH') displayName = (route.request().postDataJSON() as { display_name: string }).display_name;
    await route.fulfill({ json: { id: userId, email: 'owner@example.com', display_name: displayName, role: 'owner', active: true, created_at: '2026-07-01T12:00:00Z', etag: '01980000-0000-7000-8000-000000000020' } });
  });
  await page.route('**/api/v1/profile/password', async (route) => {
    expect(route.request().postDataJSON()).toEqual({
      current_password: 'old password for testing',
      new_password: 'new password for testing'
    });
    expect(route.request().headers()['if-match']).toBe('"01980000-0000-7000-8000-000000000020"');
    await route.fulfill({ json: { id: userId, email: 'owner@example.com', display_name: displayName, role: 'owner', active: true, created_at: '2026-07-01T12:00:00Z', etag: '01980000-0000-7000-8000-000000000022' } });
  });
  await page.route(/\/api\/v1\/sessions(?:\?.*)?$/, async (route) => route.fulfill({ json: { data: sessions, next_cursor: null } }));
  await page.route(/\/api\/v1\/sessions\/[0-9a-f-]{36}$/, async (route) => {
    const id = new URL(route.request().url()).pathname.split('/').at(-1);
    sessions = sessions.filter((session) => session.id !== id);
    await route.fulfill({ status: 204 });
  });
  await page.route(/\/api\/v1\/oidc\/identities(?:\?.*)?$/, async (route) => {
    await route.fulfill({ json: { data: identities, has_local_password: true, linking_available: true } });
  });
  await page.route('**/api/v1/profile/reauthenticate', async (route) => {
    expect(route.request().postDataJSON()).toEqual({
      current_password: 'old password for testing',
      purpose: 'oidc_unlink',
      resource_id: '01980000-0000-7000-8000-000000000030'
    });
    await route.fulfill({ status: 204 });
  });
  await page.route('**/api/v1/oidc/identities/*', async (route) => {
    const id = new URL(route.request().url()).pathname.split('/').at(-1);
    identities = identities.filter((identity) => identity.id !== id);
    await route.fulfill({ status: 204 });
  });
  await page.goto('/settings/profile');
  await expect(page.getByRole('heading', { name: 'Personal profile' })).toBeVisible();
  await page.getByLabel('Display name').fill('');
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(page.getByText('Enter your display name.')).toBeVisible();
  await page.getByLabel('Display name').fill('Ada Operations');
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(page.getByText('Profile updated.')).toBeVisible();
  page.once('dialog', (dialog) => dialog.dismiss());
  await page.getByRole('button', { name: 'Link an OIDC identity' }).click();
  await expect(page.getByRole('button', { name: 'Link an OIDC identity' })).toBeEnabled();
  page.on('dialog', (dialog) =>
    dialog.type() === 'prompt' ? dialog.accept('old password for testing') : dialog.accept()
  );
  await page.getByRole('button', { name: 'Unlink' }).click();
  await expect(page.getByText('OIDC identity unlinked.')).toBeVisible();
  await expect(page.getByText('No OIDC identity is linked to this account.')).toBeVisible();
  await page.getByRole('button', { name: 'Revoke' }).click();
  await expect(page.getByText('Session revoked.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Revoke' })).toHaveCount(0);
  await page.getByLabel('Current password').fill('old password for testing');
  await page.getByLabel('New password', { exact: true }).fill('new password for testing');
  await page.getByLabel('Confirm new password').fill('new password for testing');
  await page.getByRole('button', { name: 'Change password' }).click();
  await expect(
    page.getByText('Password changed. All previous sessions were revoked and this browser was rotated.')
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('OIDC-only profile enrolls a local password before unlinking', async ({ page }) => {
  const userId = '01980000-0000-7000-8000-000000000101';
  let canUnlink = false;
  let hasLocalPassword = false;
  let enrollmentAttempts = 0;
  await page.route('**/api/v1/sessions/current', async (route) =>
    route.fulfill({
      json: {
        user: { id: userId, email: 'oidc@example.com', display_name: 'OIDC User', role: 'developer' },
        csrf_token: 'csrf-test-token'
      }
    })
  );
  await page.route('**/api/v1/profile', async (route) =>
    route.fulfill({
      json: {
        id: userId,
        email: 'oidc@example.com',
        display_name: 'OIDC User',
        role: 'developer',
        active: true,
        created_at: '2026-07-01T12:00:00Z',
        updated_at: '2026-07-12T12:00:00Z',
        etag: '01980000-0000-7000-8000-000000000120'
      }
    })
  );
  await page.route('**/api/v1/profile/password/enroll', async (route) => {
    expect(route.request().postDataJSON()).toEqual({ new_password: 'local password for testing' });
    expect(route.request().headers()['if-match']).toBe('"01980000-0000-7000-8000-000000000120"');
    enrollmentAttempts += 1;
    if (enrollmentAttempts === 1) {
      await route.fulfill({
        status: 428,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          title: 'Recent authentication required',
          status: 428,
          detail: 'Verify your identity again before adding a local password.'
        })
      });
      return;
    }
    canUnlink = true;
    hasLocalPassword = true;
    await route.fulfill({
      json: {
        id: userId,
        email: 'oidc@example.com',
        display_name: 'OIDC User',
        role: 'developer',
        active: true,
        created_at: '2026-07-01T12:00:00Z',
        updated_at: '2026-07-12T12:00:00Z',
        etag: '01980000-0000-7000-8000-000000000121'
      }
    });
  });
  await page.route(/\/api\/v1\/sessions(?:\?.*)?$/, async (route) =>
    route.fulfill({ json: { data: [], next_cursor: null } })
  );
  await page.route(/\/api\/v1\/oidc\/identities(?:\?.*)?$/, async (route) =>
    route.fulfill({
      json: {
        data: [
          {
            id: '01980000-0000-7000-8000-000000000130',
            issuer: 'https://id.example.test',
            email_at_link: 'oidc@example.com',
            last_login_at: '2026-07-12T11:00:00Z',
            created_at: '2026-07-01T12:00:00Z',
            can_unlink: canUnlink
          }
        ],
        has_local_password: hasLocalPassword,
        linking_available: true
      }
    })
  );

  await page.clock.install();
  await page.goto('/settings/profile?reauthenticated=password_enrollment');
  await expect(page.getByRole('heading', { name: 'Add a local password' })).toBeVisible();
  await expect(page.getByLabel('Current password')).toHaveCount(0);
  await page.clock.fastForward(5 * 60 * 1000);
  await expect(page.getByText('Identity verification expired.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Verify identity with OIDC' })).toBeVisible();

  await page.goto('/settings/profile?reauthenticated=password_enrollment');
  await page.getByLabel('New password', { exact: true }).fill('local password for testing');
  await page.getByLabel('Confirm new password').fill('local password for testing');
  await page.getByRole('button', { name: 'Add local password' }).click();
  await expect(page.getByText('Verify your identity again before adding a local password.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Verify identity with OIDC' })).toBeVisible();

  await page.goto('/settings/profile?reauthenticated=password_enrollment');
  await page.getByLabel('New password', { exact: true }).fill('local password for testing');
  await page.getByLabel('Confirm new password').fill('local password for testing');
  await page.getByRole('button', { name: 'Add local password' }).click();
  await expect(
    page.getByText('Local password added. All previous sessions were revoked and this browser was rotated.')
  ).toBeVisible();
  await expect(page.getByRole('button', { name: 'Unlink' })).toBeEnabled();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
