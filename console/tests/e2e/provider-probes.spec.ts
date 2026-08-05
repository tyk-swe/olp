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
  providerRecord,
  sessionOptions,
  withProviderModels
} from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('native provider detail probes the current draft before certification', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let currentProvider = providerRecord('draft', [modelRecord], {
    etag: '01980000-0000-7000-8000-000000000120',
    updated_at: '2026-07-12T12:20:00Z'
  });
  const mutationOrder: string[] = [];
  let probeEtag = '';
  let certificationEtag = '';

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
    if (pathname.endsWith('/probe') && request.method() === 'POST') {
      mutationOrder.push('probe');
      probeEtag = (await request.allHeaders())['if-match'];
      currentProvider = {
        ...currentProvider,
        last_probe_at: '2026-07-12T12:21:00Z',
        last_probe_status: 'succeeded',
        last_probe_detail: 'Credentialed connector request succeeded.'
      };
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          succeeded: true,
          checked_at: currentProvider.last_probe_at,
          probe_type: 'connector_connectivity',
          detail: currentProvider.last_probe_detail
        }
      });
      return;
    }
    if (
      pathname.endsWith(`/models/${ids.model}/certify`) &&
      request.method() === 'POST'
    ) {
      mutationOrder.push('certify');
      certificationEtag = (await request.allHeaders())['if-match'];
      if (!currentProvider.last_probe_at) {
        await route.fulfill({
          status: 422,
          contentType: 'application/problem+json',
          json: {
            title: 'Invalid provider configuration',
            status: 422,
            detail:
              'native capability certification requires a successful credentialed probe of the current provider draft'
          }
        });
        return;
      }
      currentProvider = withProviderModels(
        currentProvider,
        [certifiedModelRecord],
        {
          etag: '01980000-0000-7000-8000-000000000121',
          updated_at: '2026-07-12T12:22:00Z',
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
          checked_at: '2026-07-12T12:22:00Z',
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
    page.getByRole('heading', { name: 'Models and capabilities' })
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Test completed draft' })
  ).toBeDisabled();
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(
    page.getByText(/reviewed tuples passed server certification/)
  ).toBeVisible();
  await expect(page.getByText('2/2 certified', { exact: true })).toBeVisible();
  expect(mutationOrder).toEqual(['probe', 'certify']);
  expect(probeEtag).toBe('"01980000-0000-7000-8000-000000000120"');
  expect(certificationEtag).toBe(probeEtag);
});
