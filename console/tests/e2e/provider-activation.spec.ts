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
  now,
  providerRecord,
  sessionOptions,
  withProviderModels
} from './gateway-access-fixtures';

// Mirrors DISABLED_EDIT_NOTE in providerEditor.ts.
const DISABLED_EDIT_NOTE =
  'This provider is disabled. Restore it as a draft to change configuration, rotate credentials, or review models again.';

test.beforeEach(async ({ page }) => mockProviderKinds(page));

test('provider detail keeps the live revision and credential until a certified draft activates', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  const nextCredential = '01980000-0000-7000-8000-000000000104';
  let currentProvider = providerRecord('active', [certifiedModelRecord], {
    kind: 'openai_compatible',
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
  }> = [
    {
      id: ids.credential,
      version: 1,
      active: true,
      draft_selected: false,
      created_at: now,
      revoked_at: null
    }
  ];
  let rotatedCredential = '';
  let certificationEtag = '';
  let probeEtag = '';
  let activationEtag = '';
  let revisionRequests = 0;

  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/v1/providers') {
      await route.fulfill({
        json: { items: [currentProvider], next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      revisionRequests += 1;
      await route.fulfill({ json: { items: [], next_cursor: null } });
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
      await route.fulfill({ json: { items: versions } });
      return;
    }
    if (pathname.endsWith('/credentials') && request.method() === 'POST') {
      rotatedCredential = (request.postDataJSON() as { credential: string })
        .credential;
      versions = [
        {
          id: nextCredential,
          version: 2,
          active: false,
          draft_selected: true,
          created_at: '2026-07-12T12:10:00Z',
          revoked_at: null
        },
        { ...versions[0], active: true, draft_selected: false }
      ];
      currentProvider = providerRecord(
        'draft',
        [
          {
            ...certifiedModelRecord,
            capabilities: certifiedModelRecord.capabilities.map(
              (capability) => ({
                ...capability,
                source: 'declared',
                certified_at: null
              })
            )
          }
        ],
        {
          kind: 'openai_compatible',
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
        }
      );
      await route.fulfill({
        status: 201,
        json: {
          provider_id: ids.provider,
          credential_id: nextCredential,
          credential_version: 2,
          etag: currentProvider.etag,
          runtime_generation: null
        }
      });
      return;
    }
    if (pathname.endsWith(`/models/${ids.model}/certify`)) {
      certificationEtag = (await request.allHeaders())['if-match'];
      currentProvider = withProviderModels(
        currentProvider,
        [certifiedModelRecord],
        {
          etag: '01980000-0000-7000-8000-000000000202',
          updated_at: '2026-07-12T12:12:00Z'
        }
      );
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          model_id: ids.model,
          status: 'succeeded',
          checked_at: '2026-07-12T12:12:00Z',
          certified_count: 2,
          attempted_count: 2,
          results: certifiedModelRecord.capabilities.map((capability) => ({
            ...capability,
            succeeded: true,
            error_code: null,
            detail: 'Live tuple certified'
          }))
        }
      });
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
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          succeeded: true,
          checked_at: '2026-07-12T12:13:00Z',
          probe_type: 'connector_connectivity',
          detail: 'Compatible endpoint reachable'
        }
      });
      return;
    }
    if (pathname.endsWith('/activate')) {
      activationEtag = (await request.allHeaders())['if-match'];
      currentProvider = providerRecord('active', [certifiedModelRecord], {
        kind: 'openai_compatible',
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
        {
          id: nextCredential,
          version: 2,
          active: true,
          draft_selected: false,
          created_at: '2026-07-12T12:10:00Z',
          revoked_at: null
        },
        {
          id: ids.credential,
          version: 1,
          active: false,
          draft_selected: false,
          created_at: now,
          revoked_at: '2026-07-12T12:13:00Z'
        }
      ];
      await route.fulfill({
        json: {
          id: ids.provider,
          state: 'active',
          etag: currentProvider.etag,
          runtime_generation: { id: ids.generation, sequence: 4 }
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
  await expect(
    page.getByRole('heading', { name: 'Credential versions' })
  ).toBeVisible();
  await page
    .getByPlaceholder('New credential')
    .fill('rotated-write-only-secret');
  await page.getByRole('button', { name: 'Stage rotation' }).click();
  await expect(page.getByText(/Credential version staged/)).toBeVisible();
  await expect(page.getByPlaceholder('New credential')).toHaveValue('');
  await expect(page.getByText('rotated-write-only-secret')).toHaveCount(0);
  expect(rotatedCredential).toBe('rotated-write-only-secret');
  await expect(page.getByText('Revision 1 remains live.')).toBeVisible();
  await expect(
    page.getByText('revision 1 live · changes pending')
  ).toBeVisible();
  await expect(page.getByText('runtime active', { exact: true })).toBeVisible();
  await expect(
    page.getByText('pending activation', { exact: true })
  ).toBeVisible();
  await expect(page.getByRole('button', { name: 'Revoke' })).toHaveCount(0);
  await expect(
    page.getByRole('button', { name: 'Activate changes' })
  ).toBeDisabled();

  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(
    page.getByText(/reviewed tuples passed server certification/)
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Activate changes' })
  ).toBeDisabled();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Connection succeeded/)).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Activate changes' })
  ).toBeEnabled();
  await page.getByRole('button', { name: 'Activate changes' }).click();

  await expect(page.getByText('revision 2 active')).toBeVisible();
  await expect(page.getByText('Revision 1 remains live.')).toHaveCount(0);
  await expect(page.getByText('runtime active', { exact: true })).toBeVisible();
  await expect(
    page.getByText('pending activation', { exact: true })
  ).toHaveCount(0);
  await expect.poll(() => revisionRequests).toBeGreaterThanOrEqual(2);
  expect(certificationEtag).toBe('"01980000-0000-7000-8000-000000000201"');
  expect(probeEtag).toBe('"01980000-0000-7000-8000-000000000202"');
  expect(activationEtag).toBe('"01980000-0000-7000-8000-000000000202"');
});

