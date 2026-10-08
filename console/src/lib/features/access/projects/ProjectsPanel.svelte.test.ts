// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  createProject,
  getProjectEndUserPolicy,
  listAllProjectMembers,
  listProjectMemberPage,
  listProjectPage,
  putProjectMember,
  removeProjectMember,
  renameProject,
  type Project,
  type ProjectMember
} from '$lib/features/access/projects/api';
import { listUsers, type User } from '$lib/features/access/users/api';
import ProjectsProbe from './test/ProjectsProbe.svelte';

vi.mock('$lib/features/access/budgets/api', () => ({
  getBudget: vi.fn().mockResolvedValue({
    policy: null,
    etag: '00000000-0000-4000-8000-000000000001',
    usage: {
      daily: { accrued: '0', window_ends_at: '2026-10-08T00:00:00Z' },
      monthly: { accrued: '0', window_ends_at: '2026-11-01T00:00:00Z' },
      unpriced_attempts: 0
    }
  }),
  putBudget: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/projects/api')>()),
  createProject: vi.fn(),
  getProjectAttributionBudgets: vi.fn(async () => ({
    budgets: {},
    usage: {},
    etag: '00000000-0000-4000-8000-000000000001'
  })),
  getProjectLimitTemplates: vi.fn(async () => ({
    templates: {},
    etag: '44444444-4444-4444-4444-444444444444'
  })),
  getProjectRouteGroups: vi.fn().mockResolvedValue({
    groups: {},
    etag: '00000000-0000-0000-0000-000000000001'
  }),
  getProjectAttributionPolicy: vi.fn(() =>
    Promise.resolve({
      policy: null,
      etag: '00000000-0000-4000-8000-000000000001'
    })
  ),
  getProjectEndUserPolicy: vi.fn(),
  listAllProjectMembers: vi.fn(),
  listProjectMemberPage: vi.fn(),
  listProjectPage: vi.fn(),
  putProjectMember: vi.fn(),
  removeProjectMember: vi.fn(),
  renameProject: vi.fn()
}));
vi.mock('$lib/features/access/users/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/users/api')>()),
  listUsers: vi.fn()
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
  vi.mocked(getProjectEndUserPolicy).mockResolvedValue({
    policy: null,
    etag: project.etag
  });
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
  vi.mocked(listAllProjectMembers).mockResolvedValue([member]);
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

function button(text: string) {
  return [...host.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === text
  )!;
}

function navButton(label: string, text: string) {
  return [...host.querySelectorAll(`nav[aria-label="${label}"] button`)].find(
    (candidate) => candidate.textContent === text
  ) as HTMLButtonElement;
}

it('pages projects forward and back without losing the first page', async () => {
  const second: Project = {
    ...project,
    id: '66666666-6666-6666-6666-666666666666',
    name: 'Research'
  };
  vi.mocked(listProjectPage).mockImplementation(async (cursor) =>
    cursor === 'c1'
      ? { items: [second], nextCursor: null }
      : { items: [project], nextCursor: 'c1' }
  );
  render();
  await settle();
  navButton('Project pages', 'Next').click();
  await settle();
  expect(host.textContent).toContain('Research');
  expect(host.textContent).not.toContain('Platform');
  navButton('Project pages', 'Previous').click();
  await settle();
  expect(host.textContent).toContain('Platform');
  expect(host.textContent).not.toContain('Research');
});

it('pages members forward and back', async () => {
  const later: ProjectMember = {
    ...member,
    user_id: '77777777-7777-7777-7777-777777777777',
    email: 'later@example.com'
  };
  vi.mocked(listProjectMemberPage).mockImplementation(async (_id, cursor) =>
    cursor === 'm1'
      ? { items: [later], nextCursor: null }
      : { items: [member], nextCursor: 'm1' }
  );
  render();
  await settle();
  button('Members').click();
  await settle();
  navButton('Member pages', 'Next').click();
  await settle();
  expect(host.textContent).toContain('later@example.com');
  expect(host.textContent).not.toContain('operator@example.com');
  navButton('Member pages', 'Previous').click();
  await settle();
  expect(host.textContent).toContain('operator@example.com');
});

it('omits members on other pages from the add-member picker', async () => {
  const manager: User = {
    ...candidate,
    id: '88888888-8888-8888-8888-888888888888',
    email: 'mgr@example.com',
    display_name: 'Manager'
  };
  vi.mocked(listProjectMemberPage).mockResolvedValue({
    items: [member],
    nextCursor: 'm1'
  });
  vi.mocked(listAllProjectMembers).mockResolvedValue([
    member,
    { ...member, user_id: manager.id, email: manager.email }
  ]);
  vi.mocked(listUsers).mockResolvedValue([candidate, manager]);
  render();
  await settle();
  button('Members').click();
  await settle();
  const options = [
    ...host.querySelectorAll<HTMLOptionElement>('#member-user option')
  ].map((option) => option.value);
  expect(options).toContain(candidate.id);
  expect(options).not.toContain(manager.id);
});

it('explains and retries a failed current-members load for the add form', async () => {
  vi.mocked(listAllProjectMembers).mockRejectedValueOnce(
    new Error('members walk failed')
  );
  render();
  await settle();
  button('Members').click();
  await settle();
  expect(host.textContent).toContain('operator@example.com');
  const alert = [...host.querySelectorAll('[role="alert"]')].find((node) =>
    node.textContent?.includes('Current members could not be loaded')
  );
  expect(alert).toBeDefined();
  expect((button('Add member') as HTMLButtonElement).disabled).toBe(true);
  alert!.querySelector<HTMLButtonElement>('button')!.click();
  await settle();
  expect(listAllProjectMembers).toHaveBeenCalledTimes(2);
  expect(host.textContent).not.toContain('Current members could not be loaded');
  const options = [
    ...host.querySelectorAll<HTMLOptionElement>('#member-user option')
  ].map((option) => option.value);
  expect(options).toContain(candidate.id);
});

it('opens a created project with a fresh member cursor and rename value', async () => {
  const created: Project = {
    ...project,
    id: '99999999-9999-9999-9999-999999999999',
    name: 'Data science'
  };
  vi.mocked(listProjectMemberPage).mockResolvedValue({
    items: [member],
    nextCursor: 'cursor-a'
  });
  vi.mocked(createProject).mockResolvedValue(created);
  render();
  await settle();
  button('Members').click();
  await settle();
  navButton('Member pages', 'Next').click();
  await settle();
  expect(listProjectMemberPage).toHaveBeenCalledWith(
    project.id,
    'cursor-a',
    expect.anything()
  );
  vi.mocked(listProjectPage).mockResolvedValue({
    items: [project, created],
    nextCursor: null
  });
  const input = host.querySelector<HTMLInputElement>('#project-name')!;
  input.value = 'Data science';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector<HTMLFormElement>('form[aria-label="Create project"]')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(listProjectMemberPage).toHaveBeenLastCalledWith(
    created.id,
    undefined,
    expect.anything()
  );
  expect(host.querySelector<HTMLInputElement>('#project-rename')!.value).toBe(
    'Data science'
  );
});
