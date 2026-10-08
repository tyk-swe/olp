import { afterEach, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import { listIncreases } from '$lib/features/access/budget-increases/api';

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
