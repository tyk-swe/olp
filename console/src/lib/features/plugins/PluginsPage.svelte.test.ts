// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import {
  approvePlugin,
  installPlugin,
  listPlugins,
  uninstallPlugin,
  type Plugin
} from '$lib/features/plugins/api';
import PluginsProbe from './test/PluginsProbe.svelte';

const role = vi.hoisted(() => ({ current: 'owner' }));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({
    role: role.current,
    can: (capability: string) =>
      role.current === 'owner' || capability === 'configuration.read'
  })
}));
vi.mock('$lib/features/plugins/api', async (original) => ({
  ...(await original<typeof import('$lib/features/plugins/api')>()),
  approvePlugin: vi.fn(),
  installPlugin: vi.fn(),
  listPlugins: vi.fn(),
  uninstallPlugin: vi.fn()
}));

const pending: Plugin = {
  digest: 'a'.repeat(64),
  abi_version: 1,
  size_bytes: 4_738_008,
  executable: null,
  manifest: {
    name: 'reference',
    version: '0.1.0',
    description: 'Reference plugin for the OpenLLMProxy plugin SDK.',
    origins: ['https://api.example.com', 'https://login.example.com'],
    profiles: [
      {
        id: 'reference-chat',
        label: 'Reference Chat Completions',
        dialect: 'openai-chat',
        hosting: {
          address: 'https://api.example.com/v1',
          headers: { Authorization: 'Token {credential}' }
        }
      }
    ]
  },
  installed_by: '22222222-2222-2222-2222-222222222222',
  installed_by_email: 'owner@example.com',
  installed_at: '2026-09-27T06:00:00Z',
  approved_by: null,
  approved_by_email: null,
  approved_at: null,
  etag: '33333333-3333-3333-3333-333333333333'
};
const approved: Plugin = {
  ...pending,
  approved_by: pending.installed_by,
  approved_by_email: 'owner@example.com',
  approved_at: '2026-09-27T06:05:00Z',
  etag: '44444444-4444-4444-4444-444444444444'
};

function listed(items: Plugin[], unconfinedPluginsEnabled = false) {
  return { items, unconfined_plugins_enabled: unconfinedPluginsEnabled };
}

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  role.current = 'owner';
  vi.stubGlobal(
    'confirm',
    vi.fn(() => true)
  );
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
});

function render() {
  component = mount(PluginsProbe, { target: host, props: { client } });
  flushSync();
}

