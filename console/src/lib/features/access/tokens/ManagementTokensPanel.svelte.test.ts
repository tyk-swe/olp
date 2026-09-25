// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  createManagementToken,
  listManagementTokenPage,
  revokeManagementToken,
  type ManagementToken,
  type Project
} from '$lib/features/access/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import ManagementTokensProbe from './test/ManagementTokensProbe.svelte';

const role = vi.hoisted(() => ({ current: 'owner' }));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ role: role.current, can: () => role.current === 'owner' })
}));
vi.mock('$lib/features/access/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/api')>()),
  createManagementToken: vi.fn(),
  listManagementTokenPage: vi.fn(),
  revokeManagementToken: vi.fn()
}));
vi.mock('$lib/clipboard', () => ({ copyText: vi.fn(async () => true) }));

const token: ManagementToken = {
  id: '11111111-1111-1111-1111-111111111111',
  lookup_id: 'lookup',
  name: 'deploy-automation',
  scopes: ['read', 'configure'],
  all_projects: true,
  project_ids: [],
  created_by: '22222222-2222-2222-2222-222222222222',
  created_by_email: 'owner@example.com',
  etag: '33333333-3333-3333-3333-333333333333',
  expires_at: new Date(Date.now() + 86400000).toISOString(),
  revoked_at: null,
  created_at: new Date().toISOString()
};

const dialogPrototype = HTMLDialogElement.prototype as unknown as Record<
  string,
  unknown
>;
const originalMethods = new Map<string, PropertyDescriptor | undefined>();
function stub(name: string, implementation: (this: HTMLDialogElement) => void) {
  originalMethods.set(
    name,
    Object.getOwnPropertyDescriptor(HTMLDialogElement.prototype, name)
  );
  dialogPrototype[name] = implementation;
}

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
// Project list pages served by the management API, keyed by request cursor.
let projectPages: Record<
  string,
  { items: Project[]; next_cursor: string | null }
>;
let projectRequests: Request[];

beforeEach(() => {
  vi.resetAllMocks();
  role.current = 'owner';
  stub('showModal', function showModal(this: HTMLDialogElement) {
    this.open = true;
  });
  stub('close', function close(this: HTMLDialogElement) {
    this.open = false;
    this.dispatchEvent(new Event('close'));
  });
  vi.stubGlobal(
    'confirm',
    vi.fn(() => true)
  );
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listManagementTokenPage).mockResolvedValue({
    items: [token],
    nextCursor: null
  });
  projectPages = { '': { items: [], next_cursor: null } };
  projectRequests = captureRequests((request) =>
    jsonResponse(
      projectPages[new URL(request.url).searchParams.get('cursor') ?? '']
    )
  );
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
  for (const [name, descriptor] of originalMethods) {
    if (descriptor)
      Object.defineProperty(HTMLDialogElement.prototype, name, descriptor);
    else delete dialogPrototype[name];
  }
  originalMethods.clear();
});

