import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { clearCsrfToken } from '$lib/features/access/session/csrf';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import { ApiProblem } from '$lib/api/http';
import {
  account,
  pool,
  route,
  budget,
  projectId,
  keyId,
  userId,
  binding
} from '$lib/features/code-mode/test/fixtures';
import {
  listCodeAccounts,
  listCodePools,
  listCodeRoutes,
  listCodeBudgets,
  listCodeBindings,
  listCodeAttempts,
  listCodeRefusals,
  listCodeTokenWindows,
  listCodeRevisions,
  getCodeClientConfiguration,
  saveCodeAccount,
  saveCodePool,
  saveCodeRoute,
  saveCodeBudget,
  publishCodeRoute,
  retireCodeBinding
} from './code-mode';

beforeEach(() =>
  authLifecycle.establishSession({
    user: {
      id: userId,
      email: 'operator@example.com',
      display_name: 'Operator',
      role: 'operator',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'fixture-csrf'
  })
);
afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('code-mode collection boundaries', () => {
  it.each([
    ['accounts', listCodeAccounts],
    ['pools', listCodePools],
    ['routes', listCodeRoutes],
    ['budgets', listCodeBudgets],
    ['bindings', listCodeBindings],
    ['attempts', listCodeAttempts],
    ['refusals', listCodeRefusals],
    ['token-windows', listCodeTokenWindows]
  ] as const)(
    'uses project isolation, abort signals and cursor pagination for %s',
    async (name, list) => {
      const controller = new AbortController();
      const requests = captureRequests(() =>
        jsonResponse({ items: [], next_cursor: 'next-page' })
      );
      expect(
        await list(
          { project_id: projectId, cursor: 'page-two', limit: 25 },
          controller.signal
        )
      ).toEqual({ items: [], nextCursor: 'next-page' });
      const url = new URL(requests[0].url);
      expect(url.pathname).toBe(`/api/v1/code/${name}`);
      expect(Object.fromEntries(url.searchParams)).toEqual({
        project_id: projectId,
        cursor: 'page-two',
        limit: '25'
      });
      controller.abort();
      expect(requests[0].signal.aborted).toBe(true);
    }
  );
  it('preserves the metadata filters and revision cursor', async () => {
    const requests = captureRequests(() =>
      jsonResponse({ items: [], next_cursor: null })
    );
    await listCodeAttempts({
      project_id: projectId,
      route_id: route.id,
      api_key_id: keyId,
      account_id: account.id,
      binding_id: binding.id
    });
    expect(Object.fromEntries(new URL(requests[0].url).searchParams)).toEqual({
      project_id: projectId,
      route_id: route.id,
      api_key_id: keyId,
      account_id: account.id,
      binding_id: binding.id,
      limit: '50'
    });
    await listCodeRevisions(route.id, 'older');
    expect(new URL(requests[1].url).pathname).toBe(
      `/api/v1/code/routes/${route.id}/revisions`
    );
    expect(new URL(requests[1].url).searchParams.get('cursor')).toBe('older');
  });
});

it.each([undefined, 'another-native-model'])(
  'loads the generated client configuration for model %s with an abort signal',
  async (model) => {
    const configuration = {
      route_slug: route.slug,
      base_url: `https://gateway.example/code/${route.slug}`,
      native_models: ['native-model', 'another-native-model'],
      client: 'codex',
      client_version: '0.160.0',
      configuration: 'server-generated configuration',
      qualification_gaps: []
    };
    const requests = captureRequests(() => jsonResponse(configuration));
    const controller = new AbortController();
    expect(
      await getCodeClientConfiguration(
        route,
        'https://gateway.example',
        model ? { model } : {},
        controller.signal
      )
    ).toEqual(configuration);
    const url = new URL(requests[0].url);
    expect(url.pathname).toBe(`/api/v1/code/routes/${route.id}/client-config`);
    expect(Object.fromEntries(url.searchParams)).toEqual({
      gateway_url: 'https://gateway.example',
      ...(model ? { model } : {})
    });
    controller.abort();
    expect(requests[0].signal.aborted).toBe(true);
  }
);

it('sends safe management mutations through CSRF, idempotency and ETag middleware', async () => {
  const requests = captureRequests(() => jsonResponse(account));
  const accountWrite = {
    project_id: projectId,
    provider_id: account.provider_id,
    credential_id: account.credential_id,
    name: account.name,
    models: account.models,
    enabled: true
  };
  const poolWrite = {
    project_id: projectId,
    name: pool.name,
    kind: pool.kind,
    owner_user_id: null,
    account_ids: pool.account_ids,
    api_key_ids: pool.api_key_ids
  };
  const routeWrite = {
    project_id: projectId,
    slug: route.slug,
    pool_id: pool.id,
    models: route.models,
    enabled: true
  };
  const budgetWrite = {
    project_id: projectId,
    route_id: null,
    api_key_id: null,
    daily_tokens: 50000,
    monthly_tokens: null,
    enabled: true
  };
  await saveCodeAccount(accountWrite);
  await saveCodeAccount(accountWrite, account);
  await saveCodePool(poolWrite);
  await saveCodePool(poolWrite, pool);
  await saveCodeRoute(routeWrite);
  await saveCodeRoute(routeWrite, route);
  await saveCodeBudget(budgetWrite);
  await saveCodeBudget(budgetWrite, budget);
  await publishCodeRoute(route);
  await retireCodeBinding(binding.id);
  for (const request of requests) {
    expect(request.headers.get('X-CSRF-Token')).toBe('fixture-csrf');
    expect(request.credentials).toBe('same-origin');
    if (request.method === 'PUT' || request.url.endsWith('/publish'))
      expect(request.headers.get('If-Match')).toBe(`"${account.etag}"`);
    if (request.method === 'POST')
      expect(request.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
  }
  expect(await requests[0].json()).toEqual(accountWrite);
  expect(await requests[2].json()).toEqual(poolWrite);
  expect(await requests[4].json()).toEqual(routeWrite);
  expect(await requests[6].json()).toEqual(budgetWrite);
  expect(new URL(requests[8].url).pathname).toBe(
    `/api/v1/code/routes/${route.id}/publish`
  );
  expect(new URL(requests[9].url).pathname).toBe(
    `/api/v1/code/bindings/${binding.id}/retire`
  );
  expect(
    requests.every(
      (request) => !new URL(request.url).pathname.includes('probe')
    )
  ).toBe(true);
});

it('keeps typed conflicts and never retries mutations or invents a config endpoint', async () => {
  const requests = captureRequests(() =>
    jsonResponse(
      { title: 'Conflict', status: 412, detail: 'The draft changed.' },
      { status: 412 }
    )
  );
  await expect(publishCodeRoute(route)).rejects.toBeInstanceOf(ApiProblem);
  expect(requests).toHaveLength(1);
});
