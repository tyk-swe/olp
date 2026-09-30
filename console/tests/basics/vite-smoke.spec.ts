import { readFileSync } from 'node:fs';
import { expect, test } from '../playwright';
import { gatewayOwner } from '../gateway/signIn';
import { manage, provisionGenerationRoute } from '../helpers/management';
import { waitForRoutePublication } from '../journeys/fixtures';

test('Vite setup, cookies, CSRF, protected deep links and inference proxy work through the browser', async ({
  page,
  context
}) => {
  await page.goto('/providers/new');
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByLabel('Display name').fill(gatewayOwner.name);
  await page.getByLabel('Work email').fill(gatewayOwner.email);
  await page
    .getByLabel('Password', { exact: true })
    .fill(gatewayOwner.password);
  await page.getByLabel('Confirm password').fill(gatewayOwner.password);
  await page
    .getByLabel('Setup token')
    .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
  await page.getByRole('button', { name: 'Create owner account' }).click();
  await expect(page).toHaveURL(/\/$/);

  const cookies = (await context.cookies()).filter((cookie) =>
    cookie.name.startsWith('__Host-olp_')
  );
  expect(
    cookies.some(
      (cookie) => cookie.name === '__Host-olp_session' && cookie.httpOnly
    )
  ).toBe(true);
  expect(
    cookies.some(
      (cookie) => cookie.name === '__Host-olp_csrf' && !cookie.httpOnly
    )
  ).toBe(true);
  for (const cookie of cookies) {
    expect(cookie.domain).toBe('localhost');
    expect(cookie.path).toBe('/');
    expect(cookie.secure).toBe(true);
    expect(cookie.sameSite).toBe('Lax');
  }

  const rejected = await page.evaluate(async () => {
    const current = await (await fetch('/api/v1/profile')).json();
    const response = await fetch('/api/v1/profile', {
      method: 'PATCH',
      headers: {
        'Content-Type': 'application/json',
        'If-Match': `"${current.etag}"`
      },
      body: JSON.stringify({ display_name: 'Missing CSRF' })
    });
    return { status: response.status, problem: await response.json() };
  });
  expect(rejected.status).toBe(403);
  expect(rejected.problem.type).toMatch(/csrf_invalid$/);
  await page.goto('/settings/profile');
  await page.getByLabel('Display name').fill('Vite Owner');
  await page.getByRole('button', { name: 'Save profile' }).click();
  await expect(page.getByText('Profile updated.')).toBeVisible();

  await page.getByRole('button', { name: 'Open account menu' }).click();
  await page
    .getByRole('banner')
    .getByRole('button', { name: 'Sign out', exact: true })
    .click();
  await page.goto('/providers/new');
  await expect(page).toHaveURL(/\/login\?return_to=%2Fproviders%2Fnew$/);
  await page.getByLabel('Email').fill(gatewayOwner.email);
  await page
    .getByLabel('Password', { exact: true })
    .fill(gatewayOwner.password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page).toHaveURL(/\/providers\/new$/);
  await expect(
    page.getByRole('heading', { name: 'Connect an upstream provider.' })
  ).toBeVisible();

  const route = 'vite-proxy-smoke';
  await provisionGenerationRoute(page, {
    providerName: 'Vite proxy upstream',
    route,
    endpoint: 'http://127.0.0.1:4187/v1',
    model: 'compatible-e2e-model',
    credential: 'compatible-provider-secret'
  });
  const key = await manage<{ secret: string }>(
    page,
    'POST',
    '/api/v1/api-keys',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: 'Vite proxy key',
        scopes: ['inference', 'models_read'],
        allowed_routes: [route]
      }
    }
  );
  expect(key.status).toBe(201);
  await waitForRoutePublication(page, key.body.secret, route);
  const replies = await page.evaluate(
    async ({ secret, route }) => {
      const replies = [];
      for (const stream of [false, true]) {
        const response = await fetch('/v1/chat/completions', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${secret}`
          },
          body: JSON.stringify({
            model: route,
            messages: [{ role: 'user', content: 'Hello' }],
            stream
          })
        });
        replies.push({
          status: response.status,
          type: response.headers.get('content-type'),
          body: await response.text()
        });
      }
      return replies;
    },
    { secret: key.body.secret, route }
  );
  expect(replies[0].status).toBe(200);
  expect(JSON.parse(replies[0].body).choices[0].message.content).toBe(
    'Hello from the compatible upstream'
  );
  expect(replies[1].status).toBe(200);
  expect(replies[1].type).toContain('text/event-stream');
  expect(replies[1].body).toContain('data: [DONE]');
});
