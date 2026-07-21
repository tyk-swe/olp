import { expect, test, type Page } from '@playwright/test';

const ids = {
  user: '01980000-0000-7000-8000-000000000001',
  activeOne: '01980000-0000-7000-8000-000000000101',
  activeTwo: '01980000-0000-7000-8000-000000000102',
  draftOne: '01980000-0000-7000-8000-000000000201',
  draftTwo: '01980000-0000-7000-8000-000000000202',
  requestOne: '01980000-0000-7000-8000-000000000301',
  requestTwo: '01980000-0000-7000-8000-000000000302',
  provider: '01980000-0000-7000-8000-000000000303',
  key: '01980000-0000-7000-8000-000000000304',
  generation: '01980000-0000-7000-8000-000000000305',
  jobOne: '01980000-0000-7000-8000-000000000401',
  jobTwo: '01980000-0000-7000-8000-000000000402',
  apiKeyOne: '01980000-0000-7000-8000-000000000501',
  apiKeyTwo: '01980000-0000-7000-8000-000000000502'
};
const now = '2026-07-12T12:00:00Z';

async function mockSession(page: Page) {
  await page.route('**/api/v1/sessions/current', async (route) => {
    await route.fulfill({
      json: {
        user: {
          id: ids.user,
          email: 'owner@example.com',
          display_name: 'Ada Owner',
          role: 'owner'
        },
        csrf_token: 'csrf-routing-state'
      }
    });
  });
}

function activeRoute(id: string, slug: string) {
  return {
    id,
    slug,
    created_at: now,
    revision_count: 1,
    latest_revision: {
      id: `${id}-revision`,
      route_id: id,
      revision: 1,
      slug,
      overall_timeout_ms: 120000,
      max_attempts: 1,
      source_draft_id: ids.draftOne,
      activated_by: ids.user,
      activated_at: now,
      operations: ['generation'],
      targets: []
    }
  };
}

function routeDraft(id: string, slug: string) {
  return {
    id,
    slug,
    state: 'draft',
    overall_timeout_ms: 120000,
    max_attempts: 1,
    etag: `${id}-etag`,
    based_on_revision_id: null,
    operations: ['generation'],
    targets: [],
    created_at: now,
    updated_at: now
  };
}

function requestSummary(id: string, route: string) {
  return {
    id,
    runtime_generation_id: ids.generation,
    api_key_id: ids.key,
    route,
    operation: 'generation',
    surface: 'openai',
    started_at: now,
    completed_at: now,
    status_code: 200,
    error_class: null,
    total_latency_ms: 120,
    first_byte_ms: 40,
    attempt_count: 1,
    input_tokens: 10,
    output_tokens: 5,
    cached_input_tokens: 0,
    estimated_cost: '0.001',
    unpriced: false,
    usage_complete: true
  };
}

function requestDetail(id: string, route: string) {
  return { ...requestSummary(id, route), attempts: [] };
}

function mediaJob(id: string, route: string) {
  return {
    id,
    upstream_job_id: `${id}-upstream`,
    api_key_id: ids.key,
    provider_id: ids.provider,
    provider_name: 'production-openai',
    provider_model: 'video-test',
    route,
    operation: 'video_create',
    surface: 'openai',
    state: 'running',
    lifecycle: 'active',
    progress_percent: 40,
    content_available: false,
    expires_at: null,
    error_class: null,
    completed_at: null,
    last_polled_at: now,
    reconciliation_error: null,
    deleted_at: null,
    etag: `${id}-etag`,
    created_at: now,
    updated_at: now
  };
}

function apiKey(id: string, name: string) {
  return {
    id,
    lookup_id: `olp_${id.slice(-4)}`,
    name,
    scopes: ['inference'],
    allowed_routes: [],
    requests_per_minute: null,
    tokens_per_minute: null,
    max_concurrency: null,
    expires_at: null,
    revoked_at: null,
    rotated_at: null,
    etag: `${id}-etag`,
    created_by: ids.user,
    created_by_email: 'owner@example.com',
    created_at: now
  };
}

