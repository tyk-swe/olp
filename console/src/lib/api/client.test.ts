import { afterEach, describe, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import {
  ApiProblem,
  ensureSuccess,
  fieldIssues,
  isEtagMismatch
} from '$lib/api/http';
import { stringifyNativeJSON } from '$lib/json/nativeJson';
import {
  clearCsrfToken,
  getCsrfToken,
  setCsrfToken
} from '$lib/features/access/session/api';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000001',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const,
    access_scope: 'global' as const
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
    const requests = captureRequests(() =>
      jsonResponse({ setup_required: false })
    );

    await apiClient.GET('/api/v1/setup/status');

    const request = requests[0];
    expect(request).toBeDefined();
    expect(new URL(request.url).origin).toBe('http://127.0.0.1');
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
      params: {
        path: { key: 'retention_days' },
        header: { 'If-Match': `"${etag}"` }
      },
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
      body: {
        email: 'operator@example.com',
        password: 'correct horse battery staple'
      }
    });
    await apiClient.PATCH('/api/v1/profile', {
      params: { header: { 'If-Match': 'profile-etag' } },
      body: { display_name: 'Operator' }
    });
    await apiClient.PUT('/api/v1/settings/{key}', {
      params: {
        path: { key: 'retention_days' },
        header: { 'If-Match': 'setting-etag' }
      },
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
      body: {
        email: 'operator@example.com',
        password: 'correct horse battery staple'
      }
    });
    expect(requests[5]?.headers.has('x-csrf-token')).toBe(false);
  });

  it('routes response-side CSRF rotation through the lifecycle', async () => {
    captureRequests(() =>
      jsonResponse(
        {},
        { headers: { 'x-csrf-token': 'csrf-rotated-by-response' } }
      )
    );
    authLifecycle.establishSession(session);

    await apiClient.PATCH('/api/v1/profile', {
      params: { header: { 'If-Match': 'profile-etag' } },
      body: { display_name: 'Operator' }
    });

    expect(getCsrfToken()).toBe('csrf-rotated-by-response');
  });
});

it('retains native configuration through the generated client without Content-Length', async () => {
  const source =
    '{"configuration":{"kind":"openai","auth_mode":"none","options":{"parameter_defaults":{"seed":9007199254740993,"tiny":1e-1000,"schema":{"10":{},"2":{},"__proto__":{"inert":true}}}}}}';
  captureRequests(
    () =>
      new Response(source, {
        headers: {
          'Content-Type': 'application/json',
          'Transfer-Encoding': 'chunked'
        }
      })
  );
  const response = await apiClient.GET('/api/v1/providers/{provider_id}', {
    params: { path: { provider_id: 'provider' } }
  });
  expect(stringifyNativeJSON(response.data)).toBe(source);
  expect(response.response.headers.has('content-length')).toBe(false);
});

describe('generated API error boundary', () => {
  it('decodes problem documents on unsuccessful responses', async () => {
    const problem = {
      type: 'https://openllmproxy.dev/problems/etag_mismatch',
      title: 'Precondition failed',
      status: 412,
      detail: 'The stored revision no longer matches the supplied ETag.',
      errors: {
        display_name: [{ code: 'required', message: 'Provide a display name.' }]
      }
    };
    captureRequests(() => jsonResponse(problem, { status: 412 }));
    authLifecycle.establishSession(session);

    const response = await apiClient.PATCH('/api/v1/profile', {
      params: { header: { 'If-Match': 'stale-etag' } },
      body: { display_name: 'Operator' }
    });

    expect(response.data).toBeUndefined();
    expect(response.error).toEqual(problem);
    let caught: unknown;
    try {
      ensureSuccess(response.error, response.response);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect(isEtagMismatch(caught)).toBe(true);
    expect(fieldIssues(caught)).toEqual([
      {
        field: 'display_name',
        code: 'required',
        message: 'Provide a display name.'
      }
    ]);
  });

  it('keeps unstructured error bodies raw for the generic fallback', async () => {
    captureRequests(
      () =>
        new Response('<html>upstream unavailable</html>', {
          status: 502,
          headers: { 'content-type': 'text/html' }
        })
    );

    const response = await apiClient.GET('/api/v1/setup/status');

    expect(response.error).toBe('<html>upstream unavailable</html>');
    let caught: unknown;
    try {
      ensureSuccess(response.error, response.response);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(ApiProblem);
    expect((caught as ApiProblem).problem.title).toBe('Request failed (502)');
  });
});
