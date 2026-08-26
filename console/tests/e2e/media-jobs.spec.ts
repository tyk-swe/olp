import AxeBuilder from '@axe-core/playwright';
import { expect, mockSession, test } from '../playwright';

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

test('media jobs expose retention timestamps and filter by key, provider, and creation window', async ({ page }) => {
  await mockSession(page);
  let query = new URLSearchParams();
  await page.route('**/api/v1/media-jobs*', async (route) => {
    query = new URL(route.request().url()).searchParams;
    await route.fulfill({ json: { data: [job], next_cursor: null } });
  });

  await page.goto('/media-jobs');
  await expect(page.getByRole('heading', { name: 'Media Jobs' })).toBeVisible();

  const row = page.getByRole('row').filter({ hasText: 'video-render' });
  // Retention was invisible before: an operator could not see when the record
  // or the upstream content goes away.
  await expect(page.getByRole('columnheader', { name: 'Expires' })).toBeVisible();
  await expect(row).toContainText(providerId);
  await expect(row).toContainText('Not finished');
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
  expect(query.get('created_after')).toBe(new Date('2026-07-12T09:30').toISOString());
  expect(query.get('created_before')).toBe(new Date('2026-07-12T18:00').toISOString());

  await page.getByRole('button', { name: 'Clear' }).click();
  await expect(page.getByLabel('API key ID')).toHaveValue('');
});
