import AxeBuilder from '@axe-core/playwright';
import { manage } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import { expect, test } from '../playwright';

// The mock OpenAI upstream on :4187 serves one invoiced day (yesterday) at
// /v1/organization/costs, so the real billing importer worker must land it.
const endpoint = 'http://127.0.0.1:4187/v1';

function utcDay(offsetDays: number): string {
  const day = new Date();
  day.setUTCDate(day.getUTCDate() + offsetDays);
  day.setUTCHours(0, 0, 0, 0);
  return day.toISOString().slice(0, 10);
}

test('billing import reconciles the vendor invoice with usage', async ({
  page
}) => {
  // The billing importer pass runs once a minute; leave generous headroom.
  test.setTimeout(300_000);
  await signIn(page);

  const provider = await manage(page, 'POST', '/api/v1/providers', {
    idempotency: crypto.randomUUID(),
    body: {
      name: `Reconciliation OpenAI ${Date.now().toString(36)}`,
      configuration: {
        kind: 'openai',
        auth_mode: 'api_key',
        endpoint
      },
      credential: 'sk-provider-e2e-credential',
      model: 'gpt-test'
    }
  });
  expect(provider.status).toBe(201);
  const providerId = (provider.body as { id: string }).id;

  // Configure the import through the UI and trigger an immediate run.
  await page.goto(`/providers/${providerId}`);
  const section = page.locator('.billing-import');
  await expect(
    section.getByRole('heading', { name: 'Billing import' })
  ).toBeVisible();
  await section.locator('#billing-admin-key').fill('sk-admin-e2e-admin-key');
  await section.getByRole('button', { name: 'Create import' }).click();
  await expect(section.getByText('Billing import saved.')).toBeVisible();
  await section.getByRole('button', { name: 'Run now' }).click();
  await expect(section.getByText('Import scheduled.')).toBeVisible();

  // The importer's pass polls every minute; poll the API for the first
  // success rather than the always-visible status label.
  await expect(async () => {
    const status = await manage(
      page,
      'GET',
      `/api/v1/providers/${providerId}/billing-import`
    );
    expect(status.status).toBe(200);
    const body = status.body as {
      status: {
        last_success_at: string | null;
        last_error_code: string | null;
      };
    };
    expect(body.status.last_error_code).toBeNull();
    expect(body.status.last_success_at).not.toBeNull();
  }).toPass({ timeout: 180_000, intervals: [10_000, 20_000] });

  // The reconciliation panel on Usage shows the invoiced bucket.
  const start = utcDay(-2);
  const end = utcDay(1);
  await page.goto(
    `/usage?start=${start}T00%3A00%3A00Z&end=${end}T00%3A00%3A00Z`
  );
  const recon = page.locator('.reconciliation-panel');
  await expect(recon).toBeVisible();
  await expect(recon.getByText('$12.34').first()).toBeVisible();
  // No usage was recorded, so the capture cell is a zero rate.
  await expect(recon.getByRole('cell', { name: '0.0%' }).first()).toBeVisible();

  const axe = await new AxeBuilder({ page })
    .include('.reconciliation-panel')
    .analyze();
  expect(axe.violations).toEqual([]);
});
