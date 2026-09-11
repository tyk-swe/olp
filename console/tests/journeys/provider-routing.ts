import { expect, type Page, type TestInfo } from '../playwright';
import { vertical } from './fixtures';

export async function verifyProviderRouting(page: Page, info: TestInfo) {
  await page.goto('/providers');
  await page
    .getByRole('link', { name: 'Concurrent provider name', exact: true })
    .click();
  await page.getByRole('link', { name: 'Duplicate connection' }).click();
  await expect(page.getByLabel('Credential', { exact: true })).toHaveValue('');
  await page.getByLabel('Vendor', { exact: true }).selectOption('azure');
  await page.getByLabel('Provider name').fill('Routing pool connection');
  await page.getByLabel('Seed model (optional)').fill(vertical.deployment);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(vertical.credential);
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  const bulk = page.locator('section').filter({
    has: page.getByRole('heading', { name: 'Validate models in bulk' })
  });
  await bulk.getByRole('checkbox').check();
  await bulk.getByLabel('Capabilities to validate').selectOption('generation');
  await bulk
    .getByRole('button', { name: 'Validate 1 selected models' })
    .click();
  await expect(bulk.getByText(/Checked 1 models/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await page.getByRole('link', { name: 'View provider', exact: true }).click();
  await expect(page).toHaveURL(/\/providers\/[0-9a-f-]{36}$/);
  const providerId = new URL(page.url()).pathname.split('/').pop()!;

  const pool = page.locator('section').filter({
    has: page.getByRole('heading', { name: 'Credential pool', exact: true })
  });
  await pool
    .getByRole('button', { name: 'Add credential', exact: true })
    .click();
  await pool.getByLabel('Name', { exact: true }).fill('Standby');
  await pool.getByLabel('Priority', { exact: true }).fill('1');
  await pool
    .getByLabel('Credential', { exact: true })
    .fill(vertical.credential);
  await pool.getByLabel('Allowed models').fill(vertical.deployment);
  await pool.getByLabel('Requests / minute').fill('1000');
  await pool.getByLabel('Concurrent requests').fill('2');
  await pool
    .getByRole('button', { name: 'Save credential', exact: true })
    .click();
  const standby = pool
    .getByRole('listitem')
    .filter({ has: page.getByText('Standby', { exact: true }) });
  await standby.getByRole('button', { name: 'Validate access' }).click();
  await expect(
    pool.getByText('Standby: model access validated.')
  ).toBeVisible();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(
    page.getByRole('button', { name: 'Activate changes' })
  ).toBeEnabled();
  await page.getByRole('button', { name: 'Activate changes' }).click();
  await expect(page.getByText(/Activated in runtime generation/)).toBeVisible();
  await expect(
    page.getByText(vertical.credential, { exact: true })
  ).toHaveCount(0);
  await pool.screenshot({
    path: info.outputPath('provider-credential-pool.png')
  });

  await page.goto('/models');
  await page
    .getByText('Compare models and create routes', { exact: true })
    .click();
  const comparison = page.locator('details.bulk-routes');
  await comparison
    .getByRole('checkbox', { name: /Routing pool connection/ })
    .check();
  await comparison
    .getByRole('checkbox', { name: /Concurrent provider name/ })
    .check();
  const names = comparison.getByRole('textbox');
  await expect(names).toHaveCount(2);
  await names.nth(0).fill('provider-pool-route');
  await names.nth(1).fill('provider-pool-route');
  await comparison.screenshot({
    path: info.outputPath('provider-model-comparison.png')
  });
  await comparison
    .getByRole('button', { name: 'Create reviewed route drafts' })
    .click();
  await comparison
    .getByRole('link', { name: 'provider-pool-route', exact: true })
    .click();
  await page.getByLabel('Maximum attempts').fill('3');
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  const policy = page.locator('section').filter({
    has: page.getByRole('heading', {
      name: 'Provider routing policy',
      exact: true
    })
  });
  await policy
    .getByLabel('Only these vendors or connections')
    .first()
    .fill(`provider:${providerId}`);
  await policy.getByRole('button', { name: 'Save routing policy' }).click();
  await expect(policy.getByText(/Routing policy staged/)).toBeVisible();
  await page.getByRole('button', { name: 'Simulate order' }).click();
  const explanation = page.getByLabel('Routing explanation');
  await expect(
    explanation.getByText('provider not allowed', { exact: true })
  ).toBeVisible();
  await expect(explanation.getByText(/^Attempt 2/)).toBeVisible();
  await explanation.screenshot({
    path: info.outputPath('provider-routing-preview.png')
  });
  await page
    .getByRole('button', { name: 'Activate route', exact: true })
    .click();
  await expect(
    page.getByText('Revision 1 active', { exact: true })
  ).toBeVisible();

  await page.goto('/playground');
  await page
    .getByLabel('Route slug', { exact: true })
    .fill('provider-pool-route');
  await page
    .getByLabel('Prompt', { exact: true })
    .fill('Confirm provider pool routing.');
  await page.getByLabel('Max output tokens', { exact: true }).fill('16');
  await page.locator('#playground-routing-strategy').selectOption('weighted');
  await expect(async () => {
    await page.getByRole('button', { name: 'Run test', exact: true }).click();
    await expect(page.getByText(vertical.reply, { exact: true })).toBeVisible();
  }).toPass({ timeout: 30000, intervals: [500, 1000] });
  await expect(
    page.getByRole('heading', { name: 'Routing explanation', exact: true })
  ).toBeVisible();
  await page
    .getByLabel('Routing explanation')
    .screenshot({ path: info.outputPath('provider-playground-routing.png') });
  // A disabled default key must not prevent using an independently validated slot.
  await page.goto(`/providers/${providerId}`);
  const defaultSlot = pool
    .getByRole('listitem')
    .filter({ has: page.getByText('Default', { exact: true }) });
  await defaultSlot.getByRole('button', { name: 'Edit / rotate' }).click();
  await pool
    .getByLabel('Credential', { exact: true })
    .fill('invalid-provider-secret');
  await pool.getByLabel('Enabled', { exact: true }).uncheck();
  await pool
    .getByRole('button', { name: 'Save credential', exact: true })
    .click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(
    page.getByRole('button', { name: 'Activate changes' })
  ).toBeEnabled();
  await page.getByRole('button', { name: 'Activate changes' }).click();
  await expect(page.getByText(/Activated in runtime generation/)).toBeVisible();
  await page.goto('/playground');
  await page
    .getByLabel('Route slug', { exact: true })
    .fill('provider-pool-route');
  await page
    .getByLabel('Prompt', { exact: true })
    .fill('Use the enabled credential.');
  await page.getByLabel('Max output tokens', { exact: true }).fill('16');
  await expect(async () => {
    await page.getByRole('button', { name: 'Run test', exact: true }).click();
    await expect(page.getByText(vertical.reply, { exact: true })).toBeVisible();
    await expect(
      page.getByLabel('Routing explanation').getByText(/^Attempt 2/)
    ).toHaveCount(0);
  }).toPass({ timeout: 30000, intervals: [500, 1000] });
}
