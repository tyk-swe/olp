import AxeBuilder from '@axe-core/playwright';
import { expect, type Page } from '../playwright';
import { manage } from '../helpers/management';
import { vertical, waitForRoutePublication } from './fixtures';

export async function verifyResponseCaching(page: Page) {
  const slug = 'response-cache-journey';
  const providers = await manage<{ items: { id: string; name: string }[] }>(
    page,
    'GET',
    '/api/v1/providers'
  );
  const provider = providers.body.items.find(
    (item) => item.name === vertical.providerName
  );
  expect(provider).toBeTruthy();
  const draft = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/route-drafts',
    {
      idempotency: crypto.randomUUID(),
      body: {
        slug,
        operations: ['generation'],
        fidelity: { mode: 'transformed' },
        overall_timeout_ms: 20000,
        max_attempts: 1,
        targets: [
          {
            provider_id: provider!.id,
            provider_model: vertical.deployment,
            priority: 0,
            weight: 1,
            timeout_ms: 15000
          }
        ]
      }
    }
  );
  expect(draft.status, draft.source).toBe(201);
  await page.goto(`/routes/${draft.body.id}`);
  await page
    .getByLabel('Response cache', { exact: true })
    .selectOption('exact');
  await expect(page.getByLabel('Sharing scope')).toHaveValue('key');
  await page.getByLabel('Lifetime (seconds)').fill('60');
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  await expect(page.getByText(/^Draft saved\./)).toBeVisible();
  await page
    .getByRole('button', { name: 'Activate route', exact: true })
    .click();
  await expect(
    page.getByText('Revision 1 active', { exact: true })
  ).toBeVisible();
  const key = await manage<{ secret: string }>(
    page,
    'POST',
    '/api/v1/api-keys',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: 'Response cache journey',
        scopes: ['inference', 'models_read'],
        allowed_routes: [slug]
      }
    }
  );
  expect(key.status, key.source).toBe(201);
  await waitForRoutePublication(page, key.body.secret, slug);
  async function infer() {
    return page.evaluate(
      async ({ slug, secret }) => {
        const response = await fetch('/v1/chat/completions', {
          method: 'POST',
          headers: {
            Authorization: `Bearer ${secret}`,
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            model: slug,
            messages: [{ role: 'user', content: 'Reply with OK.' }],
            max_tokens: 16
          })
        });
        return {
          status: response.status,
          cache: response.headers.get('x-olp-cache'),
          body: await response.text()
        };
      },
      { slug, secret: key.body.secret }
    );
  }
  const first = await infer();
  const second = await infer();
  expect(first.status, first.body).toBe(200);
  expect(first.cache).toBe('miss');
  expect(second.cache).toBe('hit');
  expect(second.body).toBe(first.body);
  await page.getByRole('button', { name: 'Purge cached responses' }).click();
  await expect(
    page.getByText('Cached responses for this route were invalidated.')
  ).toBeVisible();
  expect((await infer()).cache).toBe('miss');
  await page.getByRole('button', { name: 'Refresh cache statistics' }).click();
  await expect(page.getByText(/Last 30 days:/)).toBeVisible();
  const accessibility = await new AxeBuilder({ page })
    .include('.cache-editor')
    .withTags(['wcag2a', 'wcag2aa', 'wcag21aa'])
    .analyze();
  expect(accessibility.violations).toEqual([]);
  await page.screenshot({
    path: 'test-results/response-cache-journey.png',
    fullPage: true
  });
}
