import { afterEach, describe, expect, it, vi } from 'vitest';
import { apiClient } from './client';
import { clearCsrfToken, getCsrfToken, setCsrfToken } from './session';
import { authLifecycle } from '$lib/auth/lifecycle';
import { captureRequests, jsonResponse } from './test/requestCapture';

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

describe('generated API request boundary', () => {
  it('keeps reads same-origin, uncached, redirect-denying, and JSON-accepting', async () => {
    setCsrfToken('csrf-for-mutations-only');
    const requests = captureRequests(() => jsonResponse({ setup_required: false }));

    await apiClient.GET('/api/v1/setup/status');

    const request = requests[0];
    expect(request).toBeDefined();
    expect(new URL(request.url).origin).toBe(globalThis.location.origin);
    expect(request.cache).toBe('no-store');
    expect(request.credentials).toBe('same-origin');
    expect(request.redirect).toBe('error');
    expect(request.headers.get('accept')).toBe('application/json');
    expect(request.headers.has('x-csrf-token')).toBe(false);
  });

  it('serializes bare UUID If-Match values and preserves strong ETags', async () => {
    const etag = '019b036f-fcad-72a0-9a35-734fa53adf5f';
    const requests = captureRequests(() => jsonResponse({}));
    authLifecycle.establishSession(session);

    await apiClient.PATCH('/api/v1/profile', {
      params: { header: { 'If-Match': etag } },
      body: { display_name: 'Operator' }
    });
    await apiClient.PUT('/api/v1/settings/{key}', {
      params: { path: { key: 'retention_days' }, header: { 'If-Match': `"${etag}"` } },
      body: { value: '30' }
    });

    expect(requests.map((request) => request.headers.get('if-match'))).toEqual([
      `"${etag}"`,
      `"${etag}"`
    ]);
  });

  it('adds CSRF only to mutating methods and stops after token clearing', async () => {
    const requests = captureRequests(() => jsonResponse({}));
    authLifecycle.establishSession(session);

    await apiClient.POST('/api/v1/sessions', {
      body: { email: 'operator@example.com', password: 'correct horse battery staple' }
    });
    await apiClient.PATCH('/api/v1/profile', {
      params: { header: { 'If-Match': 'profile-etag' } },
      body: { display_name: 'Operator' }
    });
    await apiClient.PUT('/api/v1/settings/{key}', {
      params: { path: { key: 'retention_days' }, header: { 'If-Match': 'setting-etag' } },
      body: { value: '30' }
    });
    await apiClient.DELETE('/api/v1/sessions/current');
    await apiClient.GET('/api/v1/sessions/current');

    expect(requests.map((request) => request.method)).toEqual([
      'POST',
      'PATCH',
      'PUT',
      'DELETE',
      'GET'
    ]);
    expect(requests[0]?.headers.has('x-csrf-token')).toBe(false);
    for (const request of requests.slice(1, 4)) {
      expect(request.headers.get('x-csrf-token')).toBe('csrf-boundary-token');
    }
    expect(requests[4]?.headers.has('x-csrf-token')).toBe(false);

    clearCsrfToken();
    await apiClient.POST('/api/v1/sessions', {
      body: { email: 'operator@example.com', password: 'correct horse battery staple' }
    });
    expect(requests[5]?.headers.has('x-csrf-token')).toBe(false);
  });

  it('routes response-side CSRF rotation through the lifecycle', async () => {
    captureRequests(() =>
      jsonResponse({}, { headers: { 'x-csrf-token': 'csrf-rotated-by-response' } })
    );
    authLifecycle.establishSession(session);

    await apiClient.PATCH('/api/v1/profile', {
      params: { header: { 'If-Match': 'profile-etag' } },
      body: { display_name: 'Operator' }
    });

    expect(getCsrfToken()).toBe('csrf-rotated-by-response');
  });
});
