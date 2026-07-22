import { expect, test } from '@playwright/test';

test('protected routes never render before authentication and return after login', async ({ page }) => {
  let authenticated = false;
  let loginBody: unknown;

  await page.route('**/api/v1/setup/status', async (route) => {
    await route.fulfill({ json: { setup_required: false } });
  });
  await page.route('**/api/v1/auth/capabilities', async (route) => {
    await route.fulfill({ json: { local_login_enabled: true, oidc_login_enabled: false } });
  });
  await page.route('**/api/v1/sessions/current', async (route) => {
    if (!authenticated) {
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
        csrf_token: 'csrf-auth-test'
      }
    });
  });
  await page.route('**/api/v1/sessions', async (route) => {
    loginBody = route.request().postDataJSON();
    authenticated = true;
    await route.fulfill({
      status: 201,
      json: {
        user: {
          id: '01980000-0000-7000-8000-000000000001',
          email: 'owner@example.com',
          display_name: 'Ada Owner',
          role: 'owner'
        },
        csrf_token: 'csrf-auth-test'
      }
    });
  });
  await page.route('**/api/v1/providers*', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });

  await page.goto('/providers?view=all#overview');
  await expect(page).toHaveURL(/\/login\?return_to=%2Fproviders%3Fview%3Dall%23overview$/);
  await expect(page.getByRole('heading', { name: 'Providers' })).toHaveCount(0);
  await expect(page.getByRole('link', { name: 'Continue with single sign-on' })).toHaveCount(0);

  await page.getByLabel('Email').fill('owner@example.com');
  await page.getByLabel('Password').fill('correct horse battery staple');
  await page.getByRole('button', { name: 'Sign in' }).click();

  await expect(page).toHaveURL(/\/providers\?view=all#overview$/);
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();
  expect(loginBody).toEqual({
    email: 'owner@example.com',
    password: 'correct horse battery staple'
  });
});

test('failed sign out does not pretend the server session ended', async ({ page }) => {
  await page.route('**/api/v1/sessions/current', async (route) => {
    if (route.request().method() === 'DELETE') {
      await route.fulfill({
        status: 503,
        contentType: 'application/problem+json',
        json: { title: 'Session store unavailable', status: 503 }
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
        csrf_token: 'csrf-auth-test'
      }
    });
  });
  await page.route('**/api/v1/providers*', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });

  await page.goto('/providers');
  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();

  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByRole('alert')).toContainText('Sign out failed');
  await expect(page.getByRole('alert')).toContainText('Your session may still be active');
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();
});

test('sign out treats an already-absent server session as complete', async ({ page }) => {
  await page.route('**/api/v1/sessions/current', async (route) => {
    if (route.request().method() === 'DELETE') {
      await route.fulfill({
        status: 401,
        contentType: 'application/problem+json',
        json: { title: 'No active session', status: 401 }
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
        csrf_token: 'csrf-auth-test'
      }
    });
  });
  await page.route('**/api/v1/providers*', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });

  await page.goto('/providers');
  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();

  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toHaveCount(0);
});

test('login renders only discovered authentication methods', async ({ page }) => {
  await page.route('**/api/v1/auth/capabilities', async (route) => {
    await route.fulfill({ json: { local_login_enabled: false, oidc_login_enabled: true } });
  });
  await page.route('**/api/v1/oidc/login', async (route) => {
    expect(route.request().method()).toBe('POST');
    expect(route.request().postDataJSON()).toEqual({
      return_to: '/settings?tab=security#sessions'
    });
    await route.fulfill({
      json: { authorization_url: new URL('/mock-idp/authorize', route.request().url()).href }
    });
  });
  await page.route('**/mock-idp/authorize', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'text/html',
      body: '<!doctype html><title>Mock identity provider</title>'
    });
  });

  await page.goto('/login?return_to=%2Fsettings%3Ftab%3Dsecurity%23sessions');
  await expect(page.getByLabel('Email')).toHaveCount(0);
  const sso = page.getByRole('link', { name: 'Continue with single sign-on' });
  await expect(sso).toBeVisible();
  await sso.click();
  await expect(page).toHaveURL(/\/mock-idp\/authorize$/);
});

test('OIDC initiation failures remain a recoverable login-page problem', async ({ page }) => {
  await page.route('**/api/v1/auth/capabilities', async (route) => {
    await route.fulfill({ json: { local_login_enabled: true, oidc_login_enabled: true } });
  });
  await page.route('**/api/v1/oidc/login', async (route) => {
    await route.fulfill({
      status: 404,
      contentType: 'application/problem+json',
      json: {
        type: 'about:blank',
        title: 'OIDC unavailable',
        detail: 'Single sign-on was disabled after this page loaded.',
        status: 404
      }
    });
  });

  await page.goto('/login');
  await page.getByRole('link', { name: 'Continue with single sign-on' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole('alert')).toContainText(
    'Single sign-on was disabled after this page loaded.'
  );
  await expect(page.getByLabel('Email')).toBeVisible();
});
