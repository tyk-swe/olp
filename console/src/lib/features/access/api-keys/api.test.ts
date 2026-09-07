import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import { hasNonrevokedApiKey } from '$lib/features/access/api-keys/api';

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('setup API key existence', () => {
  it('stops at the first nonrevoked key, including expired keys', async () => {
    const requests = captureRequests(() =>
      jsonResponse({
        items: [{ revoked_at: null, expires_at: '2020-01-01T00:00:00Z' }],
        next_cursor: 'unused'
      })
    );

    await expect(hasNonrevokedApiKey()).resolves.toBe(true);
    expect(requests).toHaveLength(1);
  });

  it('scans revoked-only leading pages and stops at a later match', async () => {
    const controller = new AbortController();
    const requests = captureRequests((_request, index) =>
      jsonResponse({
        items: [{ revoked_at: index < 2 ? '2026-01-01T00:00:00Z' : null }],
        next_cursor: `page-${index + 2}`
      })
    );

    await expect(hasNonrevokedApiKey(controller.signal)).resolves.toBe(true);
    expect(
      requests.map((request) => new URL(request.url).searchParams.get('cursor'))
    ).toEqual([null, 'page-2', 'page-3']);

    controller.abort();
    expect(requests.every((request) => request.signal.aborted)).toBe(true);
  });

  it('returns false for an empty inventory', async () => {
    const requests = captureRequests(() =>
      jsonResponse({ items: [], next_cursor: null })
    );

    await expect(hasNonrevokedApiKey()).resolves.toBe(false);
    expect(requests).toHaveLength(1);
  });

  it('returns false after exhausting revoked keys', async () => {
    const requests = captureRequests((_request, index) =>
      jsonResponse({
        items: [{ revoked_at: '2026-01-01T00:00:00Z' }],
        next_cursor: index === 0 ? 'last' : null
      })
    );

    await expect(hasNonrevokedApiKey()).resolves.toBe(false);
    expect(requests).toHaveLength(2);
  });

  it('propagates a later-page API error', async () => {
    const requests = captureRequests((_request, index) =>
      index === 0
        ? jsonResponse({ items: [], next_cursor: 'last' })
        : jsonResponse({ title: 'Unavailable', status: 503 }, { status: 503 })
    );

    await expect(hasNonrevokedApiKey()).rejects.toMatchObject({
      problem: { status: 503, title: 'Unavailable' }
    });
    expect(requests).toHaveLength(2);
  });

  it('propagates cancellation without scanning another page', async () => {
    const controller = new AbortController();
    const requests = captureRequests((request) => {
      controller.abort();
      request.signal.throwIfAborted();
      return jsonResponse({ items: [], next_cursor: 'unused' });
    });

    await expect(hasNonrevokedApiKey(controller.signal)).rejects.toMatchObject({
      name: 'AbortError'
    });
    expect(requests).toHaveLength(1);
  });

  it('fails closed when a page repeats a cursor', async () => {
    const requests = captureRequests(() =>
      jsonResponse({ items: [], next_cursor: 'repeat' })
    );

    await expect(hasNonrevokedApiKey()).rejects.toBeInstanceOf(ApiProblem);
    expect(requests).toHaveLength(2);
  });
});
