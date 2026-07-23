import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

import { mockProviderKinds } from './provider-capabilities';

test('updates retention with ETag and creates an exact-decimal pricing revision', async ({ page }) => {
  await mockProviderKinds(page);
  const etag = '01980000-0000-7000-8000-000000000021';
  let retention = '30';
  let settingHeaders: Record<string, string> = {};
  let pricingHeaders: Record<string, string> = {};
  let pricingPayload: Record<string, unknown> = {};
  let revisions: unknown[] = [];
  await page.route('**/api/v1/sessions/current', async (route) => route.fulfill({ json: { user: { id: '01980000-0000-7000-8000-000000000001', email: 'owner@example.com', display_name: 'Ada Owner', role: 'owner' }, csrf_token: 'csrf-test-token' } }));
  await page.route('**/api/v1/settings', async (route) => route.fulfill({ json: { data: [{ key: 'request_retention_days', value: retention, etag, updated_by: '01980000-0000-7000-8000-000000000001', updated_at: '2026-07-12T12:00:00Z' }] } }));
  await page.route('**/api/v1/settings/request_retention_days', async (route) => {
    settingHeaders = route.request().headers();
    retention = (route.request().postDataJSON() as { value: string }).value;
    await route.fulfill({ json: { key: 'request_retention_days', value: retention, etag, updated_by: '01980000-0000-7000-8000-000000000001', updated_at: '2026-07-12T12:01:00Z' } });
  });
  await page.route('**/api/v1/pricing/revisions', async (route) => {
    if (route.request().method() === 'POST') {
      pricingHeaders = route.request().headers();
      pricingPayload = route.request().postDataJSON() as Record<string, unknown>;
      revisions = [{ id: '01980000-0000-7000-8000-000000000099', revision: 1, effective_at: '2026-07-12T12:00:00Z', created_by: '01980000-0000-7000-8000-000000000001', created_at: '2026-07-12T12:00:00Z', prices: (pricingPayload.prices as unknown[]) }];
      await route.fulfill({ status: 201, json: revisions[0] });
    } else await route.fulfill({ json: { data: revisions } });
  });

  await page.goto('/settings');
  await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible();
  await page.getByLabel('Request Retention Days').fill('45');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await expect(page.getByText('Request Retention Days saved.')).toBeVisible();
  expect(settingHeaders['if-match']).toBe(`"${etag}"`);
  expect(settingHeaders['x-csrf-token']).toBe('csrf-test-token');

  await page.getByLabel('Upstream model').fill('gpt-test');
  await page.getByLabel('Input / million').fill('2.500000');
  await page.getByLabel('Output / million').fill('10.125000');
  await page.getByRole('button', { name: 'Create pricing revision' }).click();
  await expect(page.getByText('Pricing revision created.')).toBeVisible();
  expect(pricingHeaders['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  expect(pricingPayload).toMatchObject({ prices: [{ model: 'gpt-test', input_per_million: '2.500000', output_per_million: '10.125000', unit_price: null, currency: 'USD' }] });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
