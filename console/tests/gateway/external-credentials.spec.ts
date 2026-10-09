import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from './signIn';

test('external credentials create and rotate through the operator console', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  await page.goto('/providers/new');
  await page.locator('input[name="kind"][value="openai_compatible"]').check();
  await page
    .getByLabel('Provider name', { exact: true })
    .fill('Vault browser provider');
  await page
    .getByLabel('Endpoint', { exact: true })
    .fill('http://127.0.0.1:4187/v1');
  await page
    .getByLabel('Use an external credential version', { exact: true })
    .check();
  const reference = {
    store: 'vault',
    secret_id: 'http://127.0.0.1:4199/v1/secret/data/provider',
    version: '1',
    field: 'api_key'
  };
  await page
    .getByLabel('Pinned external credential reference', { exact: true })
    .fill(JSON.stringify(reference));
  const creation = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === '/api/v1/providers'
  );
  await page.getByRole('button', { name: 'Save and test connection' }).click();
  const response = await creation;
  expect(response.status(), await response.text()).toBe(201);
  const provider = await response.json();
  expect(JSON.parse(response.request().postData()!)).toMatchObject({
    credential_reference: reference
  });
  expect(response.request().postData()).not.toContain(
    'compatible-provider-secret'
  );
  await page.goto(`/providers/${provider.id}`);
  await page
    .getByLabel('Use an external credential version', { exact: true })
    .first()
    .check();
  await page
    .getByLabel('Pinned external credential reference', { exact: true })
    .fill(JSON.stringify({ ...reference, version: '2' }));
  await page
    .getByRole('button', { name: 'Stage rotation', exact: true })
    .click();
  await expect(
    page.getByText(
      'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.'
    )
  ).toBeVisible();
  await expect(page.getByText('vault · pinned store version 2')).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({ path: info.outputPath('external-credentials.png') });
});