function render() {
  component = mount(ManagementTokensProbe, {
    target: host,
    props: { client }
  });
  flushSync();
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

it('lists tokens for an owner and revokes with its etag', async () => {
  vi.mocked(revokeManagementToken).mockResolvedValue();
  render();
  await settle();
  expect(host.textContent).toContain('deploy-automation');
  expect(host.textContent).toContain('owner@example.com');
  const revoke = host.querySelector<HTMLButtonElement>('button.danger-button');
  expect(revoke).toBeTruthy();
  revoke!.click();
  await settle();
  expect(confirm).toHaveBeenCalled();
  expect(revokeManagementToken).toHaveBeenCalledWith(token.id, token.etag);
  expect(host.textContent).toContain('Management token revoked.');
});

it('hides token administration from non-owners', async () => {
  role.current = 'developer';
  render();
  await settle();
  expect(host.textContent).toContain(
    'Only owners can manage management tokens.'
  );
  expect(host.querySelector('form')).toBeNull();
  expect(host.querySelector('button.danger-button')).toBeNull();
  expect(listManagementTokenPage).not.toHaveBeenCalled();
});

it('shows the created secret once behind an explicit acknowledgement', async () => {
  vi.mocked(createManagementToken).mockResolvedValue({
    ...token,
    secret: 'olpm_lookup_secret'
  });
  render();
  await settle();
  const input = host.querySelector<HTMLInputElement>('input[type="text"]');
  expect(input).toBeTruthy();
  input!.value = 'nightly sync';
  input!.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(createManagementToken).toHaveBeenCalledWith(
    'nightly sync',
    ['read'],
    expect.any(String),
    undefined
  );
  const dialog = host.querySelector('dialog');
  expect(dialog?.open).toBe(true);
  expect(host.querySelector('.token-secret')?.textContent).toBe(
    'olpm_lookup_secret'
  );
  expect(host.textContent).toContain('displayed once');
  const acknowledge = [...host.querySelectorAll('button')].find(
    (button) => button.textContent === 'I have saved it'
  );
  acknowledge!.click();
  await settle();
  expect(host.querySelector('dialog')).toBeNull();
  expect(host.textContent).not.toContain('olpm_lookup_secret');
});

it('submits selected project ids for a scoped token', async () => {
  const project = {
    id: '44444444-4444-4444-4444-444444444444',
    name: 'Platform',
    etag: 'p1',
    member_count: 2,
    created_by: token.created_by,
    created_by_email: 'owner@example.com',
    created_at: token.created_at,
    updated_at: token.created_at
  };
  projectPages = { '': { items: [project], next_cursor: null } };
  vi.mocked(createManagementToken).mockResolvedValue({
    ...token,
    secret: 'olpm_lookup_secret'
  });
  render();
  await settle();
  const radios = [
    ...host.querySelectorAll<HTMLInputElement>('input[name="token-projects"]')
  ];
  expect(radios.length).toBe(2);
  radios[1].click();
  await settle();
  const projectBox = [
    ...host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  ].find((box) => box.closest('label')?.textContent?.includes('Platform'));
  expect(projectBox).toBeTruthy();
  projectBox!.click();
  await settle();
  const input = host.querySelector<HTMLInputElement>('input[type="text"]');
  input!.value = 'scoped sync';
  input!.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(createManagementToken).toHaveBeenCalledWith(
    'scoped sync',
    ['read'],
    expect.any(String),
    [project.id]
  );
});

it('blocks scoped creation without a selected project', async () => {
  render();
  await settle();
  const radios = [
    ...host.querySelectorAll<HTMLInputElement>('input[name="token-projects"]')
  ];
  radios[1].click();
  await settle();
  const input = host.querySelector<HTMLInputElement>('input[type="text"]');
  input!.value = 'scoped sync';
  input!.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle();
  expect(createManagementToken).not.toHaveBeenCalled();
  expect(host.textContent).toContain('Select at least one project');
});

it('surfaces list failures without token controls', async () => {
  vi.mocked(listManagementTokenPage).mockRejectedValue(
    new Error('management unavailable')
  );
  render();
  await settle();
  const alert = host.querySelector('[role="alert"]');
  expect(alert?.textContent).toContain('management unavailable');
});

it('offers projects from every page when scoping a token', async () => {
  const project = (id: string, name: string): Project => ({
    id,
    name,
    etag: `${id}-etag`,
    member_count: 0,
    created_by: token.created_by,
    created_by_email: 'owner@example.com',
    created_at: token.created_at,
    updated_at: token.created_at
  });
  projectPages = {
    '': {
      items: [project('55555555-5555-5555-5555-555555555555', 'First page')],
      next_cursor: 'projects-2'
    },
    'projects-2': {
      items: [project('66666666-6666-6666-6666-666666666666', 'Later page')],
      next_cursor: null
    }
  };
  render();
  for (let tick = 0; tick < 5; tick += 1) await settle();
  host
    .querySelectorAll<HTMLInputElement>('input[name="token-projects"]')[1]!
    .click();
  await settle();
  const options = [
    ...host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  ].map((box) => box.closest('label')?.textContent);
  expect(options).toEqual(expect.arrayContaining(['First page', 'Later page']));
  expect(
    projectRequests.map((request) =>
      new URL(request.url).searchParams.get('cursor')
    )
  ).toEqual([null, 'projects-2']);
});
