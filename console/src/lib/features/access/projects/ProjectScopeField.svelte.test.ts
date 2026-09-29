import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { listProjectMemberships } from '$lib/features/access/projects/api';
import ProjectScopeProbe from './test/ProjectScopeProbe.svelte';

const role = vi.hoisted(() => ({ user: { access_scope: 'global' } }));

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => role
}));
vi.mock('$lib/features/access/projects/api', () => ({
  listProjectMemberships: vi.fn()
}));

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listProjectMemberships).mockResolvedValue([]);
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
});

it('offers the installation-wide boundary to installation-wide members', async () => {
  component = mount(ProjectScopeProbe, { target: host, props: { client } });
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('option')?.textContent).toBe('Installation-wide');
  });
});

it('explains why nothing can be chosen when the boundary is withheld', async () => {
  component = mount(ProjectScopeProbe, {
    target: host,
    props: { client, unassigned: false }
  });
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('select')).not.toBeNull();
  });
  expect(host.querySelector('option')).toBeNull();
  expect(host.textContent).toContain('No project exists yet');
});
