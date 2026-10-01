import { afterEach, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import {
  listAllProjectMembers,
  listProjects
} from '$lib/features/access/projects/api';

afterEach(() => {
  vi.restoreAllMocks();
});

function pages(
  byCursor: Record<string, { items: unknown[]; next: string | null }>
) {
  return vi.spyOn(apiClient, 'GET').mockImplementation((async (
    _path: string,
    init: { params: { query: { cursor?: string } } }
  ) => {
    const page = byCursor[init.params.query.cursor ?? ''];
    return {
      data: { items: page.items, next_cursor: page.next },
      response: new Response(null, { status: 200 })
    };
  }) as unknown as typeof apiClient.GET);
}

it('collects every project page', async () => {
  const get = pages({
    '': { items: [{ id: 'a' }], next: 'c2' },
    c2: { items: [{ id: 'b' }], next: null }
  });
  expect(await listProjects()).toEqual([{ id: 'a' }, { id: 'b' }]);
  expect(get).toHaveBeenCalledTimes(2);
});

it('collects every project member page', async () => {
  pages({
    '': { items: [{ user_id: 'u1' }], next: 'm2' },
    m2: { items: [{ user_id: 'u-mgr' }], next: null }
  });
  expect(await listAllProjectMembers('p1')).toEqual([
    { user_id: 'u1' },
    { user_id: 'u-mgr' }
  ]);
});
