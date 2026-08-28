import AxeBuilder from '@axe-core/playwright';
import { expect, mockSession, test } from '../playwright';

// The filters convert local wall time to instants, so the browser's zone has to
// be fixed for the query the console sends to be a literal.
test.use({ timezoneId: 'UTC' });

const jobId = '01980000-0000-7000-8000-000000000201';
const keyId = '01980000-0000-7000-8000-000000000202';
const providerId = '01980000-0000-7000-8000-000000000203';

const job = {
  id: jobId,
  api_key_id: keyId,
  provider_id: providerId,
  provider_name: 'Primary OpenAI',
  provider_model: 'sora-test',
  route: 'video-render',
  operation: 'video_create',
  surface: 'openai',
  state: 'running',
  lifecycle: 'active',
  progress_percent: 42,
  content_available: false,
  etag: '01980000-0000-7000-8000-000000000204',
  upstream_job_id: 'vid_test',
  error_class: null,
  reconciliation_error: null,
  created_at: '2026-07-12T11:00:00Z',
  completed_at: null,
  last_polled_at: '2026-07-12T11:59:00Z',
  expires_at: '2026-07-19T11:00:00Z',
  deleted_at: null,
  updated_at: '2026-07-12T12:00:00Z'
};

test('media jobs list the working timestamps and filter by key, provider, and creation window', async ({
  page
}) => {
  await mockSession(page);
  let query = new URLSearchParams();
  await page.route(
    /\/api\/v1\/media-jobs(?:\/[^?]+)?(?:\?.*)?$/,
    async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith(`/media-jobs/${jobId}`)) {
        await route.fulfill({ json: job });
        return;
      }
      query = url.searchParams;
      await route.fulfill({ json: { items: [job], next_cursor: null } });
    }
  );

  await page.goto('/media-jobs');
  await expect(page.getByRole('heading', { name: 'Media Jobs' })).toBeVisible();

  const row = page.getByRole('row').filter({ hasText: 'video-render' });
  // The list answers "what is happening now"; the retention and polling clocks
  // belong to one job and live on its detail panel.
  await expect(
    page.getByRole('columnheader', { name: 'Created' })
  ).toBeVisible();
  await expect(
    page.getByRole('columnheader', { name: 'Updated' })
  ).toBeVisible();
  await expect(page.getByRole('columnheader', { name: 'Expires' })).toHaveCount(
    0
  );
  await expect(
    page.getByRole('columnheader', { name: 'Last polled' })
  ).toHaveCount(0);
  await expect(row).toContainText(providerId);
  await expect(row).not.toContainText('Not finished');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.getByLabel('API key ID').fill(keyId);
  await page.getByLabel('Provider ID').fill(providerId);
  await page.getByLabel('Created after').fill('2026-07-12T09:30');
  await page.getByLabel('Created before').fill('2026-07-12T18:00');
  await page.getByLabel('State').selectOption('running');
  await page.getByRole('button', { name: 'Apply filters' }).click();

  await expect.poll(() => query.get('api_key_id')).toBe(keyId);
  expect(query.get('provider_id')).toBe(providerId);
  expect(query.get('state')).toBe('running');
  expect(query.get('created_after')).toBe('2026-07-12T09:30:00.000Z');
  expect(query.get('created_before')).toBe('2026-07-12T18:00:00.000Z');

  await page.getByRole('button', { name: 'Clear' }).click();
  await expect(page.getByLabel('API key ID')).toHaveValue('');
});

test('the media job detail panel keeps the retention and polling clocks', async ({
  page
}) => {
  await mockSession(page);
  await page.route(
    /\/api\/v1\/media-jobs(?:\/[^?]+)?(?:\?.*)?$/,
    async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith(`/media-jobs/${jobId}`))
        await route.fulfill({ json: job });
      else await route.fulfill({ json: { items: [job], next_cursor: null } });
    }
  );

  await page.goto(`/media-jobs/${jobId}`);
  const facts = page.getByRole('region', { name: 'video-render' });
  // Retention was invisible before: an operator could not see when the record
  // or the upstream content goes away.
  await expect(
    facts.getByText('Expires', { exact: true }).locator('..')
  ).toContainText('Jul 19, 2026');
  await expect(
    facts.getByText('Completed', { exact: true }).locator('..')
  ).toContainText('Not finished');
  await expect(
    facts.getByText('Last polled', { exact: true }).locator('..')
  ).toContainText('Jul 12, 2026');
  await expect(
    facts.getByText('Deleted', { exact: true }).locator('..')
  ).toContainText('Not deleted');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
