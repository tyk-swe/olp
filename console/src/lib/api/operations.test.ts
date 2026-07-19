import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from './http';
import { listProviderHealth, listRequests, operationsTesting } from './operations';
import { captureRequests, jsonResponse } from './test/requestCapture';

afterEach(() => {
  vi.unstubAllGlobals();
});

function providerHealth(providerId: string) {
  return {
    provider_id: providerId,
    provider_name: `Provider ${providerId}`,
    provider_kind: 'openai',
    provider_state: 'active',
    status: 'healthy',
    attempt_count: 1,
    success_count: 1,
    rate_limit_count: 0,
    server_error_count: 0,
    transport_error_count: 0
  };
}

describe('operations query serialization', () => {
  it('removes empty filters while retaining zero and false values', () => {
    expect(
      operationsTesting.compact({
        status: 0,
        enabled: false,
        route: '',
        cursor: undefined,
        provider: null
      })
    ).toEqual({ status: 0, enabled: false });
  });
});

describe('provider-health pagination', () => {
  it('aggregates every page and sends each cursor once', async () => {
    const first = providerHealth('provider-1');
    const second = providerHealth('provider-2');
    const requests = captureRequests((_request, index) =>
      index === 0
        ? jsonResponse({ window_minutes: 30, data: [first], next_cursor: 'page-2' })
        : jsonResponse({ window_minutes: 30, data: [second], next_cursor: null })
    );

    await expect(listProviderHealth(30)).resolves.toEqual({
      window_minutes: 30,
      data: [first, second]
    });
    expect(requests).toHaveLength(2);
    expect(new URL(requests[0]!.url).searchParams.get('cursor')).toBeNull();
    expect(new URL(requests[1]!.url).searchParams.get('cursor')).toBe('page-2');
    for (const request of requests) {
      const params = new URL(request.url).searchParams;
      expect(params.get('window_minutes')).toBe('30');
      expect(params.get('limit')).toBe('200');
    }
  });

  it('rejects a repeated cursor instead of looping', async () => {
    const requests = captureRequests(() =>
      jsonResponse({ window_minutes: 15, data: [], next_cursor: 'repeat' })
    );

    const error = await listProviderHealth().catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      title: 'Provider-health pagination repeated a cursor',
      status: 502
    });
    expect(requests).toHaveLength(2);
  });
});

describe('operations API errors', () => {
  it('preserves structured problem details', async () => {
    captureRequests(() =>
      jsonResponse(
        { title: 'Rate limited', detail: 'Retry after the window', status: 429 },
        { status: 503, headers: { 'content-type': 'application/problem+json' } }
      )
    );

    const error = await listRequests({}).catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      title: 'Rate limited',
      detail: 'Retry after the window',
      status: 429
    });
  });

  it('falls back to the response status for unstructured errors', async () => {
    captureRequests(() => jsonResponse('gateway unavailable', { status: 503 }));

    const error = await listRequests({}).catch((value: unknown) => value);

    expect(error).toBeInstanceOf(ApiProblem);
    expect((error as ApiProblem).problem).toEqual({
      title: 'Request failed',
      status: 503
    });
  });
});
