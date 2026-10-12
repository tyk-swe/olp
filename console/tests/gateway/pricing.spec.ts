import { manage, provisionGenerationRoute } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '../playwright';
import { takeSecret, waitForRoutePublication } from '../journeys/fixtures';

test.describe.configure({ mode: 'serial' });

const upstream = {
  origin: 'http://127.0.0.1:4187',
  endpoint: 'http://127.0.0.1:4187/v1',
  model: 'compatible-e2e-model',
  credential: 'compatible-provider-secret'
};
const provider = 'Priced upstream';
const route = 'pricing-chat';
const price = { input: '1000.00', output: '2600.00' };

// Provision the priced provider and published route every pricing journey
// needs, then keep the issued key secret for gateways that require it.
async function provision(page: Page): Promise<string> {
  await provisionGenerationRoute(page, {
    providerName: provider,
    route,
    endpoint: upstream.endpoint,
    model: upstream.model,
    credential: upstream.credential
  });
  const key = await page.evaluate(async () => {
    const session = await fetch('/api/v1/sessions/current').then((r) =>
      r.json()
    );
    const response = await fetch('/api/v1/api-keys', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': session.csrf_token,
        'Idempotency-Key': crypto.randomUUID()
      },
      body: JSON.stringify({
        name: 'Pricing key',
        scopes: ['inference'],
        allowed_routes: []
      })
    });
    const body = await response.json();
    expect(response.status, JSON.stringify(body)).toBe(201);
    return body.secret as string;
  });
  await waitForRoutePublication(page, key, route);
  return key;
}

test.beforeEach(async ({ page }) => {
  await signIn(page);
});

test('publish a discount, a rate card revision, and a tiered, windowed price', async ({
  page
}, info) => {
  await provisionGenerationRoute(page, {
    providerName: provider,
    route,
    endpoint: upstream.endpoint,
    model: upstream.model,
    credential: upstream.credential
  });
  const providers = await manage<{ items: { id: string; name: string }[] }>(
    page,
    'GET',
    '/api/v1/providers'
  );
  const providerId = providers.body.items.find(
    (item) => item.name === provider
  )!.id;

  // A provider-scoped discount by name: the panel keeps the published rows
  // newest first and repeats the multiplier back.
  await page.goto('/settings');
  await expect(
    page.getByRole('heading', { name: 'Provider cost discounts' })
  ).toBeVisible();
  await page.getByLabel('Provider kind').selectOption('openai_compatible');
  await page.getByLabel('Provider ID').fill(providerId);
  await page.getByLabel('Multiplier').fill('0.8');
  await page.getByRole('button', { name: 'Publish discount' }).click();
  await expect(
    page.getByText('Discount published. New usage will price under it.')
  ).toBeVisible();
  await expect(page.getByRole('table', { name: 'Discounts' })).toContainText(
    '0.8'
  );

  // An installation rate card at a 50% markup, then a revision.
  await expect(
    page.getByRole('heading', { name: 'Consumer charge terms' })
  ).toBeVisible();
  await page.getByLabel('Owner kind').selectOption('installation');
  await page.getByRole('button', { name: 'Create card' }).click();
  await expect(
    page.getByText('Rate card created. Publish a revision to charge with it.')
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Publish revision', exact: true })
    .click();
  await page.getByLabel('Markup').fill('1.5');
  await page
    .getByRole('button', { name: 'Publish revision', exact: true })
    .last()
    .click();
  await expect(
    page.getByText('Revision published. New usage will charge under it.')
  ).toBeVisible();
  await expect(page.getByRole('table', { name: 'Rate cards' })).toContainText(
    'r1 ×1.5'
  );

  // A pricing revision with one tier and one window survives as revision 1.
  await expect(
    page.getByRole('heading', { name: 'Pricing revisions' })
  ).toBeVisible();
  await page.getByLabel('Provider kind').selectOption('openai_compatible');
  await page.getByLabel('Upstream model').fill(upstream.model);
  await page
    .getByLabel('Operation', { exact: true })
    .selectOption('generation');
  await page.getByLabel('Input / million', { exact: true }).fill(price.input);
  await page.getByLabel('Output / million', { exact: true }).fill(price.output);
  await page.getByRole('button', { name: 'Add tier' }).click();
  await page.getByLabel('Above input tokens').fill('1000');
  const windows = page.locator('[aria-label="Time windows"]');
  await page.getByRole('button', { name: 'Add window period' }).click();
  await windows.getByLabel('Window time zone').fill('America/New_York');
  await windows
    .getByRole('group', { name: 'Window 1 days' })
    .getByRole('button', { name: 'Mon' })
    .click();
  await windows.getByLabel('Start').fill('00:00');
  await windows.getByLabel('End').fill('06:00');
  await windows.getByLabel('Multiplier').fill('0.5');
  await page.getByRole('button', { name: 'Create pricing revision' }).click();
  await expect(
    page.getByText(
      'Pricing revision created. New usage will use the effective revision.'
    )
  ).toBeVisible();
  await expect(page.getByText('Revision 1', { exact: true })).toBeVisible();
  await page.screenshot({
    path: info.outputPath('pricing.png'),
    fullPage: true
  });

  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('the cost calculator bounds routes and quotes usage', async ({
  page
}, info) => {
  const secret = await provision(page);

  // Route mode: the published route's target reports min/max bounds.
  await page.goto('/usage');
  await expect(
    page.getByRole('heading', { name: 'Cost calculator' })
  ).toBeVisible();
  await page.getByLabel('Route').selectOption(route);
  await page.getByLabel('Input tokens').fill('1000');
  await page.getByLabel('Output token limit').fill('100');
  await page.getByRole('button', { name: 'Simulate' }).click();
  await expect(
    page.getByRole('table').filter({ hasText: upstream.model }).first()
  ).toContainText('USD');

  // Usage mode: the same provider's usage quotes cost and the 2x card's
  // charge.
  await page.getByRole('tab', { name: 'Usage' }).click();
  await page.getByLabel('Provider').selectOption({ label: provider });
  await page.getByLabel('Model').fill(upstream.model);
  await page.getByLabel('Operation').selectOption('generation');
  await page.locator('#calc-input').fill('1000');
  await page.locator('#calc-output').fill('100');
  await page.getByRole('button', { name: 'Add tool call' }).click();
  await page.locator('#calc-tool-name-0').fill('web_search');
  await page.locator('#calc-tool-count-0').fill('2');
  await page.getByRole('button', { name: 'Quote' }).click();
  const result = page.locator('.quote-result');
  await expect(result).toContainText('USD');
  await expect(result).toContainText('Charge');
  await page.screenshot({
    path: info.outputPath('calculator.png'),
    fullPage: true
  });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
