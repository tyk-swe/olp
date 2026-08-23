import AxeBuilder from '@axe-core/playwright';
import {
  emulateTwoHundredPercentZoom,
  expect,
  failUnexpectedApiRequest,
  mockSession,
  test
} from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import {
  ids,
  modelRecord,
  providerRecord,
  sessionOptions,
  withProviderModels
} from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test(
  'compatible preset resolves to persisted connector fields',
  { tag: '@browser' },
  async ({ page }) => {
    await mockSession(page, sessionOptions);
    let createBody: Record<string, unknown> | undefined;
    const currentProvider = providerRecord('draft', [], {
      name: 'groq-production',
      kind: 'openai_compatible',
      endpoint: 'https://api.groq.com/openai/v1'
    });

    await page.route(
      '**/api/v1/provider-kinds/openai_compatible/capabilities',
      async (route) => {
        await route.fulfill({
          json: { provider_kind: 'openai_compatible', capabilities: [] }
        });
      }
    );
    await page.route('**/api/v1/providers**', async (route) => {
      const request = route.request();
      const pathname = new URL(request.url()).pathname;
      if (pathname === '/api/v1/providers' && request.method() === 'POST') {
        createBody = request.postDataJSON();
        await route.fulfill({ status: 201, json: { id: ids.provider } });
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

    await page.goto('/providers/new');
    await page.getByRole('radio', { name: /OpenAI-compatible/ }).check();
    const endpoint = page.getByLabel('Compatible endpoint');
    await expect(page.getByLabel('Compatible provider')).toHaveValue('');
    await expect(endpoint).toBeEditable();

    await page.getByLabel('Compatible provider').selectOption('groq');
    const presetEndpoint = page.getByLabel('Preset endpoint');
    await expect(presetEndpoint).toHaveValue('https://api.groq.com/openai/v1');
    await expect(presetEndpoint).not.toBeEditable();
    await expect(page.getByText(/Verified against/)).toContainText(
      'OpenAI Compatibility'
    );

    await page.getByRole('radio', { name: /Azure OpenAI/ }).check();
    await page
      .getByLabel('Azure resource endpoint')
      .fill('https://resource.openai.azure.com');
    await page.getByRole('radio', { name: /OpenAI-compatible/ }).check();
    await expect(page.getByLabel('Compatible provider')).toHaveValue('');
    await expect(page.getByLabel('Compatible endpoint')).toHaveValue(
      'https://resource.openai.azure.com'
    );
    await expect(page.getByLabel('Compatible endpoint')).toBeEditable();
    await expect(page.getByText(/Verified against/)).toHaveCount(0);

    await page.getByLabel('Compatible provider').selectOption('');
    await expect(page.getByLabel('Compatible endpoint')).toHaveValue('');
    await expect(page.getByLabel('Compatible endpoint')).toBeEditable();
    await page.getByLabel('Compatible provider').selectOption('groq');
    await page.getByLabel('Provider name').fill('groq-production');
    await page.getByLabel('Credential', { exact: true }).fill('gsk-secret');
    await page
      .getByRole('button', { name: /Save and test connection/ })
      .click();

    await expect(
      page.getByRole('heading', { name: 'Verify upstream reachability' })
    ).toBeVisible();
    expect(createBody).toMatchObject({
      name: 'groq-production',
      kind: 'openai_compatible',
      endpoint: 'https://api.groq.com/openai/v1',
      auth_mode: 'api_key',
      credential: 'gsk-secret'
    });
    expect(createBody).not.toHaveProperty('preset_id');
  }
);

test(
  'provider wizard keeps the write-only secret out of subsequent steps',
  { tag: '@browser' },
  async ({ page }) => {
    await page.emulateMedia({
      forcedColors: 'active',
      reducedMotion: 'reduce'
    });
    await mockSession(page, sessionOptions);
    let currentProvider = providerRecord();
    let createBody: Record<string, unknown> | undefined;
    let createHeaders: Record<string, string> = {};
    const probeEtags: string[] = [];
    let certificationEtag = '';
    let certificationAttempts = 0;

    await page.route(
      '**/api/v1/provider-kinds/openai/capabilities',
      async (route) => {
        await route.fulfill({
          json: {
            provider_kind: 'openai',
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
      const url = new URL(request.url());
      const pathname = url.pathname;
      if (pathname === '/api/v1/providers' && request.method() === 'POST') {
        createBody = request.postDataJSON();
        createHeaders = await request.allHeaders();
        await route.fulfill({
          status: 201,
          json: {
            id: ids.provider,
            name: 'production-openai',
            kind: 'openai',
            state: 'draft',
            model: 'gpt-5.4',
            etag: currentProvider.etag
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
      if (
        pathname === `/api/v1/providers/${ids.provider}/models` &&
        request.method() === 'GET'
      ) {
        await route.fulfill({
          json: { items: currentProvider.models, next_cursor: null }
        });
        return;
      }
      if (pathname.endsWith('/probe')) {
        const headers = await request.allHeaders();
        probeEtags.push(headers['if-match']);
        const checkedAt = currentProvider.models.length
          ? '2026-07-12T12:04:00Z'
          : '2026-07-12T12:00:30Z';
        currentProvider = {
          ...currentProvider,
          last_probe_at: checkedAt,
          last_probe_status: 'succeeded',
          last_probe_detail: 'OpenAI reachable'
        };
        await route.fulfill({
          json: {
            provider_id: ids.provider,
            succeeded: true,
            checked_at: checkedAt,
            probe_type: 'connector_connectivity',
            detail: 'OpenAI reachable'
          }
        });
        return;
      }
      if (pathname.endsWith('/discovery')) {
        currentProvider = providerRecord(
          'draft',
          [{ ...modelRecord, enabled: false, capabilities: [] }],
          {
            etag: '01980000-0000-7000-8000-000000000110',
            updated_at: '2026-07-12T12:01:00Z'
          }
        );
        await route.fulfill({ json: currentProvider });
        return;
      }
      if (pathname.endsWith(`/models/${ids.model}`)) {
        const body = request.postDataJSON() as {
          enabled: boolean;
          capabilities: Array<{
            operation: string;
            surface: string;
            mode: string;
          }>;
        };
        currentProvider = providerRecord(
          'draft',
          [
            {
              ...modelRecord,
              enabled: body.enabled,
              capabilities: body.capabilities.map((capability) => ({
                ...capability,
                source: 'declared',
                certified_at: null
              }))
            }
          ],
          {
            etag: '01980000-0000-7000-8000-000000000111',
            updated_at: '2026-07-12T12:02:00Z'
          }
        );
        await route.fulfill({ json: currentProvider });
        return;
      }
      if (pathname.endsWith(`/models/${ids.model}/certify`)) {
        const headers = await request.allHeaders();
        certificationEtag = headers['if-match'];
        certificationAttempts += 1;
        if (certificationAttempts === 1) {
          await route.fulfill({
            status: 503,
            contentType: 'application/problem+json',
            json: {
              title: 'Certification temporarily unavailable',
              status: 503,
              detail: 'Certification temporarily unavailable.'
            }
          });
          return;
        }
        const capabilities = (
          currentProvider.models[0].capabilities as Array<
            Record<string, unknown>
          >
        ).map((capability) => ({
          ...capability,
          source: 'certified',
          certified_at: '2026-07-12T12:03:00Z'
        }));
        currentProvider = withProviderModels(
          currentProvider,
          [{ ...currentProvider.models[0], capabilities }],
          {
            etag: '01980000-0000-7000-8000-000000000112',
            updated_at: '2026-07-12T12:03:00Z',
            last_probe_at: null,
            last_probe_status: null,
            last_probe_detail: null
          }
        );
        await route.fulfill({
          json: {
            provider_id: ids.provider,
            model_id: ids.model,
            status: 'succeeded',
            checked_at: '2026-07-12T12:03:00Z',
            certified_count: capabilities.length,
            attempted_count: capabilities.length,
            results: capabilities.map((capability) => ({
              ...capability,
              succeeded: true,
              error_code: null,
              detail: 'Certified by server'
            }))
          }
        });
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
        await route.fulfill({
          json: {
            id: ids.provider,
            state: 'active',
            etag: currentProvider.etag,
            runtime_generation: { id: ids.generation, sequence: 2 }
          }
        });
        return;
      }
      failUnexpectedApiRequest(route);
    });

    await emulateTwoHundredPercentZoom(page);
    await page.goto('/providers/new');
    await expect(
      page.getByRole('heading', { name: 'Connect an upstream provider.' })
    ).toBeVisible();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

    await page.getByLabel('Provider name').fill('production-openai');
    await page.getByLabel('Seed model (optional)').fill('gpt-5.4');
    await page
      .getByLabel('Credential', { exact: true })
      .fill('sk-upstream-secret');
    await page
      .getByRole('button', { name: /Save and test connection/ })
      .click();

    await expect(
      page.getByRole('heading', { name: 'Verify upstream reachability' })
    ).toBeVisible();
    await expect(page.getByText('sk-upstream-secret')).toHaveCount(0);
    expect(createBody).toMatchObject({
      name: 'production-openai',
      kind: 'openai',
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
    await expect(
      page.getByRole('heading', { name: 'Discover upstream models' })
    ).toBeVisible();
    await page
      .getByRole('button', { name: 'Discover upstream models' })
      .click();
    await expect(
      page.getByRole('heading', { name: 'Review model capabilities' })
    ).toBeVisible();
    await page.getByRole('button', { name: 'Add capability' }).click();
    await expect(
      page.getByLabel('Operation 1').locator('option[value="model_list"]')
    ).toHaveCount(0);
    await expect(
      page.getByLabel('Operation 1').locator('option[value="model_get"]')
    ).toHaveCount(0);
    await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
    await page.getByRole('button', { name: 'Save capability review' }).click();
    await expect(
      page.getByText('Capability review saved with declared provenance.')
    ).toBeVisible();
    await expect(
      page.getByRole('button', { name: 'Activate provider' })
    ).toBeDisabled();
    await page
      .getByRole('button', { name: 'Server-certify capabilities' })
      .click();
    await expect(page.getByRole('alert')).toContainText(
      'Certification temporarily unavailable.'
    );
    await page
      .getByRole('button', { name: 'Server-certify capabilities' })
      .click();
    await expect(
      page.getByText(/reviewed tuples passed server certification/)
    ).toBeVisible();
    await expect(
      page.getByRole('button', { name: 'Activate provider' })
    ).toBeDisabled();
    await page.getByRole('button', { name: 'Test completed draft' }).click();
    await expect(page.getByText(/Final draft test passed/)).toBeVisible();
    await expect(
      page.getByRole('button', { name: 'Activate provider' })
    ).toBeEnabled();
    await page.getByRole('button', { name: 'Activate provider' }).click();
    await expect(
      page.getByRole('heading', { name: 'Now build a stable route slug.' })
    ).toBeVisible();
    await expect(
      page.getByText('Provider activated in runtime generation 2.')
    ).toBeVisible();
    expect(certificationAttempts).toBe(2);
    expect(certificationEtag).toBe('"01980000-0000-7000-8000-000000000111"');
    expect(probeEtags).toEqual([
      '"01980000-0000-7000-8000-000000000109"',
      '"01980000-0000-7000-8000-000000000111"',
      '"01980000-0000-7000-8000-000000000112"'
    ]);
  }
);