test('route list keeps both cursor tracks through the new-route boundary', async ({ page }) => {
  await mockSession(page);
  await page.route(/\/api\/v1\/(?:routes|route-drafts)(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url());
    const cursor = url.searchParams.get('cursor');
    if (url.pathname === '/api/v1/routes') {
      await route.fulfill({
        json: cursor === 'active-next'
          ? { items: [activeRoute(ids.activeTwo, 'active-page-two')], next_cursor: null }
          : { items: [activeRoute(ids.activeOne, 'active-page-one')], next_cursor: 'active-next' }
      });
      return;
    }
    await route.fulfill({
      json: cursor === 'draft-next'
        ? { items: [routeDraft(ids.draftTwo, 'draft-page-two')], next_cursor: null }
        : { items: [routeDraft(ids.draftOne, 'draft-page-one')], next_cursor: 'draft-next' }
    });
  });
  await page.route('**/api/v1/provider-models**', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route(`**/api/v1/route-drafts/${ids.draftTwo}`, async (route) => {
    await route.fulfill({ json: routeDraft(ids.draftTwo, 'draft-page-two') });
  });

  await page.goto('/routes');
  await page.getByLabel('Active route pages').getByRole('button', { name: 'Next' }).click();
  await page.getByLabel('Route draft pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('active-page-two', { exact: true })).toBeVisible();
  await expect(page.getByText('draft-page-two', { exact: true })).toBeVisible();

  await page.getByRole('link', { name: /New route draft/ }).click();
  await expect(page.getByRole('heading', { name: 'Build a route draft.' })).toBeVisible();
  await page.getByRole('link', { name: 'Cancel' }).click();

  await expect(page.getByLabel('Active route pages')).toContainText('Page 2');
  await expect(page.getByLabel('Route draft pages')).toContainText('Page 2');
  await expect(page.getByText('active-page-two', { exact: true })).toBeVisible();
  await expect(page.getByText('draft-page-two', { exact: true })).toBeVisible();

  await page.getByRole('link', { name: 'draft-page-two', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'draft-page-two', exact: true })).toBeVisible();
  await page.getByRole('link', { name: 'Cancel' }).click();
  await expect(page.getByLabel('Active route pages')).toContainText('Page 2');
  await expect(page.getByLabel('Route draft pages')).toContainText('Page 2');
});

test('request filters and cursor history survive list-detail-list navigation', async ({ page }) => {
  await mockSession(page);
  const seenFilters: URLSearchParams[] = [];
  await page.route(/\/api\/v1\/requests(?:\/[^?]+)?(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname !== '/api/v1/requests') {
      await route.fulfill({ json: requestDetail(ids.requestTwo, 'request-page-two') });
      return;
    }
    seenFilters.push(url.searchParams);
    await route.fulfill({
      json: url.searchParams.get('cursor') === 'request-next'
        ? { data: [requestSummary(ids.requestTwo, 'request-page-two')], next_cursor: null }
        : { data: [requestSummary(ids.requestOne, 'request-page-one')], next_cursor: 'request-next' }
    });
  });

  await page.goto('/requests');
  await page.getByLabel('Route').fill('support-chat');
  await page.getByLabel('Operation').fill('generation');
  await page.getByLabel('Provider ID').fill(ids.provider);
  await page.getByLabel('Model').fill('gpt-test');
  await page.getByLabel('API key ID').fill(ids.key);
  await page.getByLabel('Status code').fill('200');
  await page.getByLabel('Error class').fill('transport');
  await page.getByLabel('Started after').fill('2026-07-12T10:00');
  await page.getByLabel('Started before').fill('2026-07-12T14:00');
  expect(await page.getByRole('form', { name: 'Request filters' }).evaluate((form) => ({
    valid: (form as HTMLFormElement).checkValidity(),
    invalid: Array.from((form as HTMLFormElement).elements)
      .filter((element) => element instanceof HTMLInputElement && !element.checkValidity())
      .map((element) => ({ name: (element as HTMLInputElement).name, message: (element as HTMLInputElement).validationMessage })),
    values: Object.fromEntries(new FormData(form as HTMLFormElement))
  }))).toEqual({
    valid: true,
    invalid: [],
    values: {
      route: 'support-chat',
      operation: 'generation',
      provider: ids.provider,
      model: 'gpt-test',
      key: ids.key,
      status: '200',
      error: 'transport',
      after: '2026-07-12T10:00',
      before: '2026-07-12T14:00'
    }
  });
  await page.getByRole('button', { name: 'Apply filters' }).click();
  await expect.poll(() => seenFilters.some((filters) =>
    filters.get('route') === 'support-chat'
      && filters.get('provider_id') === ids.provider
      && filters.get('model') === 'gpt-test'
      && filters.get('api_key_id') === ids.key
      && filters.get('operation') === 'generation'
      && filters.get('status_code') === '200'
      && filters.get('error_class') === 'transport'
      && filters.has('started_after')
      && filters.has('started_before')
      && !filters.has('cursor')
  )).toBe(true);
  await page.getByLabel('Request pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('request-page-two', { exact: true }).first()).toBeVisible();

  await page.getByRole('link', { name: `View request ${ids.requestTwo}` }).click();
  await page.getByRole('link', { name: 'Back to requests' }).click();

  await expect(page.getByLabel('Route')).toHaveValue('support-chat');
  await expect(page.getByLabel('Operation')).toHaveValue('generation');
  await expect(page.getByLabel('Provider ID')).toHaveValue(ids.provider);
  await expect(page.getByLabel('Model')).toHaveValue('gpt-test');
  await expect(page.getByLabel('API key ID')).toHaveValue(ids.key);
  await expect(page.getByLabel('Status code')).toHaveValue('200');
  await expect(page.getByLabel('Error class')).toHaveValue('transport');
  await expect(page.getByLabel('Started after')).toHaveValue('2026-07-12T10:00');
  await expect(page.getByLabel('Started before')).toHaveValue('2026-07-12T14:00');
  await expect(page.getByLabel('Request pages')).toContainText('Page 2');
  await expect(page.getByText('request-page-two', { exact: true }).first()).toBeVisible();
  expect(seenFilters.map((filters) => Object.fromEntries(filters))).toEqual(
    expect.arrayContaining([
      expect.objectContaining({
        route: 'support-chat',
        provider_id: ids.provider,
        model: 'gpt-test',
        api_key_id: ids.key,
        operation: 'generation',
        status_code: '200',
        error_class: 'transport',
        started_after: expect.any(String),
        started_before: expect.any(String),
        cursor: 'request-next'
      })
    ])
  );
});

