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

test('the media job detail panel refreshes progress and polling clocks', async ({
  page
}) => {
  await mockSession(page);
  let detailRequests = 0;
  const refreshResponse = Promise.withResolvers<void>();
  const refreshedJob = {
    ...job,
    progress_percent: 84,
    last_polled_at: '2026-07-13T11:59:00Z',
    updated_at: '2026-07-13T12:00:00Z'
  };
  await page.route(
    /\/api\/v1\/media-jobs(?:\/[^?]+)?(?:\?.*)?$/,
    async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith(`/media-jobs/${jobId}`)) {
        detailRequests += 1;
        if (detailRequests > 1) await refreshResponse.promise;
        await route.fulfill({
          json: detailRequests === 1 ? job : refreshedJob
        });
      } else await route.fulfill({ json: { items: [job], next_cursor: null } });
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
  await expect(
    facts.getByText('Progress', { exact: true }).locator('..')
  ).toContainText('42%');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  const refresh = page.getByRole('button', { name: 'Refresh', exact: true });
  await expect(refresh).toBeEnabled();
  await refresh.click();
  await expect(refresh).toBeDisabled();
  refreshResponse.resolve();
  await expect(refresh).toBeEnabled();
  await expect(
    facts.getByText('Progress', { exact: true }).locator('..')
  ).toContainText('84%');
  await expect(
    facts.getByText('Last polled', { exact: true }).locator('..')
  ).toContainText('Jul 13, 2026');
  await expect(
    facts.getByText('Updated', { exact: true }).locator('..')
  ).toContainText('Jul 13, 2026');
  await page.getByRole('link', { name: 'All media jobs' }).click();
  await expect(page.getByRole('heading', { name: 'Media Jobs' })).toBeVisible();
});

test.describe('media-job investigation URLs', () => {
  test.use({ timezoneId: 'America/New_York' });

  test('direct links retain precise applied filters across edits, reload and browser history', async ({
    page
  }) => {
    await mockSession(page);
    const requests: URLSearchParams[] = [];
    await page.route('**/api/v1/media-jobs?*', async (route) => {
      requests.push(new URL(route.request().url()).searchParams);
      await route.fulfill({ json: { items: [job], next_cursor: null } });
    });
    const filters = {
      route: 'video-render',
      state: 'running',
      lifecycle: 'active',
      api_key_id: keyId,
      provider_id: providerId,
      created_after: '2026-11-01T05:50:00.123456Z',
      created_before: '2026-11-01T06:10:00.456789Z'
    };
    await page.goto(`/media-jobs?${new URLSearchParams(filters)}`);
    await expect(
      page.getByRole('row').filter({ hasText: 'video-render' })
    ).toBeVisible();
    await expect(page.getByLabel('Created after')).toHaveValue(
      '2026-11-01T01:50'
    );
    await expect(page.getByLabel('Created before')).toHaveValue(
      '2026-11-01T01:10'
    );
    const count = requests.length;
    await page.getByLabel('Route').fill('changed-route');
    expect(requests).toHaveLength(count);
    expect(new URL(page.url()).searchParams.get('route')).toBe('video-render');
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await expect.poll(() => requests.length).toBe(count + 1);
    expect(Object.fromEntries(requests.at(-1)!)).toMatchObject(filters);
    await page.getByRole('button', { name: 'Apply filters' }).click();
    await expect
      .poll(() => new URL(page.url()).searchParams.get('route'))
      .toBe('changed-route');
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(new URL(page.url()).searchParams.get('created_after')).toBe(
      filters.created_after
    );
    expect(new URL(page.url()).searchParams.get('created_before')).toBe(
      filters.created_before
    );
    await page.reload();
    await expect(page.getByLabel('Route')).toHaveValue('changed-route');
    await page.goBack();
    await expect(page.getByLabel('Route')).toHaveValue('video-render');
    await page.goForward();
    await expect(page.getByLabel('Route')).toHaveValue('changed-route');
    await page.getByRole('button', { name: 'Clear' }).click();
    await expect.poll(() => new URL(page.url()).search).toBe('');
    await expect(page.getByLabel('State')).toHaveValue('');
    await expect.poll(() => requests.at(-1)?.has('state')).toBe(false);
  });

  for (const [parameter, value, label] of [
    ['state', 'unknown-state', 'State'],
    ['lifecycle', 'unknown-lifecycle', 'Lifecycle'],
    ['provider_id', 'not-a-uuid', 'Provider ID'],
    ['created_after', '2026-02-30T12:00:00Z', 'Created after']
  ]) {
    test(`invalid ${parameter} is visible and does not fetch`, async ({
      page
    }) => {
      await mockSession(page);
      let requests = 0;
      let lastQuery = new URLSearchParams();
      await page.route('**/api/v1/media-jobs?*', async (route) => {
        requests += 1;
        lastQuery = new URL(route.request().url()).searchParams;
        await route.fulfill({ json: { items: [], next_cursor: null } });
      });
      await page.goto(
        `/media-jobs?${new URLSearchParams({ [parameter]: value })}`
      );
      await expect(page.getByRole('alert')).toContainText(value);
      await expect(page.getByLabel(label)).toHaveValue(value);
      await expect(
        page.getByRole('button', { name: 'Refresh', exact: true })
      ).toBeDisabled();
      expect(requests).toBe(0);
      if (parameter === 'created_after') {
        const corrected = '2026-02-28T12:00:00Z';
        await page.getByLabel(label).fill(corrected);
        await expect(page.getByLabel(label)).toHaveValue(corrected);
        await page.getByRole('button', { name: 'Apply filters' }).click();
        await expect.poll(() => requests).toBe(1);
        await expect(page.getByRole('alert')).toHaveCount(0);
        expect(new URL(page.url()).searchParams.get(parameter)).toBe(corrected);
        expect(lastQuery.get(parameter)).toBe(corrected);
        return;
      }
      await page.getByRole('button', { name: 'Clear' }).click();
      await expect.poll(() => requests).toBe(1);
    });
  }
});
