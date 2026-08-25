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

test('provider discovery advances its ETag without dropping dirty connector edits', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  const currentModel = { ...modelRecord };
  let current = providerRecord('draft', [currentModel], {
    etag: '01980000-0000-7000-8000-000000000501'
  });
  let discoveryEtag = '';
  let saveEtag = '';

  await page.route(
    '**/api/v1/provider-kinds/openai/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'streaming' },
            { operation: 'generation', surface: 'openai', mode: 'unary' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === `/api/v1/providers/${ids.provider}/discovery`) {
      discoveryEtag = (await request.allHeaders())['if-match'];
      current = {
        ...current,
        etag: '01980000-0000-7000-8000-000000000502'
      };
      await route.fulfill({ json: current });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'PATCH'
    ) {
      saveEtag = (await request.allHeaders())['if-match'];
      current = {
        ...current,
        name: (request.postDataJSON() as { name: string }).name,
        etag: '01980000-0000-7000-8000-000000000503'
      };
      await route.fulfill({ json: current });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: current });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({
        json: { items: [currentModel], next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/credentials') || pathname.endsWith('/revisions')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await page.getByLabel('Name').fill('local-provider-name');
  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByRole('button', { name: 'Run upstream discovery' }).click();
  await expect(
    page.getByText('1 model discovered. Review capabilities before activation.')
  ).toBeVisible();
  expect(discoveryEtag).toBe('"01980000-0000-7000-8000-000000000501"');
  await expect(page.getByLabel('Name')).toHaveValue('local-provider-name');
  await expect(page.getByLabel('Mode 1')).toHaveValue('unary');
  const saveDraft = page.getByRole('button', { name: 'Save draft' });
  await expect(saveDraft).toBeEnabled();
  await saveDraft.click();
  await expect(page.getByText('Provider draft settings saved.')).toBeVisible();
  expect(saveEtag).toBe('"01980000-0000-7000-8000-000000000502"');
});

test('provider refetch failures keep dirty connector and capability forms mounted', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  const currentModel = { ...certifiedModelRecord };
  let current = providerRecord('draft', [currentModel], {
    etag: '01980000-0000-7000-8000-000000000521'
  });
  let failModelPage = false;
  let failProviderRefetch = false;
  let providerReads = 0;

  await page.route(
    '**/api/v1/provider-kinds/openai/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'streaming' },
            { operation: 'generation', surface: 'openai', mode: 'unary' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === `/api/v1/providers/${ids.provider}/probe`) {
      current = {
        ...current,
        etag: '01980000-0000-7000-8000-000000000522',
        last_probe_at: now,
        last_probe_status: 'succeeded',
        last_probe_detail: 'OpenAI reachable'
      };
      failModelPage = true;
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          succeeded: true,
          checked_at: now,
          probe_type: 'connector_connectivity',
          detail: 'OpenAI reachable'
        }
      });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/activate`) {
      failProviderRefetch = true;
      await route.fulfill({ json: { runtime_generation: { sequence: 2 } } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      if (failModelPage) {
        await route.fulfill({
          status: 503,
          json: { title: 'Model page refresh unavailable', status: 503 }
        });
      } else {
        await route.fulfill({
          json: { items: [currentModel], next_cursor: null }
        });
      }
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      providerReads += 1;
      if (failProviderRefetch) {
        await route.fulfill({
          status: 503,
          json: { title: 'Provider refresh unavailable', status: 503 }
        });
      } else {
        await route.fulfill({ json: current });
      }
      return;
    }
    if (pathname.endsWith('/credentials') || pathname.endsWith('/revisions')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await page.getByLabel('Name').fill('local-provider-name');
  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(
    page.getByText('The last loaded model page remains available below.')
  ).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('local-provider-name');
  await expect(page.getByLabel('Mode 1')).toHaveValue('unary');

  const readsBeforeActivation = providerReads;
  await page.getByRole('button', { name: 'Activate changes' }).click();
  await expect.poll(() => providerReads).toBeGreaterThan(readsBeforeActivation);
  await expect(
    page.getByText('The last loaded provider remains available below.')
  ).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('local-provider-name');
  await expect(page.getByLabel('Mode 1')).toHaveValue('unary');
});

test('provider capability conflict reloads the row and retries from the remote ETag', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let currentModel = { ...modelRecord };
  let current = providerRecord('draft', [currentModel], {
    etag: '01980000-0000-7000-8000-000000000511'
  });
  const saveEtags: string[] = [];

  await page.route(
    '**/api/v1/provider-kinds/openai/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'streaming' },
            { operation: 'generation', surface: 'openai', mode: 'unary' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers/**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (
      pathname === `/api/v1/providers/${ids.provider}/models/${ids.model}` &&
      request.method() === 'PATCH'
    ) {
      saveEtags.push((await request.allHeaders())['if-match']);
      if (saveEtags.length === 1) {
        currentModel = {
          ...currentModel,
          capabilities: [modelRecord.capabilities[1]]
        };
        current = providerRecord('draft', [currentModel], {
          etag: '01980000-0000-7000-8000-000000000512',
          name: 'remote-provider-name'
        });
        await route.fulfill({
          status: 412,
          contentType: 'application/problem+json',
          body: JSON.stringify({
            type: 'https://openllmproxy.dev/problems/etag_mismatch',
            title: 'The provider changed elsewhere',
            status: 412
          })
        });
        return;
      }
      const input = request.postDataJSON() as {
        enabled: boolean;
        capabilities: typeof modelRecord.capabilities;
      };
      currentModel = { ...currentModel, ...input };
      current = providerRecord('draft', [currentModel], {
        etag: '01980000-0000-7000-8000-000000000513'
      });
      await route.fulfill({ json: current });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: current });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({
        json: { items: [currentModel], next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/credentials') || pathname.endsWith('/revisions')) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await page.getByLabel('Name').fill('local-provider-name');
  await page.getByRole('button', { name: 'Remove capability 2' }).click();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(page.getByRole('alert')).toContainText(
    'This item changed elsewhere.'
  );
  await page.getByRole('button', { name: 'Reload' }).click();
  await expect(page.getByLabel('Name')).toHaveValue('remote-provider-name');
  await expect(page.getByLabel('Mode 1')).toHaveValue('unary');
  await expect(
    page.getByRole('button', { name: 'Remove capability 2' })
  ).toHaveCount(0);
  await expect(page.getByText('This item changed elsewhere.')).toHaveCount(0);
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('Capability review saved with declared provenance.')
  ).toBeVisible();
  expect(saveEtags).toEqual([
    '"01980000-0000-7000-8000-000000000511"',
    '"01980000-0000-7000-8000-000000000512"'
  ]);
});

test('provider wizard recovers a capability save after an ETag conflict', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let currentModel = {
    ...modelRecord,
    capabilities: [modelRecord.capabilities[0]]
  };
  let current = providerRecord('draft', [], {
    etag: '01980000-0000-7000-8000-000000000601'
  });
  const saveEtags: string[] = [];

  await page.route(
    '**/api/v1/provider-kinds/openai/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'streaming' },
            { operation: 'generation', surface: 'openai', mode: 'unary' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/v1/providers' && request.method() === 'POST') {
      await route.fulfill({ status: 201, json: { id: ids.provider } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/probe`) {
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          succeeded: true,
          checked_at: now,
          probe_type: 'connector_connectivity',
          detail: 'OpenAI reachable'
        }
      });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/discovery`) {
      current = providerRecord('draft', [currentModel], {
        etag: '01980000-0000-7000-8000-000000000602'
      });
      await route.fulfill({ json: current });
      return;
    }
    if (
      pathname === `/api/v1/providers/${ids.provider}/models/${ids.model}` &&
      request.method() === 'PATCH'
    ) {
      saveEtags.push((await request.allHeaders())['if-match']);
      if (saveEtags.length === 1) {
        current = providerRecord('draft', [currentModel], {
          etag: '01980000-0000-7000-8000-000000000603'
        });
        await route.fulfill({
          status: 412,
          contentType: 'application/problem+json',
          body: JSON.stringify({
            type: 'https://openllmproxy.dev/problems/etag_mismatch',
            title: 'The provider changed elsewhere',
            status: 412
          })
        });
        return;
      }
      const input = request.postDataJSON() as {
        enabled: boolean;
        capabilities: typeof modelRecord.capabilities;
      };
      currentModel = { ...currentModel, ...input };
      current = providerRecord('draft', [currentModel], {
        etag: '01980000-0000-7000-8000-000000000604'
      });
      await route.fulfill({ json: current });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({
        json: { items: [currentModel], next_cursor: null }
      });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: current });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto('/providers/new');
  await page.getByLabel('Provider name').fill('wizard-provider');
  await page.getByLabel('Credential').fill('write-only-provider-key');
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  await page.getByRole('button', { name: 'Test connection' }).click();
  await page.getByRole('button', { name: 'Discover upstream models' }).click();
  await expect(
    page.getByRole('heading', { name: 'Review model capabilities' })
  ).toBeVisible();

  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(page.getByRole('alert')).toContainText(
    'This item changed elsewhere.'
  );
  await page.getByRole('button', { name: 'Reload' }).click();
  await expect(page.getByLabel('Mode 1')).toHaveValue('streaming');
  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('Capability review saved with declared provenance.')
  ).toBeVisible();
  expect(saveEtags).toEqual([
    '"01980000-0000-7000-8000-000000000602"',
    '"01980000-0000-7000-8000-000000000603"'
  ]);
});
