import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

const ids = {
  provider: '01980000-0000-7000-8000-000000000101',
  model: '01980000-0000-7000-8000-000000000102',
  credential: '01980000-0000-7000-8000-000000000103',
  draft: '01980000-0000-7000-8000-000000000201',
  target: '01980000-0000-7000-8000-000000000202',
  route: '01980000-0000-7000-8000-000000000203',
  revision: '01980000-0000-7000-8000-000000000204',
  generation: '01980000-0000-7000-8000-000000000205',
  key: '01980000-0000-7000-8000-000000000301',
  user: '01980000-0000-7000-8000-000000000401',
  developer: '01980000-0000-7000-8000-000000000402',
  invitation: '01980000-0000-7000-8000-000000000403',
  session: '01980000-0000-7000-8000-000000000404',
  oidc: '01980000-0000-7000-8000-000000000405'
};

const now = '2026-07-12T12:00:00Z';

async function mockSession(page: Page) {
  await page.route('**/api/v1/sessions/current', async (route) => {
    await route.fulfill({
      json: {
        user: { id: ids.user, email: 'owner@example.com', display_name: 'Ada Owner', role: 'owner' },
        csrf_token: 'csrf-e2e'
      }
    });
  });
}

async function emulateTwoHundredPercentZoom(page: Page) {
  const viewport = page.viewportSize();
  // Desktop browser zoom must reflow at 200%. Mobile projects already exercise
  // the narrow layout; resizing an emulated device desynchronizes Chromium's
  // visual and layout viewports after its device scale has been established.
  if (!viewport || viewport.width <= 480) return;
  // Browser zoom halves the available CSS-pixel viewport. Resizing exercises
  // the same reflow contract without CSS `zoom`, which incorrectly scales
  // fixed-position dialog viewport units in headless engines.
  await page.setViewportSize({
    width: Math.max(320, Math.floor(viewport.width / 2)),
    height: Math.max(480, Math.floor(viewport.height / 2))
  });
}

function providerRecord(
  state = 'draft',
  models: Array<Record<string, unknown>> = [],
  overrides: Record<string, unknown> = {}
) {
  const hasRuntimeRevision = state === 'active';
  const enabledModels = models.filter((model) => model.enabled === true);
  const capabilities = models.flatMap((model) =>
    Array.isArray(model.capabilities) ? model.capabilities : []
  ) as Array<Record<string, unknown>>;
  return {
    id: ids.provider,
    name: 'production-openai',
    kind: 'open_ai',
    state,
    auth_mode: 'api_key',
    connector_ready: true,
    endpoint: null,
    api_version: null,
    cloud_region: null,
    cloud_project: null,
    deployment: null,
    active_revision: hasRuntimeRevision ? 1 : null,
    pending_activation: false,
    draft_credential_id: ids.credential,
    draft_credential_version: 1,
    runtime_credential_id: hasRuntimeRevision ? ids.credential : null,
    runtime_credential_version: hasRuntimeRevision ? 1 : null,
    last_probe_at: state === 'draft' ? null : now,
    last_probe_status: state === 'draft' ? null : 'succeeded',
    last_probe_detail: state === 'draft' ? null : 'OpenAI reachable',
    etag: '01980000-0000-7000-8000-000000000109',
    created_at: now,
    updated_at: now,
    model_count: models.length,
    enabled_model_count: enabledModels.length,
    capability_count: capabilities.length,
    certified_capability_count: capabilities.filter((capability) => capability.source === 'certified').length,
    // Test-only state used to serve the separately paginated model endpoint.
    models,
    ...overrides
  };
}

function withProviderModels(
  provider: ReturnType<typeof providerRecord>,
  models: Array<Record<string, unknown>>,
  overrides: Record<string, unknown> = {}
) {
  const enabledModels = models.filter((model) => model.enabled === true);
  const capabilities = models.flatMap((model) =>
    Array.isArray(model.capabilities) ? model.capabilities : []
  ) as Array<Record<string, unknown>>;
  return {
    ...provider,
    model_count: models.length,
    enabled_model_count: enabledModels.length,
    capability_count: capabilities.length,
    certified_capability_count: capabilities.filter((capability) => capability.source === 'certified').length,
    models,
    ...overrides
  };
}

const modelRecord = {
  id: ids.model,
  upstream_model: 'gpt-5.4',
  display_name: 'gpt-5.4',
  enabled: true,
  discovered_at: now,
  capabilities: [
    { operation: 'generation', surface: 'open_ai', mode: 'streaming', source: 'declared' },
    { operation: 'generation', surface: 'open_ai', mode: 'unary', source: 'declared' }
  ]
};

const certifiedModelRecord = {
  ...modelRecord,
  capabilities: modelRecord.capabilities.map((capability) => ({
    ...capability,
    source: 'certified',
    certified_at: now
  }))
};

