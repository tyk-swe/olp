import { afterEach, describe, expect, it, vi } from 'vitest';
import accessCode from './access.ts?raw';
import apiKeysCode from './api-keys.ts?raw';
import providersCode from './providers.ts?raw';
import routesCode from './routes.ts?raw';
import operationsCode from '../operations.ts?raw';
import operationStateCode from '$lib/forms/operationState.ts?raw';
import apiKeysPageCode from '$lib/features/access/api-keys/ApiKeysPage.svelte?raw';
import apiKeyInventoryCode from '$lib/features/access/api-keys/ApiKeyInventory.svelte?raw';
import invitationsPanelCode from '$lib/features/access/invitations/InvitationsPanel.svelte?raw';
import providerWizardCode from '$lib/features/gateway/providers/ProviderWizard.svelte?raw';
import providerActivationControlsCode from '$lib/features/gateway/providers/ProviderActivationControls.svelte?raw';
import providerCredentialsSectionCode from '$lib/features/gateway/providers/ProviderCredentialsSection.svelte?raw';
import providerRevisionsSectionCode from '$lib/features/gateway/providers/ProviderRevisionsSection.svelte?raw';
import routeDraftEditorCode from '$lib/features/gateway/routes/RouteDraftEditor.svelte?raw';
import routeRevisionHistoryCode from '$lib/features/gateway/routes/RouteRevisionHistory.svelte?raw';
import settingsPageCode from '$lib/features/settings/installation/SettingsPage.svelte?raw';

