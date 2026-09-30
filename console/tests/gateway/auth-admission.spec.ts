import { expect, test } from '../playwright';
import { signInGatewayOwner } from './signIn';

test('a later gateway owner login honors an admission retry', async ({
  page
}) => {
  await signInGatewayOwner(page, { reuseSession: false });
  let logins = 0;
  page.on('request', (request) => {
    if (
      request.method() === 'POST' &&
      new URL(request.url()).pathname === '/api/v1/sessions'
    )
      logins++;
  });
  await page.context().clearCookies();
  await signInGatewayOwner(page);
  expect(logins).toBe(0);

  // Revoke the cached session through the UI. The helper must detect it and
  // renew through the login form, including its real admission retry path.
  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page
    .getByRole('banner')
    .getByRole('button', { name: 'Sign out', exact: true })
    .click();
  await expect(page).toHaveURL(/\/login/);

  let attempts = 0;
  await page.route('**/api/v1/sessions', async (route) => {
    attempts++;
    if (attempts === 1) {
      await route.fulfill({
        status: 429,
        headers: {
          'content-type': 'application/problem+json',
          'cache-control': 'no-store',
          'retry-after': '1'
        },
        body: JSON.stringify({
          type: 'https://openllmproxy.dev/problems/authentication_rate_limited',
          title: 'Too Many Requests',
          status: 429,
          detail: 'Too many attempts. Try again in a minute.'
        })
      });
    } else {
      await route.continue();
    }
  });
  await signInGatewayOwner(page);
  expect(attempts).toBe(2);
});
