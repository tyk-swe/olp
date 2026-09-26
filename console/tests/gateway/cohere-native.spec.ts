import { expect, test } from '../playwright';
import { signInGatewayOwner } from './signIn';

test('Cohere native playground shows typed storage groups and exact result bytes', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  await page.route('**/api/v1/routes?*', async (route) => {
    await route.fulfill({
      headers: { 'cache-control': 'no-store' },
      json: {
        items: [
          {
            id: '00000000-0000-4000-8000-000000000001',
            slug: 'cohere-route',
            latest_revision: {
              fidelity: { mode: 'strict' },
              operations: ['embeddings', 'rerank']
            }
          }
        ],
        next_cursor: null
      }
    });
  });
  const raw =
    '{"id":"embed-native","embeddings":{"float":[[0.10000000000000001,-0,1,2,3,4,5,6]],"int8":[[-128,127,0,1,2,3,4,5]],"ubinary":[[128]]},"meta":{"billed_units":{"input_tokens":2}},"opaque":{"counter":9007199254740993}}';
  await page.route(
    '**/native/cohere-embed-v2/models/cohere-route',
    async (route) => {
      const sent = route.request().postData() ?? '';
      expect(sent).toContain('"input_type":"search_document"');
      expect(sent).toContain('"embedding_types":["float","int8","ubinary"]');
      expect(sent).toContain('"model":"cohere-route"');
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        headers: { 'cache-control': 'no-store' },
        body: raw
      });
    }
  );
  await page.goto('/playground');
  await page.getByRole('radio', { name: 'Advanced' }).check();
  await page.getByLabel('Route slug').fill('cohere-route');
  await page.getByLabel('Template').selectOption('cohere-typed-embeddings');
  await expect(page.getByLabel('Registered native dialect')).toHaveValue(
    'cohere-embed-v2'
  );
  await expect(page.getByLabel('Request JSON')).toHaveValue(/"input_type"/);
  await page.getByLabel('Inference API key').fill('olp_browser_fixture');
  await page.getByRole('button', { name: 'Run billable operation' }).click();
  const result = page.locator('.operation-result');
  await expect(result.getByText('Dense array · float')).toBeVisible();
  await expect(result.getByText('Dense array · int8')).toBeVisible();
  await expect(result.getByText('Packed binary · ubinary')).toBeVisible();
  await expect(result.getByText('1 stored byte')).toBeVisible();
  await result.locator('summary').click();
  await expect(
    result.locator('[data-testid="native-operation-result"]')
  ).toContainText('9007199254740993');
  await result.screenshot({
    path: info.outputPath('console-cohere-native.png')
  });

  await page.route(
    '**/native/cohere-rerank-v2/models/cohere-route',
    async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        headers: { 'cache-control': 'no-store' },
        body: '{"results":[{"index":1,"relevance_score":0.1000000000000000000001},{"index":0,"relevance_score":0.1000000000000000000001}]}'
      });
    }
  );
  await page.getByLabel('Template').selectOption('cohere-rerank-v2');
  await expect(page.getByLabel('Registered native dialect')).toHaveValue(
    'cohere-rerank-v2'
  );
  await page.getByLabel('Inference API key').fill('olp_browser_fixture');
  await page.getByRole('button', { name: 'Run billable operation' }).click();
  await expect(
    result.locator('td code').filter({ hasText: '0.1000000000000000000001' })
  ).toHaveCount(2);
  await expect(result.locator('tbody tr').first()).toContainText('1');
});
