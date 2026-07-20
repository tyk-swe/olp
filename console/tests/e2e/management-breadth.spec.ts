import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

const ids = {
  user: '01980000-0000-7000-8000-000000000a01',
  provider: '01980000-0000-7000-8000-000000000a02',
  revisionOne: '01980000-0000-7000-8000-000000000a03',
  revisionTwo: '01980000-0000-7000-8000-000000000a04',
  job: '01980000-0000-7000-8000-000000000a05',
  key: '01980000-0000-7000-8000-000000000a06'
};
const now = '2026-07-12T12:00:00Z';

async function mockSession(page: Page) {
  await page.route('**/api/v1/sessions/current', async (route) => {
    await route.fulfill({
      json: {
        user: { id: ids.user, email: 'owner@example.com', display_name: 'Ada Owner', role: 'owner' },
        csrf_token: 'csrf-management-breadth'
      }
    });
  });
}

function providerRecord(state = 'active') {
  return {
    id: ids.provider,
    name: 'production-openai',
    kind: 'openai',
    state,
    auth_mode: 'api_key',
    connector_ready: true,
    endpoint: null,
    api_version: null,
    cloud_region: null,
    cloud_project: null,
    deployment: null,
    active_revision: 2,
    pending_activation: state === 'draft',
    draft_credential_id: null,
    draft_credential_version: 3,
    runtime_credential_id: null,
    runtime_credential_version: 3,
    last_probe_at: now,
    last_probe_status: 'succeeded',
    last_probe_detail: 'reachable',
    etag: '01980000-0000-7000-8000-000000000a07',
    created_at: now,
    updated_at: now,
    model_count: 0,
    enabled_model_count: 0,
    capability_count: 0,
    certified_capability_count: 0
  };
}

function revision(id: string, number: number) {
  return {
    id,
    provider_id: ids.provider,
    revision: number,
    source_etag: '01980000-0000-7000-8000-000000000a08',
    name: 'production-openai',
    kind: 'openai',
    endpoint: null,
    cloud_region: null,
    cloud_project: null,
    deployment: null,
    api_version: null,
    auth_mode: 'api_key',
    historical_credential_version: number + 1,
    connector_ready: true,
    activated_by: ids.user,
    activated_at: now,
    model_count: 0,
    enabled_model_count: 0,
    capability_count: 0,
    certified_capability_count: 0
  };
}

test('provider studio compares redacted history and restores non-secret configuration', async ({ page }) => {
  await mockSession(page);
  let current = providerRecord();
  let restoreHeaders: Record<string, string> = {};
  await page.route('**/api/v1/providers/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname.endsWith('/credentials')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (url.pathname.endsWith('/models')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (url.pathname.endsWith('/revisions/diff')) {
      await route.fulfill({ json: {
        from_revision: 1, to_revision: 2, name_changed: false,
        connector_changed: false, endpoint_changed: true, cloud_context_changed: false,
        deployment_changed: false, api_version_changed: false, credential_changed: true,
        models_added: [], models_changed: ['gpt-test'], models_removed: [],
        capabilities_added: ['gpt-test/generation/openai/streaming'], capabilities_removed: []
      } });
      return;
    }
    if (url.pathname.endsWith('/restore-as-draft')) {
      restoreHeaders = await request.allHeaders();
      current = providerRecord('draft');
      await route.fulfill({ json: { provider: current, credential_restored: false } });
      return;
    }
    if (url.pathname.endsWith('/revisions')) {
      await route.fulfill({ json: {
        items: [revision(ids.revisionTwo, 2), revision(ids.revisionOne, 1)],
        next_cursor: null
      } });
      return;
    }
    if (url.pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: current });
      return;
    }
    await route.fulfill({ status: 404, json: { title: 'Not mocked', status: 404 } });
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(page.getByRole('heading', { name: 'Provider revisions' })).toBeVisible();
  await page.getByRole('button', { name: 'Compare' }).click();
  await expect(page.getByRole('region', { name: 'Provider revision 1 to 2 diff' })).toContainText('Endpoint changed');
  await expect(page.getByText('gpt-test/generation/openai/streaming')).toBeVisible();
  await expect(page.getByText(/credential metadata version 2/)).toBeVisible();
  await expect(page.getByText('historical-credential-secret')).toHaveCount(0);

  page.once('dialog', (dialog) => dialog.accept());
  await page.locator('.revision-row').filter({ hasText: 'Revision 1' }).getByRole('button', { name: 'Restore as draft' }).click();
  await expect(page.getByText(/Current credential selection was preserved/)).toBeVisible();
  expect(restoreHeaders['if-match']).toBe(current.etag);
  expect(restoreHeaders['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('media job explorer stays metadata-only through list and detail', async ({ page }) => {
  await mockSession(page);
  const job = {
    id: ids.job,
    upstream_job_id: 'video_upstream_1',
    api_key_id: ids.key,
    provider_id: ids.provider,
    provider_name: 'production-openai',
    provider_model: 'sora-test',
    route: 'video-route',
    operation: 'video_create',
    surface: 'openai',
    state: 'running',
    lifecycle: 'active',
    progress_percent: 42,
    content_available: false,
    expires_at: null,
    error_class: null,
    completed_at: null,
    last_polled_at: now,
    reconciliation_error: null,
    deleted_at: null,
    etag: '"01980000-0000-7000-8000-000000000a09"',
    created_at: now,
    updated_at: now
  };
  await page.route(/\/api\/v1\/media-jobs(?:\/[^?]+)?(?:\?.*)?$/, async (route) => {
    const pathname = new URL(route.request().url()).pathname;
    await route.fulfill({ json: pathname === `/api/v1/media-jobs/${ids.job}` ? job : { data: [job], next_cursor: null } });
  });

  await page.goto('/media-jobs');
  await expect(page.getByRole('heading', { name: 'Media Jobs' })).toBeVisible();
  await expect(page.getByRole('table')).toContainText('video-route');
  await expect(page.getByText('secret prompt')).toHaveCount(0);
  await page.getByRole('link', { name: 'View', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Media job detail' })).toBeVisible();
  await expect(page.getByText('Available through the authenticated vendor API')).toHaveCount(0);
  await expect(page.getByText('video_upstream_1')).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
