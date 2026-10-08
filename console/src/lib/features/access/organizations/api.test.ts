import { afterEach, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import {
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
