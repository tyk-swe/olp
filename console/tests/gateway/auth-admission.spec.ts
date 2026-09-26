import { expect, test } from '../playwright';
import { signInGatewayOwner } from './signIn';

test('a later gateway owner login honors an admission retry', async ({
  page
}) => {
  await signInGatewayOwner(page);
  await page.context().clearCookies();

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