test('media-job filter drafts, applied filters, and pagination survive detail navigation', async ({ page }) => {
  await mockSession(page);
  const seenFilters: URLSearchParams[] = [];
  await page.route(/\/api\/v1\/media-jobs(?:\/[^?]+)?(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname !== '/api/v1/media-jobs') {
      await route.fulfill({ json: mediaJob(ids.jobTwo, 'media-page-two') });
      return;
    }
    seenFilters.push(url.searchParams);
    await route.fulfill({
      json: url.searchParams.get('cursor') === 'media-next'
        ? { data: [mediaJob(ids.jobTwo, 'media-page-two')], next_cursor: null }
        : { data: [mediaJob(ids.jobOne, 'media-page-one')], next_cursor: 'media-next' }
    });
  });

  await page.goto('/media-jobs');
  await page.getByLabel('Route').fill('video-route');
  await page.getByLabel('State').selectOption('running');
  await page.getByLabel('Lifecycle').selectOption('active');
  await page.getByRole('button', { name: 'Apply filters' }).click();
  await page.getByLabel('Media job pages').getByRole('button', { name: 'Next' }).click();
  await page.getByRole('link', { name: 'View', exact: true }).click();
  await page.getByRole('link', { name: 'All media jobs' }).click();

  await expect(page.getByLabel('Route')).toHaveValue('video-route');
  await expect(page.getByLabel('State')).toHaveValue('running');
  await expect(page.getByLabel('Lifecycle')).toHaveValue('active');
  await expect(page.getByLabel('Media job pages')).toContainText('Page 2');
  await expect(page.getByText('media-page-two', { exact: true })).toBeVisible();
  expect(seenFilters.some((filters) =>
    filters.get('route') === 'video-route'
      && filters.get('state') === 'running'
      && filters.get('lifecycle') === 'active'
      && filters.get('cursor') === 'media-next'
  )).toBe(true);
});

test('API-key pagination survives new/cancel and resets after leaving the family', async ({ page }) => {
  await mockSession(page);
  await page.route(/\/api\/v1\/api-keys(?:\?.*)?$/, async (route) => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor');
    await route.fulfill({
      json: cursor === 'key-next'
        ? { items: [apiKey(ids.apiKeyTwo, 'key-page-two')], next_cursor: null }
        : { items: [apiKey(ids.apiKeyOne, 'key-page-one')], next_cursor: 'key-next' }
    });
  });
  await page.route('**/api/v1/routes**', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/api/v1/provider-models**', async (route) => {
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });

  await page.goto('/api-keys');
  await page.getByLabel('API key pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('key-page-two', { exact: true })).toBeVisible();
  await page.getByRole('link', { name: /Create key/ }).click();
  await page.getByRole('link', { name: 'Cancel' }).click();

  await expect(page.getByLabel('API key pages')).toContainText('Page 2');
  await expect(page.getByText('key-page-two', { exact: true })).toBeVisible();

  await page.getByRole('link', { name: 'Models', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Model inventory', exact: true })).toBeVisible();
  await page.getByRole('link', { name: 'API Keys', exact: true }).click();
  await expect(page.getByLabel('API key pages')).toContainText('Page 1');
  await expect(page.getByText('key-page-one', { exact: true })).toBeVisible();
});
