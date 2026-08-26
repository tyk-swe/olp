import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/auth/lifecycle';
import { ApiProblem } from '../http';
import { clearCsrfToken } from '../session';
import { captureRequests, jsonResponse } from '../test/requestCapture';
import {
  disableProvider,
  isProviderInUse,
  restoreProviderAsDraft,
  type Provider
} from './providers';

const providerId = '01980000-0000-7000-8000-000000000101';
const providerEtag = '01980000-0000-7000-8000-000000000109';
const generationId = '01980000-0000-7000-8000-000000000205';
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000401',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const
  },
  csrf_token: 'csrf-provider-token'
};

// Only the fields the two lifecycle calls read; the rest of the detail
// response is irrelevant to the request they build.
const provider = { id: providerId, etag: providerEtag } as Provider;

function problem(status: number, type: string, detail: string): ApiProblem {
  return new ApiProblem({ type, title: 'Conflict', status, detail });
}

function problemResponse(status: number, type: string, detail: string) {
  return jsonResponse(
    { type, title: 'Conflict', status, detail },
    { status, headers: { 'content-type': 'application/problem+json' } }
  );
}

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('disableProvider', () => {
  it('reports no runtime generation when the disable published none', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({
        provider_id: providerId,
        etag: providerEtag,
        credential_id: null,
        credential_version: null,
        runtime_generation: null
      })
    );

    await expect(disableProvider(provider)).resolves.toBeNull();

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe(
      `/api/v1/providers/${providerId}/disable`
    );
    expect(request.headers.get('if-match')).toBe(`"${providerEtag}"`);
    expect(request.headers.get('idempotency-key')).toMatch(uuid);
  });

  it('returns the sequence of the generation the disable published', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({
        provider_id: providerId,
        etag: providerEtag,
        credential_id: null,
        credential_version: null,
        runtime_generation: { id: generationId, sequence: 9 }
      })
    );

    await expect(disableProvider(provider)).resolves.toBe(9);
    expect(requests[0]!.headers.get('if-match')).toBe(`"${providerEtag}"`);
    expect(requests[0]!.headers.get('idempotency-key')).toMatch(uuid);
  });
});

describe('restoreProviderAsDraft', () => {
  it('posts the restore path with the concurrency and replay headers', async () => {
    authLifecycle.establishSession(session);
    const restored = { ...provider, state: 'draft' };
    const requests = captureRequests(() => jsonResponse(restored));

    await expect(restoreProviderAsDraft(provider)).resolves.toEqual(restored);

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe(
      `/api/v1/providers/${providerId}/restore-as-draft`
    );
    expect(request.headers.get('if-match')).toBe(`"${providerEtag}"`);
    expect(request.headers.get('idempotency-key')).toMatch(uuid);
  });
});

describe('isProviderInUse', () => {
  it('recognizes the in-use conflict the server raises for a routed provider', async () => {
    authLifecycle.establishSession(session);
    captureRequests(() =>
      problemResponse(
        409,
        'https://openllmproxy.dev/problems/configuration_resource_in_use',
        'The resource is active or referenced and cannot be removed.'
      )
    );

    const error = await disableProvider(provider).catch(
      (value: unknown) => value
    );

    expect(isProviderInUse(error)).toBe(true);
  });

  it('rejects the other conflicts that share the 409 status', async () => {
    authLifecycle.establishSession(session);
    captureRequests(() =>
      problemResponse(
        409,
        'https://openllmproxy.dev/problems/idempotency_key_reused',
        'This Idempotency-Key has already been used for this operation.'
      )
    );

    const error = await disableProvider(provider).catch(
      (value: unknown) => value
    );

    expect(error).toBeInstanceOf(ApiProblem);
    expect(isProviderInUse(error)).toBe(false);
    expect(
      isProviderInUse(
        problem(
          409,
          'https://openllmproxy.dev/problems/idempotency_in_progress',
          'An operation with this Idempotency-Key is still in progress.'
        )
      )
    ).toBe(false);
    expect(
      isProviderInUse(
        problem(
          409,
          'https://openllmproxy.dev/problems/route_not_validated',
          'The route draft has not been validated.'
        )
      )
    ).toBe(false);
  });

  it('rejects a stale-ETag problem, a plain error, and a missing error', () => {
    expect(
      isProviderInUse(
        problem(
          412,
          'https://openllmproxy.dev/problems/etag_mismatch',
          'The provider changed since it was loaded.'
        )
      )
    ).toBe(false);
    expect(isProviderInUse(new Error('network unavailable'))).toBe(false);
    expect(isProviderInUse(null)).toBe(false);
  });
});
