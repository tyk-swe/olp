import { afterEach, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import {
  organizationMembers,
  projectMembers,
  putOrganizationMember,
  removeOrganizationMember,
  removeProjectMember
} from '$lib/features/access/organizations/api';

afterEach(() => {
  vi.restoreAllMocks();
});

const noContent = {
  data: undefined,
  response: new Response(null, { status: 204 })
} as never;

it('accepts bodyless member changes', async () => {
  vi.spyOn(apiClient, 'PUT').mockResolvedValue(noContent);
  vi.spyOn(apiClient, 'DELETE').mockResolvedValue(noContent);
  await expect(
    putOrganizationMember('o1', 'u1', 'viewer', 'e1')
  ).resolves.toBeUndefined();
  await expect(
    removeOrganizationMember('o1', 'u1', 'e1')
  ).resolves.toBeUndefined();
  await expect(
    removeProjectMember('o1', 'p1', 'u1', 'e1')
  ).resolves.toBeUndefined();
});

it('follows member cursors to the last page', async () => {
  const page = (user_id: string, next_cursor: string | null) => ({
    data: { items: [{ user_id }], next_cursor },
    response: new Response(null, { status: 200 })
  });
  const get = vi
    .spyOn(apiClient, 'GET')
    .mockResolvedValueOnce(page('u2', 'c1') as never)
    .mockResolvedValueOnce(page('u1', null) as never)
    .mockResolvedValueOnce(page('u4', 'c2') as never)
    .mockResolvedValueOnce(page('u3', null) as never);
  expect(await organizationMembers('o1')).toEqual([
    { user_id: 'u2' },
    { user_id: 'u1' }
  ]);
  expect(await projectMembers('o1', 'p1')).toEqual([
    { user_id: 'u4' },
    { user_id: 'u3' }
  ]);
  for (const [call, cursor] of [
    [2, 'c1'],
    [4, 'c2']
  ] as const) {
    expect(get).toHaveBeenNthCalledWith(
      call,
      expect.any(String),
      expect.objectContaining({
        params: expect.objectContaining({ query: { cursor } })
      })
    );
  }
});
