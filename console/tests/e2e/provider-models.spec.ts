import {
  expect,
  failUnexpectedApiRequest,
  mockSession,
  test
} from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import {
  certifiedModelRecord,
  ids,
  modelRecord,
  now,
  providerRecord,
  sessionOptions
} from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('provider detail resets provider-wide model mutations and retains row-local model pages', async ({
  page
}) => {
  test.slow();
  await mockSession(page, sessionOptions);
  page.on('dialog', (dialog) => dialog.accept());
  let modelVersion = 0;
  const modelCursors: Array<string | null> = [];
  const revision = {
    id: ids.revision,
    provider_id: ids.provider,
    revision: 1,
    name: 'production-openai',
    kind: 'openai',
    connector_ready: true,
    model_count: 2,
    enabled_model_count: 1,
    capability_count: 2,
    certified_capability_count: 2,
    historical_credential_version: 1,
    activated_at: now,
    activated_by: ids.user
  };
  let currentProvider = providerRecord('active', [certifiedModelRecord], {
    kind: 'openai_compatible',
    endpoint: 'https://models.example.test/v1/',
    model_count: 2,
    active_revision: 1,
    pending_activation: false
  });

  function modelPage(cursor: string | null) {
    const firstPage = cursor !== 'opaque-next-model';
    return {
      ...certifiedModelRecord,
      id: firstPage ? ids.model : '01980000-0000-7000-8000-000000000106',
      upstream_model: `model-page-${firstPage ? 'one' : 'two'}-${modelVersion}`,
      display_name: `model-page-${firstPage ? 'one' : 'two'}-${modelVersion}`
    };
  }

  await page.route(
    '**/api/v1/provider-kinds/openai_compatible/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai_compatible',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'unary' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    if (
      pathname === `/api/v1/providers/${ids.provider}/models` &&
      request.method() === 'GET'
    ) {
      const cursor = url.searchParams.get('cursor');
      modelCursors.push(cursor);
      await route.fulfill({
        json: {
          items: [modelPage(cursor)],
          next_cursor:
            cursor === 'opaque-next-model' ? null : 'opaque-next-model'
        }
      });
      return;
    }
    if (pathname.includes('/models/') && request.method() === 'PATCH') {
      modelVersion += 1;
      currentProvider = {
        ...currentProvider,
        state: 'draft',
        pending_activation: true,
        etag: '01980000-0000-7000-8000-000000000113',
        updated_at: '2026-07-12T12:04:00Z'
      };
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname.endsWith('/certify') && request.method() === 'POST') {
      modelVersion += 1;
      currentProvider = {
        ...currentProvider,
        etag: '01980000-0000-7000-8000-000000000114',
        updated_at: '2026-07-12T12:05:00Z'
      };
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          model_id: '01980000-0000-7000-8000-000000000106',
          status: 'succeeded',
          checked_at: '2026-07-12T12:05:00Z',
          certified_count: 2,
          attempted_count: 2,
          results: certifiedModelRecord.capabilities.map((capability) => ({
            ...capability,
            succeeded: true,
            error_code: null,
            detail: 'Certified by server'
          }))
        }
      });
      return;
    }
    if (pathname.endsWith('/credentials') && request.method() === 'GET') {
      await route.fulfill({
        json: {
          items: [
            {
              id: ids.credential,
              version: 1,
              active: true,
              draft_selected: false,
              created_at: now,
              revoked_at: null
            }
          ]
        }
      });
      return;
    }
    if (pathname.endsWith('/credentials') && request.method() === 'POST') {
      modelVersion += 1;
      currentProvider = {
        ...currentProvider,
        state: 'draft',
        pending_activation: true,
        etag: '01980000-0000-7000-8000-000000000111',
        updated_at: '2026-07-12T12:02:00Z'
      };
      await route.fulfill({
        status: 201,
        json: {
          provider_id: ids.provider,
          credential_id: ids.credential,
          credential_version: 2,
          etag: currentProvider.etag,
          runtime_generation: null
        }
      });
      return;
    }
    if (pathname.endsWith('/discovery') && request.method() === 'POST') {
      modelVersion += 1;
      currentProvider = {
        ...currentProvider,
        state: 'draft',
        pending_activation: true,
        etag: `01980000-0000-7000-8000-00000000011${modelVersion}`,
        updated_at: now
      };
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname.endsWith('/restore-as-draft')) {
      modelVersion += 1;
      currentProvider = {
        ...currentProvider,
        state: 'draft',
        pending_activation: true,
        etag: '01980000-0000-7000-8000-000000000112',
        updated_at: '2026-07-12T12:03:00Z'
      };
      await route.fulfill({
        status: 201,
        json: { credential_restored: false, provider: currentProvider }
      });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [revision], next_cursor: null } });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'PATCH'
    ) {
      modelVersion += 1;
      currentProvider = {
        ...currentProvider,
        state: 'draft',
        pending_activation: true,
        etag: '01980000-0000-7000-8000-000000000110',
        updated_at: '2026-07-12T12:01:00Z'
      };
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'GET'
    ) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(
    page.getByText('model-page-one-0', { exact: true }).first()
  ).toBeVisible();
  await page
    .getByLabel('Provider model pages')
    .getByRole('button', { name: 'Next' })
    .click();
  await expect(
    page.getByText('model-page-two-0', { exact: true }).first()
  ).toBeVisible();

  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(
    page.getByText('model-page-one-1', { exact: true }).first()
  ).toBeVisible();
  expect(modelCursors.at(-1)).toBeNull();

  await page
    .getByLabel('Provider model pages')
    .getByRole('button', { name: 'Next' })
    .click();
  await expect(
    page.getByText('model-page-two-1', { exact: true }).first()
  ).toBeVisible();
  await page
    .getByPlaceholder('New credential')
    .fill('rotated-write-only-secret');
  await page.getByRole('button', { name: 'Stage rotation' }).click();
  await expect(
    page.getByText('model-page-one-2', { exact: true }).first()
  ).toBeVisible();
  expect(modelCursors.at(-1)).toBeNull();

  await page
    .getByLabel('Provider model pages')
    .getByRole('button', { name: 'Next' })
    .click();
  await expect(
    page.getByText('model-page-two-2', { exact: true }).first()
  ).toBeVisible();
  await page.getByRole('button', { name: 'Restore as draft' }).click();
  await expect(
    page.getByText('model-page-one-3', { exact: true }).first()
  ).toBeVisible();
  expect(modelCursors.at(-1)).toBeNull();

  await page
    .getByLabel('Provider model pages')
    .getByRole('button', { name: 'Next' })
    .click();
  await expect(
    page.getByText('model-page-two-3', { exact: true }).first()
  ).toBeVisible();
  const discoveryRequests = modelCursors.length;
  await page.getByRole('button', { name: 'Run upstream discovery' }).click();
  await expect(
    page.getByText('model-page-one-4', { exact: true }).first()
  ).toBeVisible();
  expect(modelCursors.slice(discoveryRequests)).not.toContain(
    'opaque-next-model'
  );

  await page
    .getByLabel('Provider model pages')
    .getByRole('button', { name: 'Next' })
    .click();
  await expect(
    page.getByText('model-page-two-4', { exact: true }).first()
  ).toBeVisible();
  await page.getByText('Manual model identifiers', { exact: true }).click();
  await page.getByLabel('Upstream model identifiers').fill('manual-model');
  const declarationRequests = modelCursors.length;
  await page
    .getByRole('button', { name: 'Add identifiers for review' })
    .click();
  await expect(
    page.getByText('model-page-one-5', { exact: true }).first()
  ).toBeVisible();
  expect(modelCursors.slice(declarationRequests)).not.toContain(
    'opaque-next-model'
  );

  await page
    .getByLabel('Provider model pages')
    .getByRole('button', { name: 'Next' })
    .click();
  await expect(
    page.getByText('model-page-two-5', { exact: true }).first()
  ).toBeVisible();
  const reviewRequests = modelCursors.length;
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).uncheck();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('model-page-two-6', { exact: true }).first()
  ).toBeVisible();
  expect(modelCursors.slice(reviewRequests)).toEqual(['opaque-next-model']);

  const certificationRequests = modelCursors.length;
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(
    page.getByText('model-page-two-7', { exact: true }).first()
  ).toBeVisible();
  await expect(page.getByText('2/2 certified', { exact: true })).toBeVisible();
  expect(modelCursors.slice(certificationRequests)).toEqual([
    'opaque-next-model'
  ]);
});

