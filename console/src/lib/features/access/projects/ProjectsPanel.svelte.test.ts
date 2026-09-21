// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  createProject,
  listProjectMemberPage,
  listProjectPage,
  listUsers,
  putProjectMember,
  removeProjectMember,
  renameProject,
  type Project,
  type ProjectMember,
  type User
} from '$lib/features/access/api';
import ProjectsProbe from './test/ProjectsProbe.svelte';

vi.mock('$lib/features/access/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/api')>()),
  createProject: vi.fn(),
  listProjectMemberPage: vi.fn(),
  listProjectPage: vi.fn(),
  listUsers: vi.fn(),
  putProjectMember: vi.fn(),
  removeProjectMember: vi.fn(),
  renameProject: vi.fn()
}));

const project: Project = {
  id: '44444444-4444-4444-4444-444444444444',
  name: 'Platform',
  etag: 'project-etag-1',
  member_count: 1,
  created_by: '22222222-2222-2222-2222-222222222222',
  created_by_email: 'owner@example.com',
  created_at: '2026-07-12T12:00:00Z',
  updated_at: '2026-07-12T12:00:00Z'
};

const member: ProjectMember = {
  user_id: '33333333-3333-3333-3333-333333333333',
  email: 'operator@example.com',
  display_name: 'Operator',
  role: 'operator',
  active: true,
  project_role: 'manager',
  added_by: project.created_by,
  added_by_email: 'owner@example.com',
  created_at: '2026-07-12T12:00:00Z'
};

const candidate: User = {
  id: '55555555-5555-5555-5555-555555555555',
  email: 'dev@example.com',
  display_name: 'Developer',
  role: 'developer',
  access_scope: 'global',
  active: true,
  etag: 'u1',
  created_at: '2026-07-12T12:00:00Z',
  updated_at: '2026-07-12T12:00:00Z'
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listProjectPage).mockResolvedValue({
    items: [project],
    nextCursor: null
  });
  vi.mocked(listProjectMemberPage).mockResolvedValue({
    items: [member],
    nextCursor: null
  });
  vi.mocked(listUsers).mockResolvedValue([candidate]);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function render() {
  component = mount(ProjectsProbe, { target: host, props: { client } });
  flushSync();
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

it('creates a project and reveals its member controls', async () => {
  vi.mocked(createProject).mockResolvedValue(project);
  render();
  await settle();
  expect(host.textContent).toContain('Platform');
  const input = host.querySelector<HTMLInputElement>('#project-name');
  input!.value = 'Data science';
  input!.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector<HTMLFormElement>('form[aria-label="Create project"]')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(createProject).toHaveBeenCalledWith('Data science');
});

it('adds, re-roles, and removes a member with the project etag', async () => {
  vi.mocked(putProjectMember).mockResolvedValue(member);
  render();
  await settle();
  const membersButton = [...host.querySelectorAll('button')].find(
    (button) => button.textContent === 'Members'
  );
  membersButton!.click();
  await settle();
  expect(host.textContent).toContain('operator@example.com');
  const userSelect = host.querySelector<HTMLSelectElement>('#member-user');
  userSelect!.value = candidate.id;
  userSelect!.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const addButton = [...host.querySelectorAll('button')].find(
    (button) => button.textContent === 'Add member'
  );
  addButton!.click();
  await settle();
  expect(putProjectMember).toHaveBeenCalledWith(
    expect.objectContaining({ id: project.id }),
    candidate.id,
    'viewer'
  );

  vi.mocked(removeProjectMember).mockResolvedValue();
  const remove = [...host.querySelectorAll('button')].find(
    (button) => button.textContent === 'Remove'
  );
  remove!.click();
  await settle();
  expect(removeProjectMember).toHaveBeenCalledWith(
    expect.objectContaining({ id: project.id }),
    member.user_id
  );
});

it('renames a project with the loaded etag', async () => {
  vi.mocked(renameProject).mockResolvedValue({
    ...project,
    name: 'Platform v2'
  });
  render();
  await settle();
  const membersButton = [...host.querySelectorAll('button')].find(
    (button) => button.textContent === 'Members'
  );
  membersButton!.click();
  await settle();
  const input = host.querySelector<HTMLInputElement>('#project-rename');
  input!.value = 'Platform v2';
  input!.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  const rename = [...host.querySelectorAll('button')].find(
    (button) => button.textContent === 'Rename'
  );
  rename!.click();
  await settle();
  expect(renameProject).toHaveBeenCalledWith(
    expect.objectContaining({ id: project.id, etag: 'project-etag-1' }),
    'Platform v2'
  );
});

it('surfaces project list failures', async () => {
  vi.mocked(listProjectPage).mockRejectedValue(new Error('projects down'));
  render();
  await settle();
  expect(host.querySelector('[role="alert"]')?.textContent).toContain(
    'Projects are unavailable'
  );
});
