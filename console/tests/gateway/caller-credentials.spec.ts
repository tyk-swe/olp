import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { manage, provisionGenerationRoute } from '../helpers/management';
import { signInGatewayOwner } from './signIn';
import { waitForRoutePublication } from '../journeys/fixtures';

test('publishes caller credentials and cost exemption without removing rate limits', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const route = 'caller-chat',
    credential = 'compatible-provider-secret';
  const { provider, draft } = await provisionGenerationRoute(page, {
    providerName: 'Caller connection',
    route,
    endpoint: 'http://127.0.0.1:4187/v1',
    model: 'compatible-e2e-model',
    credential,
    credentialSource: 'caller'
  });
  await page.goto(`/providers/${provider.id}`);
  await expect(page.getByLabel('Serving credentials')).toHaveValue('caller');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.goto(`/routes/${draft.id}`);
  const exemption = page.getByRole('checkbox', {
    name: 'Exempt caller-paid attempts from cost budgets'
  });
  await exemption.check();
  const saving = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' &&
      new URL(response.url()).pathname === `/api/v1/route-drafts/${draft.id}`
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  expect((await saving).status()).toBe(200);
  const saved = await manage(page, 'GET', `/api/v1/route-drafts/${draft.id}`);
  expect(saved.body.caller_cost_exempt).toBe(true);
  expect(
    (
      await manage(page, 'POST', `/api/v1/route-drafts/${draft.id}/activate`, {
        match: `/api/v1/route-drafts/${draft.id}`,
        idempotency: crypto.randomUUID()
      })
    ).status
  ).toBe(200);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page
    .locator('fieldset')
    .filter({ has: exemption })
    .screenshot({ path: info.outputPath('caller-cost-policy.png') });
  const key = await manage<{ secret: string }>(
    page,
    'POST',
    '/api/v1/api-keys',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: 'Caller rate key',
        scopes: ['inference', 'models_read'],
        allowed_routes: [route],
        requests_per_minute: 1,
        daily_cost_limit: '1'
      }
    }
  );
  expect(key.status).toBe(201);
  await waitForRoutePublication(page, key.body.secret, route);
  for (const expected of [200, 429]) {
    const status = await page.evaluate(
      async ({ apiKey, route, credential }) => {
        const response = await fetch('/v1/chat/completions', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${apiKey}`,
            'X-OLP-Provider-Credential': credential
          },
          body: JSON.stringify({
            model: route,
            messages: [{ role: 'user', content: 'Hello' }]
          })
        });
        await response.arrayBuffer();
        return response.status;
      },
      { apiKey: key.body.secret, route, credential }
    );
    expect(status).toBe(expected);
  }
});
