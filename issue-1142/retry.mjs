import { createRequire } from 'node:module';
import assert from 'node:assert/strict';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(resolve('console/package.json'));
const { chromium } = require('@playwright/test');
const phase = process.argv[2];
assert(['before', 'after'].includes(phase));
const id = `vector_store_${'1'.repeat(32)}`;
const body = '{"expires_after":{"anchor":"last_active_at","days":1}}';
const url = 'http://127.0.0.1:43142/provider-resources';
const root = '/v1/vector_stores';
const actions = ['get', 'update'].map((action) => ({
  operation: 'vector_store',
  surface: 'openai',
  kind: 'vector_store',
  method: action === 'get' ? 'GET' : 'POST',
  pattern: `${root}/{id}`,
  action,
  metered: action === 'update'
}));
const browser = await chromium.launch();
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const submissions = new Set();
  const replies = [];
  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname.slice('/api/v1/'.length);
    const fixtures = {
      'sessions/current': {
        csrf_token: 'fixture-csrf',
        operations: ['read'],
        user: {
          id: 'fixture-owner',
          email: 'owner@example.test',
          display_name: 'Owner',
          role: 'owner',
          access_scope: 'global'
        }
      },
      'auth/capabilities': {
        local_login_enabled: true,
        oidc_login_enabled: false
      },
      'provider-resource-contracts': { endpoints: actions }
    };
    await route.fulfill({ json: fixtures[path] ?? { items: [] } });
  });
  await page.route(`**${root}/${id}`, async (route) => {
    const request = route.request();
    const key = await request.headerValue('Idempotency-Key');
    assert.ok(key);
    assert.equal(request.method(), 'POST');
    assert.equal(request.postData(), body);
    const first = submissions.size === 0;
    const replay = submissions.has(key);
    submissions.add(key);
    const status = first ? 400 : replay ? 409 : 200;
    replies.push({ status, replay });
    await route.fulfill({
      status,
      ...(first && phase === 'after'
        ? { headers: { 'X-OLP-Mutation-Status': 'not_accepted' } }
        : {}),
      json: first
        ? { error: { message: 'The provider could not complete this resource operation.', code: 'provider_resource_error' } }
        : replay
          ? { error: { message: 'This mutation identity already exists or conflicts with its original request.', code: 'idempotency_conflict' } }
          : { id, status: 'completed', expires_after: { anchor: 'last_active_at', days: 1 } }
    });
  });
  await page.goto(url);
  const panel = page.getByRole('region', { name: 'Native resource lifecycle' });
  await panel.getByLabel('Resource family').selectOption('vector_store');
  await panel.getByLabel(/^Action/).selectOption(`POST ${root}/{id}`);
  await panel.getByLabel('Inference API key').fill('fixture-key');
  await panel.getByLabel('vector store ID').fill(id);
  await panel.getByLabel('Request body (native JSON)').fill(body);
  await panel.getByRole('button', { name: 'Update resource' }).click();
  await panel.getByText('Resource request failed (HTTP 400)', { exact: false }).waitFor();
  await panel.getByRole('button', { name: 'Update resource' }).click();
  if (phase === 'before') {
    await panel.getByText('Resource request failed (HTTP 409)', { exact: false }).waitFor();
    assert.deepEqual(replies, [{ status: 400, replay: false }, { status: 409, replay: true }]);
  } else {
    await panel.getByLabel('Native resource result').getByText('"status":"completed"', { exact: false }).waitFor();
    assert.deepEqual(replies, [{ status: 400, replay: false }, { status: 200, replay: false }]);
  }
  await panel.screenshot({
    path: fileURLToPath(new URL(`${phase}.png`, import.meta.url))
  });
  console.log(`${phase}: ${replies.map(({ status, replay }) => `${status} (${replay ? 'same' : 'new'} key)`).join(', ')}`);
} finally {
  await browser.close();
}