test('provider detail disables an active provider and restores it as a draft', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  page.on('dialog', (dialog) => dialog.accept());
  const disabledEtag = '01980000-0000-7000-8000-000000000301';
  const draftEtag = '01980000-0000-7000-8000-000000000302';
  let currentProvider = providerRecord('active', [certifiedModelRecord], {
    active_revision: 1,
    pending_activation: false
  });
  let disableAttempts = 0;
  let disableHeaders: Record<string, string> = {};
  let restoreHeaders: Record<string, string> = {};

  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/v1/providers') {
      await route.fulfill({
        json: { items: [currentProvider], next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [], next_cursor: null } });
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
    if (
      pathname === `/api/v1/providers/${ids.provider}` &&
      request.method() === 'PATCH'
    ) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname.endsWith('/disable')) {
      disableAttempts += 1;
      disableHeaders = await request.allHeaders();
      if (disableAttempts === 1) {
        await route.fulfill({
          status: 409,
          contentType: 'application/problem+json',
          body: JSON.stringify({
            type: 'https://openllmproxy.dev/problems/configuration_resource_in_use',
            title: 'Conflict',
            status: 409,
            detail:
              'The resource is active or referenced and cannot be removed.'
          })
        });
        return;
      }
      currentProvider = providerRecord('disabled', [certifiedModelRecord], {
        active_revision: null,
        pending_activation: false,
        etag: disabledEtag,
        updated_at: '2026-07-12T12:20:00Z'
      });
      await route.fulfill({
        json: {
          provider_id: ids.provider,
          etag: disabledEtag,
          credential_id: null,
          credential_version: null,
          runtime_generation: { id: ids.generation, sequence: 9 }
        }
      });
      return;
    }
    if (pathname.endsWith('/restore-as-draft')) {
      restoreHeaders = await request.allHeaders();
      currentProvider = providerRecord('draft', [certifiedModelRecord], {
        active_revision: null,
        pending_activation: false,
        etag: draftEtag,
        updated_at: '2026-07-12T12:21:00Z'
      });
      await route.fulfill({ json: currentProvider });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: currentProvider });
      return;
    }
    failUnexpectedApiRequest(route);
  });

  await page.goto(`/providers/${ids.provider}`);
  await expect(page.getByText('revision 1 active')).toBeVisible();

  await page.getByRole('button', { name: 'Disable provider' }).click();
  await expect(
    page.getByText(/The resource is active or referenced/)
  ).toBeVisible();
  await expect(page.getByText(/Retarget every route/)).toBeVisible();
  await expect(page.getByText('revision 1 active')).toBeVisible();

  // Any following action clears the conflict, so its success banner never
  // renders beside the red one the failed disable left behind.
  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(page.getByText('Provider draft settings saved.')).toBeVisible();
  await expect(page.getByText(/Retarget every route/)).toHaveCount(0);

  await page.getByRole('button', { name: 'Disable provider' }).click();
  await expect(
    page.getByText('Provider disabled in runtime generation 9.')
  ).toBeVisible();
  await expect(page.getByText('disabled · not serving')).toBeVisible();
  await expect(
    page.getByText('This provider is disabled. No revision is serving traffic.')
  ).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Disable provider' })
  ).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Save draft' })).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Run upstream discovery' })
  ).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Stage rotation' })
  ).toBeDisabled();
  await expect(page.getByText(DISABLED_EDIT_NOTE)).toHaveCount(2);

  await page.getByRole('button', { name: 'Restore provider as draft' }).click();
  await expect(page.getByText(/Provider restored as a draft/)).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Restore provider as draft' })
  ).toHaveCount(0);
  await expect(
    page.getByRole('button', { name: 'Activate changes' })
  ).toBeVisible();
  await expect(page.getByText(DISABLED_EDIT_NOTE)).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Save draft' })).toBeEnabled();
  expect(disableAttempts).toBe(2);
  expect(disableHeaders['if-match']).toBe(
    '"01980000-0000-7000-8000-000000000109"'
  );
  expect(disableHeaders['idempotency-key']).toMatch(
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
  );
  expect(restoreHeaders['if-match']).toBe(`"${disabledEtag}"`);
  expect(restoreHeaders['idempotency-key']).toMatch(
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
  );
});