test('provider wizard keeps the write-only secret out of subsequent steps', async ({ page }) => {
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await mockSession(page);
  let currentProvider = providerRecord();
  let createBody: Record<string, unknown> | undefined;
  let createHeaders: Record<string, string> = {};
  const probeEtags: string[] = [];
  let certificationEtag = '';

  await page.route('**/api/v1/provider-kinds/open_ai/capabilities', async (route) => {
    await route.fulfill({
      json: {
        provider_kind: 'open_ai',
        capabilities: [
          { operation: 'generation', surface: 'open_ai', mode: 'unary' },
          { operation: 'generation', surface: 'open_ai', mode: 'streaming' }
        ]
      }
    });
  });

  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    if (pathname === '/api/v1/providers' && request.method() === 'POST') {
      createBody = request.postDataJSON();
      createHeaders = await request.allHeaders();
      await route.fulfill({ status: 201, json: { id: ids.provider, name: 'production-openai', kind: 'open_ai', state: 'draft', model: 'gpt-5.4', etag: currentProvider.etag } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}` && request.method() === 'GET') {
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models` && request.method() === 'GET') {
      await route.fulfill({ json: { items: currentProvider.models, next_cursor: null } });
      return;
    }
    if (pathname.endsWith('/probe')) {
      const headers = await request.allHeaders();
      probeEtags.push(headers['if-match']);
      const checkedAt = currentProvider.models.length ? '2026-07-12T12:04:00Z' : '2026-07-12T12:00:30Z';
      currentProvider = { ...currentProvider, last_probe_at: checkedAt, last_probe_status: 'succeeded', last_probe_detail: 'OpenAI reachable' };
      await route.fulfill({ json: { provider_id: ids.provider, succeeded: true, checked_at: checkedAt, probe_type: 'connector_connectivity', detail: 'OpenAI reachable' } });
      return;
    }
    if (pathname.endsWith('/discovery')) {
      currentProvider = providerRecord('draft', [{ ...modelRecord, enabled: false, capabilities: [] }], {
        etag: '01980000-0000-7000-8000-000000000110',
        updated_at: '2026-07-12T12:01:00Z'
      });
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname.endsWith(`/models/${ids.model}`)) {
      const body = request.postDataJSON() as { enabled: boolean; capabilities: Array<{ operation: string; surface: string; mode: string }> };
      currentProvider = providerRecord('draft', [{ ...modelRecord, enabled: body.enabled, capabilities: body.capabilities.map((capability) => ({ ...capability, source: 'declared', certified_at: null })) }], {
        etag: '01980000-0000-7000-8000-000000000111',
        updated_at: '2026-07-12T12:02:00Z'
      });
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname.endsWith(`/models/${ids.model}/certify`)) {
      const headers = await request.allHeaders();
      certificationEtag = headers['if-match'];
      const capabilities = (currentProvider.models[0].capabilities as Array<Record<string, unknown>>).map((capability) => ({
        ...capability,
        source: 'certified',
        certified_at: '2026-07-12T12:03:00Z'
      }));
      currentProvider = withProviderModels(currentProvider, [{ ...currentProvider.models[0], capabilities }], {
        etag: '01980000-0000-7000-8000-000000000112',
        updated_at: '2026-07-12T12:03:00Z',
        last_probe_at: null,
        last_probe_status: null,
        last_probe_detail: null
      });
      await route.fulfill({ json: {
        provider_id: ids.provider,
        model_id: ids.model,
        status: 'succeeded',
        checked_at: '2026-07-12T12:03:00Z',
        certified_count: capabilities.length,
        attempted_count: capabilities.length,
        results: capabilities.map((capability) => ({ ...capability, succeeded: true, error_code: null, detail: 'Certified by server' }))
      } });
      return;
    }
    if (pathname.endsWith('/activate')) {
      currentProvider = providerRecord('active', currentProvider.models, {
        etag: '01980000-0000-7000-8000-000000000113',
        active_revision: 1,
        pending_activation: false,
        last_probe_at: '2026-07-12T12:04:00Z',
        updated_at: '2026-07-12T12:04:00Z'
      });
      await route.fulfill({ json: { id: ids.provider, state: 'active', etag: currentProvider.etag, runtime_generation: { id: ids.generation, sequence: 2 } } });
      return;
    }
    await route.fulfill({ status: 404, json: { title: 'Not mocked', status: 404, detail: pathname } });
  });

  await emulateTwoHundredPercentZoom(page);
  await page.goto('/providers/new');
  await expect(page.getByRole('heading', { name: 'Connect an upstream provider.' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.getByLabel('Provider name').fill('production-openai');
  await page.getByLabel('Seed model (optional)').fill('gpt-5.4');
  await page.getByLabel('Credential', { exact: true }).fill('sk-upstream-secret');
  await page.getByRole('button', { name: /Save and test connection/ }).click();

  await expect(page.getByRole('heading', { name: 'Verify upstream reachability' })).toBeVisible();
  await expect(page.getByText('sk-upstream-secret')).toHaveCount(0);
  expect(createBody).toMatchObject({
    name: 'production-openai',
    kind: 'open_ai',
    model: 'gpt-5.4',
    credential: 'sk-upstream-secret',
    endpoint: null,
    api_version: null,
    cloud_region: null,
    cloud_project: null,
    deployment: null
  });
  expect(createHeaders['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  expect(createHeaders['x-csrf-token']).toBe('csrf-e2e');

  await page.getByRole('button', { name: 'Test connection' }).click();
  await expect(page.getByRole('heading', { name: 'Discover upstream models' })).toBeVisible();
  await page.getByRole('button', { name: 'Discover upstream models' }).click();
  await expect(page.getByRole('heading', { name: 'Review model capabilities' })).toBeVisible();
  await page.getByRole('button', { name: 'Add capability' }).click();
  await expect(page.getByLabel('Operation 1').locator('option[value="model_list"]')).toHaveCount(0);
  await expect(page.getByLabel('Operation 1').locator('option[value="model_get"]')).toHaveCount(0);
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(page.getByText('Capability review saved with declared provenance.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Activate provider' })).toBeDisabled();
  await page.getByRole('button', { name: 'Server-certify capabilities' }).click();
  await expect(page.getByText(/reviewed tuples passed server certification/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Activate provider' })).toBeDisabled();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Activate provider' })).toBeEnabled();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(page.getByRole('heading', { name: 'Now build a stable route slug.' })).toBeVisible();
  await expect(page.getByText('Provider activated in runtime generation 2.')).toBeVisible();
  expect(certificationEtag).toBe('01980000-0000-7000-8000-000000000111');
  expect(probeEtags).toEqual([
    '01980000-0000-7000-8000-000000000109',
    '01980000-0000-7000-8000-000000000112'
  ]);
});

test('native provider detail never round-trips its official endpoint as a custom endpoint', async ({ page }) => {
  await mockSession(page);
  let updateBody: Record<string, unknown> | undefined;
  let currentProvider = providerRecord('active', [certifiedModelRecord], {
    endpoint: 'https://api.openai.com/v1/'
  });
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/credentials')) {
      await route.fulfill({ json: { items: [] } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({ json: { items: currentProvider.models, next_cursor: null } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}` && request.method() === 'PATCH') {
      updateBody = request.postDataJSON();
      currentProvider = { ...currentProvider, state: 'draft', endpoint: null };
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    await route.fulfill({ status: 404, json: { title: 'Not mocked', status: 404 } });
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(page.getByRole('heading', { name: 'Connector context' })).toBeVisible();
  await expect(page.getByLabel('Endpoint', { exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(page.getByText('Provider draft settings saved.')).toBeVisible();
  expect(updateBody).toMatchObject({
    name: 'production-openai',
    endpoint: null,
    api_version: null,
    cloud_region: null,
    cloud_project: null,
    deployment: null,
    auth_mode: 'api_key'
  });
});

test('provider detail resets provider-wide model mutations and retains row-local model pages', async ({ page }) => {
  await mockSession(page);
  page.on('dialog', (dialog) => dialog.accept());
  let modelVersion = 0;
  const modelCursors: Array<string | null> = [];
  const revision = {
    id: ids.revision,
    provider_id: ids.provider,
    revision: 1,
    name: 'production-openai',
    kind: 'open_ai',
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
    kind: 'open_ai_compatible',
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

  await page.route('**/api/v1/provider-kinds/open_ai_compatible/capabilities', async (route) => {
    await route.fulfill({
      json: {
        provider_kind: 'open_ai_compatible',
        capabilities: [{ operation: 'generation', surface: 'open_ai', mode: 'unary' }]
      }
    });
  });
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    if (pathname === `/api/v1/providers/${ids.provider}/models` && request.method() === 'GET') {
      const cursor = url.searchParams.get('cursor');
      modelCursors.push(cursor);
      await route.fulfill({
        json: {
          items: [modelPage(cursor)],
          next_cursor: cursor === 'opaque-next-model' ? null : 'opaque-next-model'
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
          items: [{
            id: ids.credential,
            version: 1,
            active: true,
            draft_selected: false,
            created_at: now,
            revoked_at: null
          }]
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
      await route.fulfill({ status: 201, json: { credential_restored: false, provider: currentProvider } });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [revision], next_cursor: null } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}` && request.method() === 'PATCH') {
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
    if (pathname === `/api/v1/providers/${ids.provider}` && request.method() === 'GET') {
      await route.fulfill({ json: currentProvider });
      return;
    }
    await route.fulfill({ status: 404, json: { title: 'Not mocked', status: 404, detail: pathname } });
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(page.getByText('model-page-one-0', { exact: true }).first()).toBeVisible();
  await page.getByLabel('Provider model pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('model-page-two-0', { exact: true }).first()).toBeVisible();

  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(page.getByText('model-page-one-1', { exact: true }).first()).toBeVisible();
  expect(modelCursors.at(-1)).toBeNull();

  await page.getByLabel('Provider model pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('model-page-two-1', { exact: true }).first()).toBeVisible();
  await page.getByPlaceholder('New credential').fill('rotated-write-only-secret');
  await page.getByRole('button', { name: 'Stage rotation' }).click();
  await expect(page.getByText('model-page-one-2', { exact: true }).first()).toBeVisible();
  expect(modelCursors.at(-1)).toBeNull();

  await page.getByLabel('Provider model pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('model-page-two-2', { exact: true }).first()).toBeVisible();
  await page.getByRole('button', { name: 'Restore as draft' }).click();
  await expect(page.getByText('model-page-one-3', { exact: true }).first()).toBeVisible();
  expect(modelCursors.at(-1)).toBeNull();

  await page.getByLabel('Provider model pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('model-page-two-3', { exact: true }).first()).toBeVisible();
  const discoveryRequests = modelCursors.length;
  await page.getByRole('button', { name: 'Run upstream discovery' }).click();
  await expect(page.getByText('model-page-one-4', { exact: true }).first()).toBeVisible();
  expect(modelCursors.slice(discoveryRequests)).not.toContain('opaque-next-model');

  await page.getByLabel('Provider model pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('model-page-two-4', { exact: true }).first()).toBeVisible();
  await page.getByText('Manual model identifiers', { exact: true }).click();
  await page.getByLabel('Upstream model identifiers').fill('manual-model');
  const declarationRequests = modelCursors.length;
  await page.getByRole('button', { name: 'Add identifiers for review' }).click();
  await expect(page.getByText('model-page-one-5', { exact: true }).first()).toBeVisible();
  expect(modelCursors.slice(declarationRequests)).not.toContain('opaque-next-model');

  await page.getByLabel('Provider model pages').getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('model-page-two-5', { exact: true }).first()).toBeVisible();
  const reviewRequests = modelCursors.length;
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).uncheck();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(page.getByText('model-page-two-6', { exact: true }).first()).toBeVisible();
  expect(modelCursors.slice(reviewRequests)).toEqual(['opaque-next-model']);

  const certificationRequests = modelCursors.length;
  await page.getByRole('button', { name: 'Server-certify capabilities' }).click();
  await expect(page.getByText('model-page-two-7', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('2/2 certified', { exact: true })).toBeVisible();
  expect(modelCursors.slice(certificationRequests)).toEqual(['opaque-next-model']);
});

test('provider detail keeps the live revision and credential until a certified draft activates', async ({ page }) => {
  await mockSession(page);
  const nextCredential = '01980000-0000-7000-8000-000000000104';
  let currentProvider = providerRecord('active', [certifiedModelRecord], {
    kind: 'open_ai_compatible',
    endpoint: 'https://models.example.test/v1/',
    active_revision: 1,
    pending_activation: false
  });
  let versions: Array<{
    id: string;
    version: number;
    active: boolean;
    draft_selected: boolean;
    created_at: string;
    revoked_at: string | null;
  }> = [{ id: ids.credential, version: 1, active: true, draft_selected: false, created_at: now, revoked_at: null }];
  let rotatedCredential = '';
  let certificationEtag = '';
  let probeEtag = '';
  let activationEtag = '';
  let revisionRequests = 0;

  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/v1/providers') {
      await route.fulfill({ json: { items: [currentProvider], next_cursor: null } });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      revisionRequests += 1;
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models` && request.method() === 'GET') {
      await route.fulfill({ json: { items: currentProvider.models, next_cursor: null } });
      return;
    }
    if (pathname.endsWith('/credentials') && request.method() === 'GET') {
      await route.fulfill({ json: { items: versions } });
      return;
    }
    if (pathname.endsWith('/credentials') && request.method() === 'POST') {
      rotatedCredential = (request.postDataJSON() as { credential: string }).credential;
      versions = [
        { id: nextCredential, version: 2, active: false, draft_selected: true, created_at: '2026-07-12T12:10:00Z', revoked_at: null },
        { ...versions[0], active: true, draft_selected: false }
      ];
      currentProvider = providerRecord('draft', [{
        ...certifiedModelRecord,
        capabilities: certifiedModelRecord.capabilities.map((capability) => ({ ...capability, source: 'declared', certified_at: null }))
      }], {
        kind: 'open_ai_compatible',
        endpoint: 'https://models.example.test/v1/',
        active_revision: 1,
        pending_activation: true,
        draft_credential_id: nextCredential,
        draft_credential_version: 2,
        runtime_credential_id: ids.credential,
        runtime_credential_version: 1,
        etag: '01980000-0000-7000-8000-000000000201',
        updated_at: '2026-07-12T12:10:00Z',
        last_probe_at: null,
        last_probe_status: null,
        last_probe_detail: null
      });
      await route.fulfill({ status: 201, json: { provider_id: ids.provider, credential_id: nextCredential, credential_version: 2, etag: currentProvider.etag, runtime_generation: null } });
      return;
    }
    if (pathname.endsWith(`/models/${ids.model}/certify`)) {
      certificationEtag = (await request.allHeaders())['if-match'];
      currentProvider = withProviderModels(currentProvider, [certifiedModelRecord], {
        etag: '01980000-0000-7000-8000-000000000202',
        updated_at: '2026-07-12T12:12:00Z'
      });
      await route.fulfill({ json: {
        provider_id: ids.provider,
        model_id: ids.model,
        status: 'succeeded',
        checked_at: '2026-07-12T12:12:00Z',
        certified_count: 2,
        attempted_count: 2,
        results: certifiedModelRecord.capabilities.map((capability) => ({ ...capability, succeeded: true, error_code: null, detail: 'Live tuple certified' }))
      } });
      return;
    }
    if (pathname.endsWith('/probe')) {
      probeEtag = (await request.allHeaders())['if-match'];
      currentProvider = {
        ...currentProvider,
        last_probe_at: '2026-07-12T12:13:00Z',
        last_probe_status: 'succeeded',
        last_probe_detail: 'Compatible endpoint reachable'
      };
      await route.fulfill({ json: { provider_id: ids.provider, succeeded: true, checked_at: '2026-07-12T12:13:00Z', probe_type: 'connector_connectivity', detail: 'Compatible endpoint reachable' } });
      return;
    }
    if (pathname.endsWith('/activate')) {
      activationEtag = (await request.allHeaders())['if-match'];
      currentProvider = providerRecord('active', [certifiedModelRecord], {
        kind: 'open_ai_compatible',
        endpoint: 'https://models.example.test/v1/',
        active_revision: 2,
        pending_activation: false,
        draft_credential_id: nextCredential,
        draft_credential_version: 2,
        runtime_credential_id: nextCredential,
        runtime_credential_version: 2,
        etag: '01980000-0000-7000-8000-000000000203',
        updated_at: '2026-07-12T12:13:00Z',
        last_probe_at: '2026-07-12T12:13:00Z',
        last_probe_status: 'succeeded',
        last_probe_detail: 'Compatible endpoint reachable'
      });
      versions = [
        { id: nextCredential, version: 2, active: true, draft_selected: false, created_at: '2026-07-12T12:10:00Z', revoked_at: null },
        { id: ids.credential, version: 1, active: false, draft_selected: false, created_at: now, revoked_at: '2026-07-12T12:13:00Z' }
      ];
      await route.fulfill({ json: { id: ids.provider, state: 'active', etag: currentProvider.etag, runtime_generation: { id: ids.generation, sequence: 4 } } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    await route.fulfill({ status: 404, json: { title: 'Not mocked', status: 404, detail: pathname } });
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(page.getByRole('heading', { name: 'Credential versions' })).toBeVisible();
  await page.getByPlaceholder('New credential').fill('rotated-write-only-secret');
  await page.getByRole('button', { name: 'Stage rotation' }).click();
  await expect(page.getByText(/Credential version staged/)).toBeVisible();
  await expect(page.getByPlaceholder('New credential')).toHaveValue('');
  await expect(page.getByText('rotated-write-only-secret')).toHaveCount(0);
  expect(rotatedCredential).toBe('rotated-write-only-secret');
  await expect(page.getByText('Revision 1 remains live.')).toBeVisible();
  await expect(page.getByText('revision 1 live · changes pending')).toBeVisible();
  await expect(page.getByText('runtime active', { exact: true })).toBeVisible();
  await expect(page.getByText('pending activation', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Revoke' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Activate changes' })).toBeDisabled();

  await page.getByRole('button', { name: 'Server-certify capabilities' }).click();
  await expect(page.getByText(/reviewed tuples passed server certification/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Activate changes' })).toBeDisabled();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Connection succeeded/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Activate changes' })).toBeEnabled();
  await page.getByRole('button', { name: 'Activate changes' }).click();

  await expect(page.getByText('revision 2 active')).toBeVisible();
  await expect(page.getByText('Revision 1 remains live.')).toHaveCount(0);
  await expect(page.getByText('runtime active', { exact: true })).toBeVisible();
  await expect(page.getByText('pending activation', { exact: true })).toHaveCount(0);
  await expect.poll(() => revisionRequests).toBeGreaterThanOrEqual(2);
  expect(certificationEtag).toBe('01980000-0000-7000-8000-000000000201');
  expect(probeEtag).toBe('01980000-0000-7000-8000-000000000202');
  expect(activationEtag).toBe('01980000-0000-7000-8000-000000000202');
});

test('provider inventory preserves its cursor through detail and wizard navigation', async ({ page }) => {
  test.slow();
  await mockSession(page);
  const firstProvider = providerRecord('active', [modelRecord]);
  const secondProvider = {
    ...firstProvider,
    id: '01980000-0000-7000-8000-000000000105',
    name: 'production-anthropic',
    kind: 'anthropic'
  };
  const seenCursors: Array<string | null> = [];
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    if (pathname === '/api/v1/providers') {
      const cursor = url.searchParams.get('cursor');
      seenCursors.push(cursor);
      await route.fulfill({
        json: cursor === 'opaque-next-provider'
          ? { items: [secondProvider], next_cursor: null }
          : { items: [firstProvider], next_cursor: 'opaque-next-provider' }
      });
      return;
    }
    const detailProvider = pathname.includes(secondProvider.id) ? secondProvider : firstProvider;
    if (pathname === `/api/v1/providers/${detailProvider.id}`) {
      await route.fulfill({ json: detailProvider });
      return;
    }
    if (pathname.endsWith('/models')) {
      await route.fulfill({ json: { items: detailProvider.models, next_cursor: null } });
      return;
    }
    if (pathname.endsWith('/credentials') || pathname.endsWith('/revisions')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    await route.fulfill({
      status: 404,
      json: { title: 'Not mocked', status: 404, detail: pathname }
    });
  });

  await page.goto('/providers');
  await expect(page.getByText('production-openai', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('production-anthropic', { exact: true })).toBeVisible();
  await page.getByRole('link', { name: 'Manage' }).click();
  await expect(page.getByRole('heading', { name: 'Connector context' })).toBeVisible();
  await page.getByRole('link', { name: 'All providers' }).click();
  await expect(page.getByText('production-anthropic', { exact: true })).toBeVisible();
  await page.getByRole('link', { name: 'Add provider' }).click();
  await expect(page.getByRole('heading', { name: 'Connect an upstream provider.' })).toBeVisible();
  await page.getByRole('link', { name: 'Cancel' }).click();
  await expect(page.getByText('production-anthropic', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Previous' }).click();
  await expect(page.getByText('production-openai', { exact: true })).toBeVisible();
  expect(seenCursors).toContain('opaque-next-provider');
});

test('model inventory pages the global catalog and updates through provider ETags', async ({ page }) => {
  await mockSession(page);
  const secondProviderId = '01980000-0000-7000-8000-000000000106';
  const secondModel = {
    ...certifiedModelRecord,
    id: '01980000-0000-7000-8000-000000000107',
    upstream_model: 'claude-sonnet-test',
    display_name: 'Claude Sonnet Test'
  };
  let firstEnabled = true;
  let mutationEtag = '';
  const seenCursors: Array<string | null> = [];

  await page.route('**/api/v1/provider-models**', async (route) => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor');
    seenCursors.push(cursor);
    await route.fulfill({
      json: cursor === 'opaque-next-model'
        ? {
            items: [{
              provider_id: secondProviderId,
              provider_name: 'production-anthropic',
              provider_kind: 'anthropic',
              model: secondModel
            }],
            next_cursor: null
          }
        : {
            items: [{
              provider_id: ids.provider,
              provider_name: 'production-openai',
              provider_kind: 'open_ai',
              model: { ...certifiedModelRecord, enabled: firstEnabled }
            }],
            next_cursor: 'opaque-next-model'
          }
    });
  });
  await page.route('**/api/v1/providers/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === `/api/v1/providers/${ids.provider}` && request.method() === 'GET') {
      await route.fulfill({ json: providerRecord('active', [{ ...certifiedModelRecord, enabled: firstEnabled }]) });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models/${ids.model}` && request.method() === 'PATCH') {
      mutationEtag = (await request.allHeaders())['if-match'];
      firstEnabled = (request.postDataJSON() as { enabled: boolean }).enabled;
      await route.fulfill({ json: providerRecord('draft', [{ ...certifiedModelRecord, enabled: firstEnabled }]) });
      return;
    }
    await route.fulfill({ status: 404, json: { title: 'Not mocked', status: 404, detail: pathname } });
  });

  await page.goto('/models');
  await expect(page.getByText('gpt-5.4', { exact: true }).first()).toBeVisible();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByText('Claude Sonnet Test')).toBeVisible();
  await page.getByRole('button', { name: 'Previous' }).click();
  await page.getByRole('checkbox', { name: 'Enabled' }).uncheck();
  await expect(page.getByRole('checkbox', { name: 'Disabled' })).not.toBeChecked();
  expect(seenCursors).toContain('opaque-next-model');
  expect(mutationEtag).toBe('01980000-0000-7000-8000-000000000109');
});

test('Route Studio creates, simulates, validates, and activates deterministic routing', async ({ page }) => {
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await mockSession(page);
  let routeState = 'draft';
  let createBody: Record<string, unknown> | undefined;
  let createHeaders: Record<string, string> = {};
  let simulationBody: Record<string, unknown> | undefined;

  const routeDraft = () => ({
    id: ids.draft,
    slug: 'default',
    state: routeState,
    overall_timeout_ms: 120000,
    max_attempts: 1,
    etag: '01980000-0000-7000-8000-000000000209',
    based_on_revision_id: null,
    operations: ['generation'],
    targets: [{ id: ids.target, provider_model_id: ids.model, provider_id: ids.provider, provider_name: 'production-openai', provider_model: 'gpt-5.4', priority: 1, weight: 100, timeout_ms: 60000, position: 0 }],
    created_at: now,
    updated_at: now
  });

  await page.route('**/api/v1/provider-models**', async (route) => {
    await route.fulfill({
      json: {
        items: [{
          provider_id: ids.provider,
          provider_name: 'production-openai',
          provider_kind: 'open_ai',
          model: certifiedModelRecord
        }],
        next_cursor: null
      }
    });
  });
  await page.route('**/api/v1/route-drafts**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/v1/route-drafts' && request.method() === 'POST') {
      createBody = request.postDataJSON();
      createHeaders = await request.allHeaders();
      await route.fulfill({ status: 201, json: { id: ids.draft, slug: 'default', state: 'draft', etag: routeDraft().etag } });
      return;
    }
    if (pathname === `/api/v1/route-drafts/${ids.draft}` && request.method() === 'GET') {
      await route.fulfill({ json: routeDraft() });
      return;
    }
    if (pathname.endsWith('/simulate')) {
      simulationBody = request.postDataJSON();
      await route.fulfill({ json: { deterministic_seed: 'setup-preview', operation: 'generation', surface: 'open_ai', mode: 'streaming', targets: [{ target_id: ids.target, provider_id: ids.provider, provider_name: 'production-openai', provider_model: 'gpt-5.4', priority: 1, eligible: true, reason: null, attempt: 1 }] } });
      return;
    }
    if (pathname.endsWith('/validate')) {
      routeState = 'validated';
      await route.fulfill({ json: { id: ids.draft, slug: 'default', state: 'validated', etag: routeDraft().etag } });
      return;
    }
    if (pathname.endsWith('/activate')) {
      await route.fulfill({ json: { route_id: ids.route, revision_id: ids.revision, revision: 1, runtime_generation: { id: ids.generation, sequence: 3 } } });
      return;
    }
    await route.fulfill({ status: 404, json: { title: 'Not mocked', status: 404, detail: pathname } });
  });

  await emulateTwoHundredPercentZoom(page);
  await page.goto('/routes/new');
  await expect(page.getByRole('heading', { name: 'Build a route draft.' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.getByRole('button', { name: 'Add target' }).click();
  await page.getByLabel('Maximum attempts').fill('1');
  await page.getByRole('button', { name: 'Create draft' }).click();
  await expect(page).toHaveURL(new RegExp(`/routes/${ids.draft}$`));
  expect(createBody).toMatchObject({
    slug: 'default',
    max_attempts: 1,
    targets: [{ provider_id: ids.provider, provider_model: 'gpt-5.4', priority: 1, weight: 100 }]
  });
  expect(createHeaders['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  expect(createHeaders['x-csrf-token']).toBe('csrf-e2e');

  await page.getByRole('button', { name: 'Simulate order' }).click();
  await expect(page.getByRole('heading', { name: 'Attempt explanation' })).toBeVisible();
  await expect(page.getByText('Eligible in priority group 1')).toBeVisible();
  expect(simulationBody).toEqual({
    operation: 'generation',
    surface: 'open_ai',
    mode: 'streaming',
    seed: 'setup-preview'
  });
  await page.getByRole('button', { name: 'Validate draft' }).click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page.getByRole('button', { name: 'Activate route' }).click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();
  await expect(page.getByRole('link', { name: 'View revision history' })).toHaveAttribute('href', `/routes/${ids.route}/revisions`);
});

test('API key creation shows a secret once with SDK snippets on mobile', async ({ page }) => {
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await mockSession(page);
  let createBody: Record<string, unknown> | undefined;
  let createHeaders: Record<string, string> = {};
  await page.route('**/api/v1/routes**', async (route) => {
    await route.fulfill({ json: { items: [{
      id: ids.route,
      slug: 'default',
      created_at: now,
      revision_count: 1,
      latest_revision: {
        id: ids.revision, route_id: ids.route, revision: 1, slug: 'default', overall_timeout_ms: 120000,
        max_attempts: 1, source_draft_id: ids.draft, activated_by: ids.user, activated_at: now,
        operations: ['generation'], targets: []
      }
    }], next_cursor: null } });
  });
  await page.route('**/api/v1/api-keys**', async (route) => {
    const request = route.request();
    if (request.method() === 'POST') {
      createBody = request.postDataJSON();
      createHeaders = await request.allHeaders();
      await route.fulfill({ status: 201, json: { id: ids.key, lookup_id: 'olp_live_abcd', secret: 'olp_secret_shown_once', runtime_generation: { id: ids.generation, sequence: 4 } } });
      return;
    }
    await route.fulfill({ json: { items: [], next_cursor: null } });
  });
  await page.route('**/anthropic/v1/messages', async (route) => {
    expect(route.request().headers()['x-api-key']).toBe('olp_secret_shown_once');
    expect(route.request().postDataJSON()).toMatchObject({ model: 'default' });
    await route.fulfill({ json: { id: 'msg_test', type: 'message', role: 'assistant', model: 'default', content: [{ type: 'text', text: 'ok' }], stop_reason: 'end_turn', usage: { input_tokens: 1, output_tokens: 1 } } });
  });

  await emulateTwoHundredPercentZoom(page);
  await page.goto('/api-keys/new');
  await page.getByLabel('Key name').fill('mobile-app');
  await page.getByLabel('Requests per minute').fill('120');
  await page.getByLabel('Concurrent requests').fill('8');
  await page.getByRole('group', { name: 'Allowed route slugs' }).getByRole('checkbox', { name: 'default' }).check();
  await page.getByRole('button', { name: /Create and show key/ }).click();

  const dialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText('olp_secret_shown_once', { exact: true })).toBeVisible();
  await expect(dialog.getByText('base_url=')).toBeVisible();
  expect((await new AxeBuilder({ page }).include('.secret-dialog').analyze()).violations).toEqual([]);
  expect(createBody).toMatchObject({ name: 'mobile-app', allowed_routes: ['default'], requests_per_minute: 120, max_concurrency: 8 });
  expect(createHeaders['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  expect(createHeaders['x-csrf-token']).toBe('csrf-e2e');

  await dialog.getByRole('tab', { name: 'Anthropic TS' }).click();
  await expect(dialog.getByText('client.messages.create')).toBeVisible();
  await dialog.getByRole('button', { name: 'Run connection test' }).click();
  await expect(dialog.getByText('Anthropic request succeeded through route default.')).toBeVisible();
  await dialog.getByRole('tab', { name: 'Gemini TS' }).click();
  await expect(dialog.getByRole('tabpanel')).toContainText('baseUrl: "http://127.0.0.1:4174/gemini"');
  await expect(dialog.getByRole('tabpanel')).toContainText('apiVersion: "v1beta"');
  await dialog.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(page).toHaveURL(/\/api-keys$/);
  await expect(page.getByText('olp_secret_shown_once')).toHaveCount(0);
});

test('API key policy updates, rotation, and revocation converge in the list', async ({ page }) => {
  await mockSession(page);
  page.on('dialog', (dialog) => dialog.accept());
  let revokedAt: string | null = null;
  let keyName = 'production SDK';
  let requestsPerMinute = 120;
  let keyEtag = '01980000-0000-7000-8000-000000000309';
  const keyRecord = () => ({
    id: ids.key,
    lookup_id: 'olp_live_abcd',
    name: keyName,
    scopes: ['inference'],
    allowed_routes: ['default'],
    requests_per_minute: requestsPerMinute,
    tokens_per_minute: null,
    max_concurrency: 8,
    expires_at: null,
    revoked_at: revokedAt,
    rotated_at: null,
    etag: keyEtag,
    created_at: now
  });

  await page.route('**/api/v1/routes**', async (route) => {
    await route.fulfill({ json: { items: [{ id: ids.route, slug: 'default', created_at: now, revision_count: 1, latest_revision: { id: ids.revision, route_id: ids.route, revision: 1, slug: 'default', overall_timeout_ms: 120000, max_attempts: 1, source_draft_id: ids.draft, activated_by: ids.user, activated_at: now, operations: ['generation'], targets: [] } }], next_cursor: null } });
  });

  await page.route('**/api/v1/api-keys**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/rotate')) {
      await route.fulfill({ json: { id: ids.key, lookup_id: 'olp_live_efgh', secret: 'rotated-key-shown-once', etag: keyRecord().etag, runtime_generation: { id: ids.generation, sequence: 5 } } });
      return;
    }
    if (pathname.endsWith('/revoke')) {
      revokedAt = now;
      await route.fulfill({ json: { id: ids.generation, sequence: 6 } });
      return;
    }
    if (request.method() === 'PATCH') {
      const body = request.postDataJSON() as { name: string; requests_per_minute: number };
      keyName = body.name;
      requestsPerMinute = body.requests_per_minute;
      keyEtag = '01980000-0000-7000-8000-000000000310';
      await route.fulfill({ json: { etag: keyEtag, runtime_generation: { id: ids.generation, sequence: 5 } } });
      return;
    }
    await route.fulfill({ json: { items: [keyRecord()], next_cursor: null } });
  });

  await page.goto('/api-keys');
  await page.getByRole('button', { name: 'Edit' }).click();
  await page.getByLabel('Key name').fill('renamed SDK');
  await page.getByLabel('Requests per minute').fill('240');
  await page.getByRole('button', { name: 'Save and publish' }).click();
  await expect(page.getByText('renamed SDK', { exact: true })).toBeVisible();
  await expect(page.getByText(/240 RPM/)).toBeVisible();
  await page.getByRole('button', { name: 'Rotate' }).click();
  const dialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(dialog.getByText('rotated-key-shown-once', { exact: true })).toBeVisible();
  await dialog.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(page.getByText('rotated-key-shown-once')).toHaveCount(0);
  await page.getByRole('button', { name: 'Revoke' }).click();
  await expect(page.getByText('revoked', { exact: true })).toBeVisible();
});

test('route revision diff and restore-as-draft remain explicit', async ({ page }) => {
  await mockSession(page);
  const revision = (id: string, number: number, slug: string) => ({
    id,
    route_id: ids.route,
    revision: number,
    slug,
    overall_timeout_ms: number === 1 ? 120000 : 90000,
    max_attempts: 1,
    source_draft_id: ids.draft,
    activated_by: ids.user,
    activated_at: now,
    operations: ['generation'],
    targets: [{ id: ids.target, provider_model_id: ids.model, provider_id: ids.provider, provider_name: 'production-openai', provider_model: 'gpt-5.4', priority: 1, weight: 100, timeout_ms: 60000, position: 0 }]
  });
  const revisionTwoId = '01980000-0000-7000-8000-000000000206';
  const history = [revision(revisionTwoId, 2, 'default'), revision(ids.revision, 1, 'legacy')];
  let restoreCalled = false;

  await page.route('**/api/v1/routes/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/revisions/diff')) {
      await route.fulfill({ json: { from_revision: 1, to_revision: 2, slug_changed: true, timeout_changed: true, max_attempts_changed: false, operations_added: [], operations_removed: [], targets_added: [], targets_removed: [], targets_changed: [] } });
      return;
    }
    if (pathname.endsWith('/restore-as-draft')) {
      restoreCalled = true;
      await route.fulfill({ status: 201, json: { id: ids.draft, slug: 'default', state: 'draft', overall_timeout_ms: 90000, max_attempts: 1, etag: '01980000-0000-7000-8000-000000000209', based_on_revision_id: revisionTwoId, operations: ['generation'], targets: [], created_at: now, updated_at: now } });
      return;
    }
    await route.fulfill({ json: { items: history } });
  });
  await page.route('**/api/v1/route-drafts/**', async (route) => {
    await route.fulfill({ json: { id: ids.draft, slug: 'default', state: 'draft', overall_timeout_ms: 90000, max_attempts: 1, etag: '01980000-0000-7000-8000-000000000209', based_on_revision_id: revisionTwoId, operations: ['generation'], targets: [], created_at: now, updated_at: now } });
  });
  await page.route('**/api/v1/providers**', async (route) => {
    await route.fulfill({ json: { items: [providerRecord('active', [modelRecord])], next_cursor: null } });
  });

  await page.goto(`/routes/${ids.route}/revisions`);
  await expect(page.getByRole('heading', { name: 'Immutable revisions' })).toBeVisible();
  await page.getByRole('button', { name: 'Compare' }).click();
  await expect(page.getByText('slug, deadline')).toBeVisible();
  const newestRow = page.getByRole('row', { name: /Revision 2/ });
  await newestRow.getByRole('button', { name: 'Restore as draft' }).click();
  await expect(page).toHaveURL(new RegExp(`/routes/${ids.draft}$`));
  expect(restoreCalled).toBe(true);
});

test('team roles, one-time invitations, sessions, and OIDC are API-backed', async ({ page }) => {
  await mockSession(page);
  const members = [
    { id: ids.user, email: 'owner@example.com', display_name: 'Ada Owner', role: 'owner', active: true, etag: '01980000-0000-7000-8000-000000000411', created_at: now, updated_at: now },
    { id: ids.developer, email: 'grace@example.com', display_name: 'Grace Developer', role: 'developer', active: true, etag: '01980000-0000-7000-8000-000000000412', created_at: now, updated_at: now }
  ];
  let inviteCreated = false;
  let oidcSaved = false;
  let oidcBody: Record<string, unknown> | undefined;

  await page.route('**/api/v1/users**', async (route) => {
    const request = route.request();
    if (request.method() === 'PATCH') {
      const body = request.postDataJSON() as { role?: string; active?: boolean };
      const updated = { ...members[1], role: body.role ?? members[1].role, active: body.active ?? members[1].active, etag: '01980000-0000-7000-8000-000000000413' };
      members[1] = updated;
      await route.fulfill({ json: updated });
      return;
    }
    await route.fulfill({ json: { data: members, next_cursor: null } });
  });
  await page.route('**/api/v1/invitations**', async (route) => {
    const request = route.request();
    if (request.method() === 'POST') {
      inviteCreated = true;
      await route.fulfill({ status: 201, json: { invitation: { id: ids.invitation, email: 'new@example.com', role: 'developer', invited_by: ids.user, status: 'pending', expires_at: '2026-07-19T12:00:00Z', created_at: now, accepted_at: null, revoked_at: null }, token: 'invite-token-shown-once' } });
      return;
    }
    await route.fulfill({ json: { data: inviteCreated ? [{ id: ids.invitation, email: 'new@example.com', role: 'developer', invited_by: ids.user, status: 'pending', expires_at: '2026-07-19T12:00:00Z', created_at: now, accepted_at: null, revoked_at: null }] : [], next_cursor: null } });
  });
  await page.route('**/api/v1/sessions?**', async (route) => {
    await route.fulfill({ json: { data: [{ id: ids.session, user_id: ids.user, current: true, created_at: now, last_seen_at: now, expires_at: '2026-07-13T12:00:00Z' }], next_cursor: null } });
  });
  await page.route('**/api/v1/oidc/configuration', async (route) => {
    const request = route.request();
    if (request.method() === 'GET' && !oidcSaved) {
      await route.fulfill({ status: 404, contentType: 'application/problem+json', body: JSON.stringify({ title: 'Not configured', status: 404 }) });
      return;
    }
    if (request.method() === 'PUT') {
      oidcBody = request.postDataJSON();
      oidcSaved = true;
    }
    await route.fulfill({ status: oidcSaved ? 200 : 404, json: { id: ids.oidc, discovery_url: 'https://id.example.com/.well-known/openid-configuration', issuer: 'https://id.example.com', client_id: 'olp-console', has_client_secret: true, enabled: true, scopes: ['openid', 'profile', 'email'], email_claim: 'email', groups_claim: 'groups', default_role: 'viewer', email_role_mappings: [], group_role_mappings: [{ claim_value: 'platform', role: 'operator' }], etag: '01980000-0000-7000-8000-000000000414' } });
  });
  await page.route('**/api/v1/oidc/link', async (route) => {
    await route.fulfill({ json: { authorization_url: '/oidc-test-redirect' } });
  });

  await page.goto('/team');
  await expect(page.getByRole('heading', { name: 'Team & Access' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.getByLabel('Role for Grace Developer').selectOption('operator');
  await expect(page.getByText('Grace Developer is now operator.')).toBeVisible();
  page.once('dialog', (dialog) => dialog.accept());
  const deactivateGrace = page.getByRole('row', { name: /Grace Developer/ }).getByRole('button', { name: 'Deactivate' });
  await deactivateGrace.focus();
  await deactivateGrace.press('Enter');
  await expect(page.getByText('Grace Developer was deactivated and existing sessions were revoked.')).toBeVisible();

  await page.getByRole('button', { name: 'Invitations' }).click();
  await page.getByPlaceholder('person@example.com').fill('new@example.com');
  await page.getByRole('button', { name: 'Create invitation' }).click();
  const invitationDialog = page.getByRole('dialog', { name: 'Copy the invitation link now.' });
  await expect(invitationDialog.getByText('invite-token-shown-once')).toBeVisible();
  await invitationDialog.getByRole('button', { name: 'I have shared it' }).click();
  await expect(page.getByText('invite-token-shown-once')).toHaveCount(0);

  await page.getByRole('button', { name: 'Sessions' }).click();
  await expect(page.getByText(ids.session)).toBeVisible();
  await page.getByRole('button', { name: 'OIDC' }).click();
  await page.getByLabel('Expected issuer').fill('https://id.example.com');
  await page.getByLabel('Discovery URL').fill('https://id.example.com/.well-known/openid-configuration');
  await page.getByLabel('Client ID').fill('olp-console');
  await page.getByLabel('Client secret').fill('oidc-write-only-secret');
  await page.getByLabel('Enabled').check();
  await page.getByLabel('Group mappings').fill('platform=operator');
  await page.getByRole('button', { name: 'Save and validate' }).click();
  await expect(page.getByText('OIDC configuration validated and enabled.')).toBeVisible();
  expect(oidcBody).toMatchObject({ client_secret: 'oidc-write-only-secret', enabled: true, group_role_mappings: [{ claim_value: 'platform', role: 'operator' }] });
  await expect(page.getByLabel('Client secret')).toHaveValue('');
  await page.getByRole('button', { name: 'Link my identity' }).click();
  await expect(page).toHaveURL(/\/oidc-test-redirect$/);
});