test('provider revision viewer shows historical configuration and paged models', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  const revision = {
    id: ids.revision,
    provider_id: ids.provider,
    revision: 3,
    name: 'production-openai',
    kind: 'openai_compatible',
    auth_mode: 'api_key',
    endpoint: 'https://models.example.test/v1/',
    cloud_region: 'westus3',
    cloud_project: null,
    deployment: 'gpt-5-4-prod',
    api_version: '2026-05-01',
    connector_ready: true,
    model_count: 2,
    enabled_model_count: 1,
    capability_count: 2,
    certified_capability_count: 2,
    historical_credential_version: 4,
    source_etag: '01980000-0000-7000-8000-000000000501',
    activated_at: now,
    activated_by: ids.user
  };
  const currentProvider = providerRecord('active', [certifiedModelRecord], {
    kind: 'openai_compatible',
    endpoint: 'https://models.example.test/v1/',
    active_revision: 3,
    pending_activation: false
  });
  const revisionModelCursors: Array<string | null> = [];

  await page.route(
    '**/api/v1/provider-kinds/openai_compatible/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai_compatible',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'unary' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    const revisionBase = `/api/v1/providers/${ids.provider}/revisions/${ids.revision}`;
    if (pathname === `${revisionBase}/models`) {
      const cursor = url.searchParams.get('cursor');
      revisionModelCursors.push(cursor);
      const firstPage = cursor !== 'opaque-revision-models';
      await route.fulfill({
        json: {
          items: [
            {
              ...certifiedModelRecord,
              id: firstPage
                ? ids.model
                : '01980000-0000-7000-8000-000000000107',
              upstream_model: firstPage ? 'archived-one' : 'archived-two',
              display_name: firstPage ? 'archived-one' : 'archived-two',
              enabled: firstPage
            }
          ],
          next_cursor: firstPage ? 'opaque-revision-models' : null
        }
      });
      return;
    }
    if (pathname === revisionBase) {
      await route.fulfill({ json: revision });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [revision], next_cursor: null } });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}/models` &&
      request.method() === 'GET'
    ) {
      await route.fulfill({
        json: { items: currentProvider.models, next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/credentials') && request.method() === 'GET') {
      await route.fulfill({
        json: {
          items: [
            {
              id: ids.credential,
              version: 1,
              active: true,
              draft_selected: false,
              created_at: now,
              revoked_at: null
            }
          ]
        }
      });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await page.getByRole('button', { name: 'View revision 3' }).click();

  const dialog = page.getByRole('dialog');
  await expect(
    dialog.getByRole('heading', { name: 'Revision 3' })
  ).toBeVisible();
  await expect(
    dialog.getByText('https://models.example.test/v1/')
  ).toBeVisible();
  await expect(dialog.getByText('gpt-5-4-prod')).toBeVisible();
  await expect(dialog.getByText('2026-05-01')).toBeVisible();
  await expect(dialog.getByText('westus3')).toBeVisible();
  await expect(dialog.getByText('2 total · 1 enabled')).toBeVisible();
  await expect(dialog.getByText('2 total · 2 certified')).toBeVisible();
  await expect(dialog.getByText('Version 4')).toBeVisible();
  await expect(
    dialog.getByText('01980000-0000-7000-8000-000000000501')
  ).toBeVisible();
  await expect(dialog.getByText('archived-one').first()).toBeVisible();
  await expect(
    dialog.getByText('generation/openai/streaming').first()
  ).toBeVisible();

  await dialog
    .getByLabel('Revision model pages')
    .getByRole('button', { name: 'Next' })
    .click();
  await expect(dialog.getByText('archived-two').first()).toBeVisible();
  await expect(dialog.getByText('not enabled')).toBeVisible();
  expect(revisionModelCursors).toEqual([null, 'opaque-revision-models']);

  await dialog.getByRole('button', { name: 'Close revision' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('failed certification tuples show the server detail and error code', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let currentProvider = providerRecord('draft', [modelRecord], {
    kind: 'openai_compatible',
    endpoint: 'https://models.example.test/v1/'
  });

  await page.route(
    '**/api/v1/provider-kinds/openai_compatible/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai_compatible',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'unary' },
            { operation: 'generation', surface: 'openai', mode: 'streaming' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (
      pathname === `/api/v1/providers/${ids.provider}/models` &&
      request.method() === 'GET'
    ) {
      await route.fulfill({
        json: { items: currentProvider.models, next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/credentials') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [] } });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (pathname.endsWith(`/models/${ids.model}/certify`)) {
      currentProvider = {
        ...currentProvider,
        etag: '01980000-0000-7000-8000-000000000601',
        updated_at: '2026-07-12T12:30:00Z'
      };
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          model_id: ids.model,
          status: 'partial',
          checked_at: '2026-07-12T12:30:00Z',
          certified_count: 1,
          attempted_count: 2,
          results: [
            {
              operation: 'generation',
              surface: 'openai',
              mode: 'unary',
              succeeded: true,
              error_code: null,
              detail: 'Certified by server'
            },
            {
              operation: 'generation',
              surface: 'openai',
              mode: 'streaming',
              succeeded: false,
              error_code: 'upstream_unsupported_mode',
              detail: 'The upstream rejected the streaming request.'
            }
          ]
        }
      });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'GET'
    ) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(page.getByText('1/2 certified', { exact: true })).toBeVisible();
  const failures = page.getByLabel('Failed certification tuples');
  await expect(
    failures.getByText('The upstream rejected the streaming request.')
  ).toBeVisible();
  await expect(failures.getByText('upstream_unsupported_mode')).toBeVisible();
  await expect(failures.getByText('generation/openai/unary')).toHaveCount(0);
});