import { authLifecycle } from '$lib/auth/lifecycle';
import { clearCsrfToken } from '../session';
import { createInvitation, revokeInvitation } from './access';
import { createApiKey, rotateApiKey, revokeApiKey, type ApiKey } from './api-keys';
import {
  createProvider,
  activateProvider,
  restoreProviderRevision,
  rotateProviderCredential,
  revokeProviderCredential,
  type Provider
} from './providers';
import { createRouteDraft, activateRoute, restoreRouteRevision, type RouteDraft } from './routes';
import { createPricingRevision } from '../operations';
import { captureRequests, jsonResponse } from '../test/requestCapture';

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000001',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const
  },
  csrf_token: 'csrf-boundary-token'
};

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('Management API Idempotency Contract', () => {
  it('requires caller-provided idempotency keys and forwards them in Idempotency-Key headers', async () => {
    authLifecycle.establishSession(session);
    const testKey = 'test-idempotency-uuid-1234';

    // 1. createInvitation
    const req1 = captureRequests(() =>
      jsonResponse({ email: 'test@example.com', role: 'developer', token: 'secret-token-1' })
    );
    await createInvitation('test@example.com', 'developer', testKey);
    expect(req1[0]!.headers.get('idempotency-key')).toBe(testKey);

    // 2. revokeInvitation
    const req2 = captureRequests(() => jsonResponse({}));
    await revokeInvitation('inv-123', testKey);
    expect(req2[0]!.headers.get('idempotency-key')).toBe(testKey);

    // 3. createApiKey
    const req3 = captureRequests(() =>
      jsonResponse({ id: 'k1', name: 'key1', secret: 'sec-1' })
    );
    await createApiKey({ name: 'key1', allowed_routes: ['*'] }, testKey);
    expect(req3[0]!.headers.get('idempotency-key')).toBe(testKey);

    // 4. rotateApiKey
    const mockApiKey = {
      id: 'k1',
      name: 'key1',
      etag: '"etag-key-1"'
    } as unknown as ApiKey;
    const req4 = captureRequests(() =>
      jsonResponse({ id: 'k1', name: 'key1', secret: 'sec-2' })
    );
    await rotateApiKey(mockApiKey, testKey);
    expect(req4[0]!.headers.get('idempotency-key')).toBe(testKey);
    expect(req4[0]!.headers.get('if-match')).toBe('"etag-key-1"');

    // 5. revokeApiKey
    const req5 = captureRequests(() => jsonResponse({}));
    await revokeApiKey(mockApiKey, testKey);
    expect(req5[0]!.headers.get('idempotency-key')).toBe(testKey);
    expect(req5[0]!.headers.get('if-match')).toBe('"etag-key-1"');

    // 6. createProvider
    const req6 = captureRequests(() => jsonResponse({ id: 'prov-1' }));
    await createProvider(
      {
        name: 'Provider 1',
        kind: 'openai',
        endpoint: 'https://api.openai.com/v1',
        auth_mode: 'api_key',
        credential: 'sk-test'
      },
      testKey
    );
    expect(req6[0]!.headers.get('idempotency-key')).toBe(testKey);

    // 7. activateProvider
    const mockProvider = {
      id: 'prov-1',
      name: 'Provider 1',
      kind: 'openai',
      state: 'draft',
      etag: '"etag-prov-1"'
    } as unknown as Provider;
    const req7 = captureRequests(() =>
      jsonResponse({ runtime_generation: { sequence: 2 } })
    );
    await activateProvider(mockProvider, testKey);
    expect(req7[0]!.headers.get('idempotency-key')).toBe(testKey);
    expect(req7[0]!.headers.get('if-match')).toBe('"etag-prov-1"');

    // 8. restoreProviderRevision
    const req8 = captureRequests(() => jsonResponse({ provider: mockProvider }));
    await restoreProviderRevision(mockProvider, 'rev-1', testKey);
    expect(req8[0]!.headers.get('idempotency-key')).toBe(testKey);
    expect(req8[0]!.headers.get('if-match')).toBe('"etag-prov-1"');

    // 9. rotateProviderCredential
    const req9 = captureRequests(() => jsonResponse({}));
    await rotateProviderCredential(mockProvider, 'new-secret', testKey);
    expect(req9[0]!.headers.get('idempotency-key')).toBe(testKey);
    expect(req9[0]!.headers.get('if-match')).toBe('"etag-prov-1"');

    // 10. revokeProviderCredential
    const req10 = captureRequests(() => jsonResponse({}));
    await revokeProviderCredential(mockProvider, 'cred-1', testKey);
    expect(req10[0]!.headers.get('idempotency-key')).toBe(testKey);
    expect(req10[0]!.headers.get('if-match')).toBe('"etag-prov-1"');

    // 11. createRouteDraft
    const req11 = captureRequests(() => jsonResponse({ id: 'draft-1' }));
    await createRouteDraft(
      {
        slug: 'default',
        operations: ['generation'],
        overall_timeout_ms: 60000,
        max_attempts: 2,
        targets: []
      },
      testKey
    );
    expect(req11[0]!.headers.get('idempotency-key')).toBe(testKey);

    // 12. activateRoute
    const mockDraft = {
      id: 'draft-1',
      slug: 'default',
      state: 'validated',
      etag: '"etag-draft-1"'
    } as unknown as RouteDraft;
    const req12 = captureRequests(() =>
      jsonResponse({
        route_id: 'route-1',
        revision: 1,
        runtime_generation: { sequence: 3 }
      })
    );
    await activateRoute(mockDraft, testKey);
    expect(req12[0]!.headers.get('idempotency-key')).toBe(testKey);
    expect(req12[0]!.headers.get('if-match')).toBe('"etag-draft-1"');

    // 13. restoreRouteRevision
    const req13 = captureRequests(() => jsonResponse(mockDraft));
    await restoreRouteRevision('route-1', 'rev-1', testKey);
    expect(req13[0]!.headers.get('idempotency-key')).toBe(testKey);

    // 14. createPricingRevision
    const req14 = captureRequests(() => jsonResponse({ id: 'price-rev-1' }));
    await createPricingRevision(
      '2026-01-01T00:00:00Z',
      [
        {
          provider_kind: 'openai',
          model: 'gpt-4o',
          operation: 'generation',
          currency: 'USD'
        }
      ],
      testKey
    );
    expect(req14[0]!.headers.get('idempotency-key')).toBe(testKey);
  });

  it('verifies zero crypto.randomUUID() calls exist in raw management API modules', () => {
    const contents = [
      accessCode,
      apiKeysCode,
      providersCode,
      routesCode,
      operationsCode
    ];

    for (const code of contents) {
      expect(code).not.toContain('randomUUID');
    }
  });

  it('verifies localStorage and sessionStorage are not used in console client source', () => {
    const allFiles = [
      operationStateCode,
      accessCode,
      apiKeysCode,
      providersCode,
      routesCode,
      operationsCode,
      apiKeysPageCode,
      apiKeyInventoryCode,
      invitationsPanelCode,
      providerWizardCode,
      providerActivationControlsCode,
      providerCredentialsSectionCode,
      providerRevisionsSectionCode,
      routeDraftEditorCode,
      routeRevisionHistoryCode,
      settingsPageCode
    ];

    for (const code of allFiles) {
      expect(code).not.toContain('localStorage');
      expect(code).not.toContain('sessionStorage');
    }
  });
});
