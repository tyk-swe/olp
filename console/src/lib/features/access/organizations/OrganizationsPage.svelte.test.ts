// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, vi } from 'vitest';
import OrganizationProbe from './test/OrganizationProbe.svelte';
import {
  createOrganizationProject,
  organizationProjects,
  projectMembers
} from './api';
import { projectKeys } from '../projects/projectKeys';
const state = vi.hoisted(() => ({
  role: 'viewer',
  owner: false,
  created: false
}));
vi.mock('./api', () => ({
  listOrganizations: vi.fn(async () => [
    { id: 'org', name: 'Division', etag: 'etag' },
    ...(state.created ? [{ id: 'new-org', name: 'Branch', etag: 'etag' }] : [])
  ]),
  createOrganization: vi.fn(async () => {
    state.created = true;
    return { id: 'new-org', name: 'Branch', etag: 'etag' };
  }),
  organizationMembers: vi.fn(async () => [
    {
      user_id: 'user',
      email: 'manager@example.com',
      role: state.role,
      installation_role: 'developer'
    }
  ]),
  organizationProjects: vi.fn(async () => []),
  projectMembers: vi.fn(async () => []),
  createOrganizationProject: vi.fn(async () => ({ id: 'project' }))
}));
vi.mock('../session/useRole.svelte', () => ({
  useRole: () => ({
    user: { id: 'user' },
    allows: (route: string) =>
      state.owner || route !== 'POST /api/v1/organizations'
  })
}));
vi.mock('../budgets/api', () => ({
  getBudget: vi.fn(async () => ({
    policy: null,
    etag: 'budget-etag',
    usage: {
      daily: { accrued: '0' },
      weekly: { accrued: '0' },
      monthly: { accrued: '0' },
      unpriced_attempts: 0
    }
  }))
}));
it.each(['viewer', 'manager'])(
  'respects organization %s scope without offering installation creation',
  async (role) => {
    state.role = role;
    vi.mocked(createOrganizationProject).mockClear();
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } }
    });
    const target = document.createElement('div');
    document.body.append(target);
    const component = mount(OrganizationProbe, { target, props: { client } });
    try {
      flushSync();
      await vi.waitFor(() => expect(target.textContent).toContain('Division'));
      await vi.waitFor(() =>
        expect(target.textContent).toContain(
          role === 'manager'
            ? 'You can manage this organization.'
            : 'You have read-only access'
        )
      );
      const button = (label: string) =>
        Array.from(target.querySelectorAll('button')).find(
          (b) => b.textContent?.trim() === label
        );
      expect(button('Create organization')).toBeUndefined();
      expect(!!button('Create organization project')).toBe(role === 'manager');
      expect(!!button('Save organization budget')).toBe(role === 'manager');
      if (role === 'manager') {
        const label = Array.from(target.querySelectorAll('label')).find((l) =>
          l.textContent?.includes('New project name')
        )!;
        const input = label.querySelector('input')!;
        input.value = 'Team';
        input.dispatchEvent(new Event('input', { bubbles: true }));
        flushSync();
        input
          .closest('form')!
          .dispatchEvent(
            new Event('submit', { bubbles: true, cancelable: true })
          );
        await vi.waitFor(() =>
          expect(createOrganizationProject).toHaveBeenCalledWith(
            'org',
            'Team',
            expect.any(String)
          )
        );
      }
    } finally {
      await unmount(component);
      client.clear();
      target.remove();
    }
  }
);

it('selects the organization it just created', async () => {
  state.owner = true;
  state.created = false;
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const target = document.createElement('div');
  document.body.append(target);
  const component = mount(OrganizationProbe, { target, props: { client } });
  try {
    flushSync();
    await vi.waitFor(() => expect(target.textContent).toContain('Division'));
    const label = Array.from(target.querySelectorAll('label')).find((l) =>
      l.textContent?.includes('New organization name')
    )!;
    const input = label.querySelector('input')!;
    input.value = 'Branch';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    input
      .closest('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() =>
      expect(target.querySelector('[role="status"]')?.textContent).toBe(
        'Saved.'
      )
    );
    flushSync();
    const select = Array.from(target.querySelectorAll('label'))
      .find((l) => l.textContent?.startsWith('Organization'))!
      .querySelector('select')!;
    expect(select.value).toBe('new-org');
  } finally {
    await unmount(component);
    client.clear();
    target.remove();
    state.owner = false;
  }
});

