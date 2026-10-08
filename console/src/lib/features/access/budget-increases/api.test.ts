import { afterEach, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import {
  listIncreases,
  revokeIncrease
} from '$lib/features/access/budget-increases/api';

afterEach(() => {
  vi.restoreAllMocks();
});

it('sends the next page token as the cursor', async () => {
  const get = vi.spyOn(apiClient, 'GET').mockResolvedValue({
    data: { items: [], next_cursor: null },
    response: new Response(null, { status: 200 })
  } as never);
  await listIncreases('c2');
  expect(get).toHaveBeenCalledWith(
    '/api/v1/budget-increases',
    expect.objectContaining({
      params: { query: { cursor: 'c2', limit: 50 } }
    })
  );
});

it('accepts a bodyless revocation', async () => {
  vi.spyOn(apiClient, 'DELETE').mockResolvedValue({
    data: undefined,
    response: new Response(null, { status: 204 })
  } as never);
  await expect(
    revokeIncrease({ id: 'i1', etag: 'e1' } as never)
  ).resolves.toBeUndefined();
});
