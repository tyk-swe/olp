import { manage, provisionGenerationRoute } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
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
  const discounts = page.getByRole('region', {
    name: 'Provider cost discounts'
  });
  await expect(
    discounts.getByRole('heading', { name: 'Provider cost discounts' })
  ).toBeVisible();
  await discounts.getByLabel('Provider kind').selectOption('openai_compatible');
  await discounts.getByLabel('Provider ID').fill(providerId);
  await discounts.getByLabel('Multiplier').fill('0.8');
  // The default effective time is the form's minute; a later minute still
  // proves the discount without racing the "now or future" check.
  await discounts
    .getByLabel('Effective at')
    .fill(new Date(Date.now() + 120_000).toISOString().slice(0, 16));
  await discounts.getByRole('button', { name: 'Publish discount' }).click();
  await expect(
    page.getByText('Discount published. New usage will price under it.')
  ).toBeVisible();
  await expect(page.getByRole('table', { name: 'Discounts' })).toContainText(
    '0.8'
  );

  // An installation rate card at a 50% markup, then a revision.
  const cards = page.getByRole('region', { name: 'Consumer charge terms' });
  await expect(
    cards.getByRole('heading', { name: 'Consumer charge terms' })
  ).toBeVisible();
  await cards.getByLabel('Owner kind').selectOption('installation');
  await cards.getByRole('button', { name: 'Create card' }).click();
  await expect(
    page.getByText('Rate card created. Publish a revision to charge with it.')
  ).toBeVisible();
  await cards
    .getByRole('button', { name: 'Publish revision', exact: true })
    .click();
  await cards.getByLabel('Markup').fill('1.5');
  await cards
    .getByLabel('Effective at')
    .fill(new Date(Date.now() + 120_000).toISOString().slice(0, 16));
  await cards
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
  const revisions = page.getByRole('region', { name: 'Pricing revisions' });
  await expect(
    revisions.getByRole('heading', { name: 'Pricing revisions' })
  ).toBeVisible();
  await revisions.getByLabel('Provider kind').selectOption('openai_compatible');
  await revisions.getByLabel('Upstream model').fill(upstream.model);
  await revisions
    .getByLabel('Operation', { exact: true })
    .selectOption('generation');
  await revisions
    .getByLabel('Input / million', { exact: true })
    .fill(price.input);
  await revisions
    .getByLabel('Output / million', { exact: true })
    .fill(price.output);
  await revisions.getByRole('button', { name: 'Add tier' }).click();
  await revisions.getByLabel('Above input tokens').fill('1000');
  const tierRows = revisions.locator('[aria-label="Input-token tiers"]');
  await tierRows.getByTitle('input per million').fill(price.input);
  await tierRows.getByTitle('output per million').fill(price.output);
  const windows = revisions.locator('[aria-label="Time windows"]');
  await revisions.getByRole('button', { name: 'Add window period' }).click();
  await windows.getByLabel('Window time zone').fill('America/New_York');
  await windows
    .getByRole('group', { name: 'Window 1 days' })
    .getByRole('button', { name: 'Mon' })
    .click();
  await windows.getByLabel('Start').fill('00:00');
  await windows.getByLabel('End').fill('06:00');
  await windows.getByLabel('Multiplier').fill('0.5');
  await revisions
    .getByRole('button', { name: 'Create pricing revision' })
    .click();
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
  // The first journey's priced provider and route are still in force.
  const names = { provider, route };

  // Route mode: the published route's target reports min/max bounds.
  await page.goto('/usage');
  const calculator = page.getByRole('region', { name: 'Cost calculator' });
  await expect(
    calculator.getByRole('heading', { name: 'Cost calculator' })
  ).toBeVisible();
  await calculator.getByLabel('Route').selectOption(names.route);
  await calculator.getByLabel('Input tokens').fill('1000');
  await calculator.getByLabel('Output token limit').fill('100');
  // Routing inputs refresh on an interval, so a price published moments ago
  // may take a few seconds to bound the target.
  await expect
    .poll(
      async () => {
        await calculator.getByRole('button', { name: 'Simulate' }).click();
        return await page
          .getByRole('table')
          .filter({ hasText: upstream.model })
          .first()
          .textContent();
      },
      { timeout: 30_000 }
    )
    .toContain('USD');

  // Usage mode: the same provider's usage quotes list cost, cost and the
  // installation card's charge.
  await calculator.getByRole('tab', { name: 'Usage' }).click();
  await calculator
    .getByLabel('Provider')
    .selectOption({ label: names.provider });
  await calculator.getByLabel('Model').fill(upstream.model);
  await calculator.getByLabel('Operation').selectOption('generation');
  await calculator.locator('#calc-input').fill('1000');
  await calculator.locator('#calc-output').fill('100');
  await calculator.getByText('More usage').click();
  await calculator.getByRole('button', { name: 'Add tool call' }).click();
  await calculator.locator('#calc-tool-name-0').fill('web_search');
  await calculator.locator('#calc-tool-count-0').fill('2');
  await calculator.getByRole('button', { name: 'Quote' }).click();
  const result = page.locator('.quote-result');
  await expect(result).toContainText('USD');
  await expect(result).toContainText('Charge');
  await page.screenshot({
    path: info.outputPath('calculator.png'),
    fullPage: true
  });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
