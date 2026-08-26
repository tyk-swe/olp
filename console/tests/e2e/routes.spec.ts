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
  certifiedModelRecord,
  ids,
  now,
  sessionOptions
} from './gateway-access-fixtures';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('Route Studio creates, simulates, validates, and activates deterministic routing', async ({
  page
}) => {
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await mockSession(page, sessionOptions);
  let routeState = 'draft';
  let draftEtag = '01980000-0000-7000-8000-000000000209';
  // Activation returns the draft to `draft` under a new ETag.
  const activatedDraftEtag = '01980000-0000-7000-8000-000000000210';
  let createBody: Record<string, unknown> | undefined;
  let createHeaders: Record<string, string> = {};
  let simulationBody: Record<string, unknown> | undefined;
  let saveHeaders: Record<string, string> = {};

  const routeDraft = () => ({
    id: ids.draft,
    slug: 'default',
    state: routeState,
    overall_timeout_ms: 120000,
    max_attempts: 1,
    etag: draftEtag,
    based_on_revision_id: null,
    operations: ['generation'],
    targets: [
      {
        id: ids.target,
        provider_model_id: ids.model,
        provider_id: ids.provider,
        provider_name: 'production-openai',
        provider_model: 'gpt-5.4',
        priority: 1,
        weight: 100,
        timeout_ms: 60000,
        position: 0
      }
    ],
    created_at: now,
    updated_at: now
  });

  await page.route('**/api/v1/provider-models**', async (route) => {
    await route.fulfill({
      json: {
        items: [
          {
            provider_id: ids.provider,
            provider_name: 'production-openai',
            provider_kind: 'openai',
            model: certifiedModelRecord
          }
        ],
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
      await route.fulfill({
        status: 201,
        json: {
          id: ids.draft,
          slug: 'default',
          state: 'draft',
          etag: routeDraft().etag
        }
      });
      return;
    }
    if (pathname === `/api/v1/route-drafts/${ids.draft}`) {
      if (request.method() === 'PUT') {
        saveHeaders = await request.allHeaders();
        await route.fulfill({ json: routeDraft() });
        return;
      }
      if (request.method() === 'GET') {
        await route.fulfill({ json: routeDraft() });
        return;
      }
    }
    if (pathname.endsWith('/simulate')) {
      simulationBody = request.postDataJSON();
      await route.fulfill({
        json: {
          deterministic_seed: 'setup-preview',
          operation: 'generation',
          surface: 'openai',
          mode: 'streaming',
          targets: [
            {
              target_id: ids.target,
              provider_id: ids.provider,
              provider_name: 'production-openai',
              provider_model: 'gpt-5.4',
              priority: 1,
              eligible: true,
              reason: null,
              attempt: 1
            }
          ]
        }
      });
      return;
    }
    if (pathname.endsWith('/validate')) {
      routeState = 'validated';
      await route.fulfill({
        json: {
          id: ids.draft,
          slug: 'default',
          state: 'validated',
          etag: routeDraft().etag
        }
      });
      return;
    }
    if (pathname.endsWith('/activate')) {
      routeState = 'draft';
      draftEtag = activatedDraftEtag;
      await route.fulfill({
        json: {
          route_id: ids.route,
          revision_id: ids.revision,
          revision: 1,
          draft_etag: activatedDraftEtag,
          runtime_generation: { id: ids.generation, sequence: 3 }
        }
      });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await emulateTwoHundredPercentZoom(page);
  await page.goto('/routes/new');
  await expect(
    page.getByRole('heading', { name: 'Build a route draft.' })
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.getByRole('button', { name: 'Add target' }).click();
  await page.getByLabel('Maximum attempts').fill('1');
  await page.getByRole('button', { name: 'Create draft' }).click();
  await expect(page).toHaveURL(new RegExp(`/routes/${ids.draft}$`));
  await expect(page.getByText(/^Created /)).toBeVisible();
  expect(createBody).toMatchObject({
    slug: 'default',
    max_attempts: 1,
    targets: [
      {
        provider_id: ids.provider,
        provider_model: 'gpt-5.4',
        priority: 1,
        weight: 100
      }
    ]
  });
  expect(createHeaders['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  expect(createHeaders['x-csrf-token']).toBe('csrf-e2e');

  await page.getByRole('button', { name: 'Simulate order' }).click();
  await expect(
    page.getByRole('heading', { name: 'Attempt explanation' })
  ).toBeVisible();
  await expect(page.getByText('Eligible in priority group 1')).toBeVisible();
  expect(simulationBody).toEqual({
    operation: 'generation',
    surface: 'openai',
    mode: 'streaming',
    seed: 'setup-preview'
  });
  await page.getByRole('button', { name: 'Validate draft' }).click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page.getByRole('button', { name: 'Activate route' }).click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();
  await expect(
    page.getByRole('link', { name: 'View revision history' })
  ).toHaveAttribute('href', `/routes/${ids.route}/revisions`);
  // The next save has to use the ETag activation handed back, not the one the
  // draft was loaded with.
  await page.getByLabel('Public model slug').fill('default-v2');
  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(page.getByText('Draft saved.')).toBeVisible();
  expect(saveHeaders['if-match']).toBe(`"${activatedDraftEtag}"`);
});

test('failed route conflict reload preserves dirty fields until a successful reload', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  let reloadFailuresRemaining = 0;
  let current = {
    id: ids.draft,
    slug: 'default',
    state: 'draft',
    overall_timeout_ms: 120000,
    max_attempts: 1,
    etag: '01980000-0000-7000-8000-000000000211',
    based_on_revision_id: null,
    operations: ['generation'],
    targets: [
      {
        id: ids.target,
        provider_model_id: ids.model,
        provider_id: ids.provider,
        provider_name: 'production-openai',
        provider_model: 'gpt-5.4',
        priority: 1,
        weight: 100,
        timeout_ms: 60000,
        position: 0
      }
    ],
    created_at: now,
    updated_at: now
  };

  await page.route('**/api/v1/provider-models**', async (route) => {
    await route.fulfill({
      json: {
        items: [
          {
            provider_id: ids.provider,
            provider_name: 'production-openai',
            provider_kind: 'openai',
            model: certifiedModelRecord
          }
        ],
        next_cursor: null
      }
    });
  });
  await page.route(`**/api/v1/route-drafts/${ids.draft}`, async (route) => {
    const request = route.request();
    if (request.method() === 'GET' && reloadFailuresRemaining > 0) {
      reloadFailuresRemaining -= 1;
      await route.fulfill({
        status: 503,
        json: { title: 'Route reload unavailable', status: 503 }
      });
      return;
    }
    if (request.method() === 'PUT') {
      current = {
        ...current,
        slug: 'remote-route',
        etag: '01980000-0000-7000-8000-000000000212'
      };
      reloadFailuresRemaining = 2;
      await route.fulfill({
        status: 412,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          type: 'https://openllmproxy.dev/problems/etag_mismatch',
          title: 'The route changed elsewhere',
          status: 412
        })
      });
      return;
    }
    await route.fulfill({ json: current });
  });

  await page.goto(`/routes/${ids.draft}`);
  await page.getByLabel('Public model slug').fill('local-route');
  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(page.getByRole('alert')).toContainText(
    'This item changed elsewhere.'
  );

  await page.getByRole('button', { name: 'Reload' }).click();
  await expect.poll(() => reloadFailuresRemaining).toBe(0);
  await expect(page.getByText('Route reload unavailable')).toBeVisible();
  await expect(page.getByLabel('Public model slug')).toHaveValue('local-route');
  await page.getByRole('button', { name: 'Reload' }).click();
  await expect(page.getByLabel('Public model slug')).toHaveValue(
    'remote-route'
  );
});