async function settle() {
  for (let i = 0; i < 3; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function button(name: string): HTMLButtonElement {
  const found = [...host.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === name
  );
  if (!found) throw new Error(`no ${name} button`);
  return found;
}

function upload(file: File) {
  const input = host.querySelector<HTMLInputElement>('input[type="file"]')!;
  Object.defineProperty(input, 'files', { value: [file], configurable: true });
  input.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  host.querySelector('form')!.requestSubmit();
}

it('installs an uploaded module and shows what it declares', async () => {
  vi.mocked(listPlugins)
    .mockResolvedValueOnce(listed([]))
    .mockResolvedValue(listed([pending]));
  vi.mocked(installPlugin).mockResolvedValue({
    plugin: pending,
    created: true
  });
  render();
  await settle();
  expect(host.textContent).toContain('No plugins installed');

  const module = new File([new Uint8Array([0, 97, 115, 109])], 'ref.wasm');
  upload(module);
  await settle();

  expect(installPlugin).toHaveBeenCalledWith(module);
  expect(host.textContent).toContain('Installed reference 0.1.0.');
  expect(host.textContent).toContain('Pending approval');
  expect(host.textContent).toContain('aaaaaaaaaaaa');
  expect(host.textContent).toContain('reference-chat');
  expect(host.textContent).toContain('openai-chat');
  expect(host.textContent).toContain('https://login.example.com');
  expect(host.textContent).toContain('https://api.example.com/v1');
});

it('explains a typed refusal with the manifest field it concerns', async () => {
  vi.mocked(listPlugins).mockResolvedValue(listed([]));
  vi.mocked(installPlugin).mockRejectedValue(
    new ApiProblem({
      type: 'https://openllmproxy.dev/problems/plugin_dialect_unknown',
      title: 'Unprocessable Entity',
      status: 422,
      detail: 'OLP has no dialect named "acme".',
      errors: {
        'manifest.profiles[0].dialect': [
          { code: 'plugin_dialect_unknown', message: 'OLP has no dialect.' }
        ]
      }
    })
  );
  render();
  await settle();
  upload(new File(['module'], 'acme.wasm'));
  await settle();
  expect(host.querySelector('[role="alert"]')?.textContent).toContain(
    'OLP has no dialect named "acme". (manifest.profiles[0].dialect)'
  );
});

it('approves exactly the reviewed origins, then uninstalls', async () => {
  vi.mocked(listPlugins)
    .mockResolvedValueOnce(listed([pending]))
    .mockResolvedValueOnce(listed([approved]))
    .mockResolvedValue(listed([]));
  vi.mocked(approvePlugin).mockResolvedValue(approved);
  vi.mocked(uninstallPlugin).mockResolvedValue();
  render();
  await settle();

  button('Review and approve').click();
  flushSync();
  const review = host.querySelector('.approval')!;
  expect(
    [...review.querySelectorAll('li')].map((item) => item.textContent)
  ).toEqual(pending.manifest.origins);
  button('Approve origins').click();
  await settle();
  expect(approvePlugin).toHaveBeenCalledWith(pending);
  expect(host.textContent).toContain('Approved reference 0.1.0.');
  expect(host.querySelector('.badge')?.textContent).toBe('Approved');
  expect(
    [...host.querySelectorAll('button')].some(
      (item) => item.textContent?.trim() === 'Review and approve'
    )
  ).toBe(false);

  button('Uninstall').click();
  await settle();
  expect(confirm).toHaveBeenCalled();
  expect(uninstallPlugin).toHaveBeenCalledWith(approved);
  expect(host.textContent).toContain('No plugins installed');
});

it('explains an uninstall refused because providers pin the plugin', async () => {
  vi.mocked(listPlugins).mockResolvedValue(listed([approved]));
  vi.mocked(uninstallPlugin).mockRejectedValue(
    new ApiProblem({
      type: 'https://openllmproxy.dev/problems/plugin_pinned',
      title: 'Conflict',
      status: 409,
      detail:
        'Providers Reference upstream pin this plugin in a draft or published revision.'
    })
  );
  render();
  await settle();
  button('Uninstall').click();
  await settle();
  expect(host.querySelector('[role="alert"]')?.textContent).toContain(
    'Providers Reference upstream pin this plugin'
  );
  expect(host.querySelectorAll('article')).toHaveLength(1);
});

it('keeps an uninstall the owner cancels', async () => {
  vi.mocked(listPlugins).mockResolvedValue(listed([pending]));
  vi.mocked(confirm).mockReturnValue(false);
  render();
  await settle();
  button('Uninstall').click();
  await settle();
  expect(uninstallPlugin).not.toHaveBeenCalled();
});

it('shows plugins read-only to other roles', async () => {
  role.current = 'viewer';
  vi.mocked(listPlugins).mockResolvedValue(listed([pending]));
  render();
  await settle();
  expect(host.textContent).toContain(
    'Only owners can install, approve or uninstall plugins.'
  );
  expect(host.textContent).toContain('reference-chat');
  expect(host.textContent).toContain('https://api.example.com');
  expect(host.querySelector('form')).toBeNull();
  expect(host.querySelectorAll('button')).toHaveLength(0);
});

it('marks a permitted unconfined plugin and shows the tier enabled', async () => {
  const unconfined: Plugin = { ...approved, executable: 'reference' };
  vi.mocked(listPlugins).mockResolvedValue(listed([unconfined], true));
  role.current = 'viewer';
  render();
  await settle();
  const card = host.querySelector('article')!;
  expect(
    [...card.querySelectorAll('.badge')].map((badge) => badge.textContent)
  ).toEqual(['Unconfined', 'Permitted']);
  expect(card.textContent).toContain('Executable');
  expect(card.textContent).toContain('reference');
  const tier = host.querySelector('.unconfined')!;
  expect(tier.querySelector('.badge')?.textContent).toBe('Enabled');
  expect(tier.textContent).toContain(
    'Only owners can review and permit unconfined plugins.'
  );
});