it.each([
  { description: 'while it is still selected', switchOrganization: false },
  { description: 'after switching organizations', switchOrganization: true }
])(
  'caches a created project under its submitted organization $description',
  async ({ switchOrganization }) => {
    state.role = 'manager';
    state.created = true;
    const createdProject = {
      id: 'project',
      name: 'Team',
      etag: 'project-etag',
      created_at: '2026-01-01T00:00:00Z',
      created_by: 'user',
      created_by_email: 'manager@example.com',
      member_count: 0,
      updated_at: '2026-01-01T00:00:00Z',
      organization_id: 'org'
    };
    const otherProject = {
      ...createdProject,
      id: 'other-project',
      name: 'Branch project',
      organization_id: 'new-org'
    };
    const response =
      Promise.withResolvers<
        Awaited<ReturnType<typeof createOrganizationProject>>
      >();
    vi.mocked(createOrganizationProject).mockReturnValueOnce(response.promise);
    vi.mocked(organizationProjects).mockImplementation(async (id) =>
      id === 'org' ? [] : [otherProject]
    );
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } }
    });
    // Inspect the mutation's cache updates before a refresh can replace them.
    vi.spyOn(client, 'invalidateQueries').mockResolvedValue();
    const root = [...projectKeys.root, 'organizations'];
    const target = document.createElement('div');
    document.body.append(target);
    const component = mount(OrganizationProbe, { target, props: { client } });
    try {
      flushSync();
      await vi.waitFor(() =>
        expect(target.textContent).toContain(
          'You can manage this organization.'
        )
      );
      const input = Array.from(target.querySelectorAll('label'))
        .find((label) => label.textContent?.includes('New project name'))!
        .querySelector('input')!;
      input.value = 'Team';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      flushSync();
      input
        .closest('form')!
        .dispatchEvent(
          new Event('submit', { bubbles: true, cancelable: true })
        );
      await vi.waitFor(() =>
        expect(createOrganizationProject).toHaveBeenCalledWith(
          'org',
          'Team',
          expect.any(String)
        )
      );
      if (switchOrganization) {
        const select = Array.from(target.querySelectorAll('label'))
          .find((label) => label.textContent?.startsWith('Organization'))!
          .querySelector('select')!;
        expect(select.disabled).toBe(false);
        select.value = 'new-org';
        select.dispatchEvent(new Event('change', { bubbles: true }));
        flushSync();
        await vi.waitFor(() =>
          expect(target.textContent).toContain('Branch project')
        );
      }
      response.resolve(createdProject);
      await vi.waitFor(() =>
        expect(target.querySelector('[role="status"]')?.textContent).toBe(
          'Saved.'
        )
      );
      flushSync();
      expect(client.getQueryData([...root, 'org', 'projects'])).toEqual([
        createdProject
      ]);
      const select = Array.from(target.querySelectorAll('label'))
        .find((label) => label.textContent?.startsWith('Organization project'))!
        .querySelector('select')!;
      expect(select.value).toBe(
        switchOrganization ? otherProject.id : createdProject.id
      );
      if (switchOrganization) {
        expect(client.getQueryData([...root, 'new-org', 'projects'])).toEqual([
          otherProject
        ]);
        expect(projectMembers).not.toHaveBeenCalledWith(
          'new-org',
          createdProject.id,
          expect.anything()
        );
      }
    } finally {
      response.resolve(createdProject);
      await unmount(component);
      client.clear();
      target.remove();
      vi.mocked(organizationProjects).mockReset().mockResolvedValue([]);
      state.role = 'viewer';
      state.created = false;
    }
  }
);