test('a disable that conflicts on something other than references shows the generic failure', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  page.on('dialog', (dialog) => dialog.accept());
  const currentProvider = providerRecord('active', [certifiedModelRecord], {
    active_revision: 1,
    pending_activation: false
  });

  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/v1/providers') {
      await route.fulfill({
        json: { items: [currentProvider], next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({
        json: { items: currentProvider.models, next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/credentials')) {
      await route.fulfill({ json: { items: [] } });
      return;
    }
    if (pathname.endsWith('/disable')) {
      await route.fulfill({
        status: 409,
        contentType: 'application/problem+json',
        body: JSON.stringify({
          type: 'https://openllmproxy.dev/problems/idempotency_key_reused',
          title: 'Conflict',
          status: 409,
          detail:
            'This Idempotency-Key has already been used for this operation.'
        })
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
  await expect(page.getByText('revision 1 active')).toBeVisible();

  await page.getByRole('button', { name: 'Disable provider' }).click();

  // A replayed Idempotency-Key is a 409 too, but retargeting routes would not
  // fix it, so only the generic failure path may run.
  await expect(
    page.getByText('This Idempotency-Key has already been used')
  ).toBeVisible();
  await expect(page.getByText(/Retarget every route/)).toHaveCount(0);
  await expect(page.getByText('revision 1 active')).toBeVisible();
});

test('a disabled provider that still reports an active revision reads as disabled and locks editing', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  // The API can still name the revision that was serving when the provider was
  // disabled. Nothing is serving now, so the disabled state has to win.
  const currentProvider = providerRecord('disabled', [certifiedModelRecord], {
    active_revision: 1,
    pending_activation: false
  });

  await page.route('**/api/v1/providers**', async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname === '/api/v1/providers') {
      await route.fulfill({
        json: { items: [currentProvider], next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/revisions') && request.method() === 'GET') {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    if (pathname === `/api/v1/providers/${ids.provider}/models`) {
      await route.fulfill({
        json: { items: currentProvider.models, next_cursor: null }
      });
      return;
    }
    if (pathname.endsWith('/credentials')) {
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

  await expect(page.getByText('disabled · not serving')).toBeVisible();
  await expect(page.getByText('Revision 1 is live.')).toHaveCount(0);
  await expect(
    page.getByText('This provider is disabled. No revision is serving traffic.')
  ).toBeVisible();
  await expect(page.getByText(DISABLED_EDIT_NOTE)).toHaveCount(2);
  await expect(page.getByRole('button', { name: 'Save draft' })).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Run upstream discovery' })
  ).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Stage rotation' })
  ).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Server-certify capabilities' })
  ).toBeDisabled();
  await expect(
    page.getByRole('button', { name: 'Disable provider' })
  ).toHaveCount(0);
  await expect(
    page.getByRole('button', { name: 'Restore provider as draft' })
  ).toBeVisible();
});
