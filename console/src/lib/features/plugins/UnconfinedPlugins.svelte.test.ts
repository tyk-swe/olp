// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { replaceState } from '$app/navigation';
import { ApiProblem } from '$lib/api/http';
import {
  beginOidcReauthentication,
  listOidcIdentities,
  reauthenticateWithPassword,
  type OidcIdentityList
} from '$lib/features/access/profile/api';
import {
  listUnconfinedExecutables,
  permitUnconfinedPlugin,
  reviewUnconfinedExecutable,
  type Plugin,
  type UnconfinedExecutable,
  type UnconfinedExecutableReview
} from '$lib/features/plugins/api';
import UnconfinedPluginsProbe from './test/UnconfinedPluginsProbe.svelte';

vi.mock('$app/navigation', () => ({ replaceState: vi.fn() }));
vi.mock('$lib/features/access/profile/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/profile/api')>()),
  beginOidcReauthentication: vi.fn(),
  listOidcIdentities: vi.fn(),
  reauthenticateWithPassword: vi.fn()
}));
vi.mock('$lib/features/plugins/api', async (original) => ({
  ...(await original<typeof import('$lib/features/plugins/api')>()),
  listUnconfinedExecutables: vi.fn(),
  permitUnconfinedPlugin: vi.fn(),
  reviewUnconfinedExecutable: vi.fn()
}));

const executable: UnconfinedExecutable = {
  name: 'reference',
  digest: 'c'.repeat(64),
  size_bytes: 9_437_184,
  permitted: false
};
const review: UnconfinedExecutableReview = {
  ...executable,
  abi_version: 1,
  manifest: {
    name: 'reference',
    version: '0.1.0',
    description: 'Reference plugin for the OpenLLMProxy plugin SDK.',
    origins: ['https://api.example.com'],
    profiles: [
      {
        id: 'reference-signed-chat',
        label: 'Reference Signed Chat Completions',
        dialect: 'openai-chat',
        hosting: { address: 'https://api.example.com/v1' },
        signing: true
      }
    ]
  }
};
const permitted: Plugin = {
  digest: executable.digest,
  abi_version: 1,
  size_bytes: executable.size_bytes,
  executable: 'reference',
  manifest: review.manifest,
  installed_by: '22222222-2222-2222-2222-222222222222',
  installed_by_email: 'owner@example.com',
  installed_at: '2026-09-27T06:00:00Z',
  approved_by: '22222222-2222-2222-2222-222222222222',
  approved_by_email: 'owner@example.com',
  approved_at: '2026-09-27T06:00:00Z',
  etag: '33333333-3333-3333-3333-333333333333'
};
const identities: OidcIdentityList = {
  items: [],
  has_local_password: true,
  linking_available: false,
  oidc_reauthentication_available: false
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
let onPermitted: ReturnType<typeof vi.fn<() => Promise<unknown>>>;
let assign: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.resetAllMocks();
  // jsdom has no modal dialogs.
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', {
    configurable: true,
    value(this: HTMLDialogElement) {
      this.open = true;
    }
  });
  onPermitted = vi.fn(async () => undefined);
  assign = vi.fn();
  vi.stubGlobal('location', { ...window.location, search: '', assign });
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
  document.querySelectorAll('dialog').forEach((dialog) => dialog.remove());
  vi.unstubAllGlobals();
  delete (HTMLDialogElement.prototype as Partial<HTMLDialogElement>).showModal;
});

function render(enabled = true, canManage = true) {
  component = mount(UnconfinedPluginsProbe, {
    target: host,
    props: { client, enabled, canManage, onPermitted }
  });
  flushSync();
}

async function settle() {
  for (let i = 0; i < 4; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function button(name: string): HTMLButtonElement {
  const found = [...document.querySelectorAll('button')].find(
    (candidate) =>
      candidate.textContent?.trim() === name ||
      candidate.getAttribute('aria-label') === name
  );
  if (!found) throw new Error(`no ${name} button`);
  return found;
}

function acknowledge() {
  const checkbox = host.querySelector<HTMLInputElement>(
    '.acknowledge input[type="checkbox"]'
  )!;
  checkbox.click();
  flushSync();
}

async function reviewReference() {
  vi.mocked(listUnconfinedExecutables).mockResolvedValue([executable]);
  vi.mocked(reviewUnconfinedExecutable).mockResolvedValue(review);
  render();
  await settle();
  button('Review reference').click();
  await settle();
}

it('shows a disabled tier and lists nothing', async () => {
  render(false);
  await settle();
  expect(host.querySelector('.badge')?.textContent).toBe('Disabled');
  expect(host.textContent).toContain('OLP_UNCONFINED_PLUGIN_DIR');
  expect(listUnconfinedExecutables).not.toHaveBeenCalled();
  expect(host.querySelectorAll('button')).toHaveLength(0);
});

it('shows other roles the tier without its executables', async () => {
  render(true, false);
  await settle();
  expect(host.querySelector('.badge')?.textContent).toBe('Enabled');
  expect(host.textContent).toContain(
    'Only owners can review and permit unconfined plugins.'
  );
  expect(listUnconfinedExecutables).not.toHaveBeenCalled();
});

it('reviews an executable with the high-risk warning before permitting it', async () => {
  await reviewReference();
  expect(reviewUnconfinedExecutable).toHaveBeenCalledWith('reference');
  const panel = host.querySelector('.review')!;
  expect(panel.textContent).toContain('Review reference 0.1.0');
  expect(panel.textContent).toContain('reference-signed-chat');
  expect(panel.textContent).toContain('https://api.example.com');
  expect(panel.textContent).toContain(executable.digest);
  expect(panel.querySelector('.risk')?.textContent).toContain('High risk.');
  expect(button('Permit unconfined plugin').disabled).toBe(true);
  acknowledge();
  expect(button('Permit unconfined plugin').disabled).toBe(false);
});

it('permits after the owner confirms their password', async () => {
  vi.mocked(listOidcIdentities).mockResolvedValue(identities);
  vi.mocked(reauthenticateWithPassword).mockResolvedValue();
  vi.mocked(permitUnconfinedPlugin).mockResolvedValue(permitted);
  await reviewReference();
  acknowledge();
  button('Permit unconfined plugin').click();
  await settle();
  expect(permitUnconfinedPlugin).not.toHaveBeenCalled();
  const dialog = document.querySelector('dialog')!;
  expect(dialog.textContent).toContain('Confirm the permission');
  const password = dialog.querySelector<HTMLInputElement>('#reauth-password')!;
  password.value = 'correct horse';
  password.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  dialog.querySelector('form')!.requestSubmit();
  await settle();
  expect(reauthenticateWithPassword).toHaveBeenCalledWith(
    'correct horse',
    'plugin_permit'
  );
  expect(permitUnconfinedPlugin).toHaveBeenCalledWith(review);
  expect(onPermitted).toHaveBeenCalled();
  expect(host.textContent).toContain('Permitted reference 0.1.0.');
  expect(host.querySelector('.review')).toBeNull();
  expect(document.querySelector('dialog')).toBeNull();
});

it('verifies an owner without a password through single sign-on', async () => {
  vi.mocked(listOidcIdentities).mockResolvedValue({
    ...identities,
    has_local_password: false
  });
  vi.mocked(beginOidcReauthentication).mockResolvedValue(
    'https://idp.example.com/authorize'
  );
  await reviewReference();
  acknowledge();
  button('Permit unconfined plugin').click();
  await settle();
  expect(beginOidcReauthentication).toHaveBeenCalledWith('plugin_permit');
  expect(assign).toHaveBeenCalledWith('https://idp.example.com/authorize');
  expect(permitUnconfinedPlugin).not.toHaveBeenCalled();
});

it('permits without asking again once single sign-on verified the owner', async () => {
  vi.stubGlobal('location', {
    ...window.location,
    search: '?reauthenticated=plugin_permit',
    assign
  });
  vi.mocked(permitUnconfinedPlugin).mockResolvedValue(permitted);
  await reviewReference();
  expect(replaceState).toHaveBeenCalled();
  acknowledge();
  button('Permit unconfined plugin').click();
  await settle();
  expect(listOidcIdentities).not.toHaveBeenCalled();
  expect(permitUnconfinedPlugin).toHaveBeenCalledWith(review);
});

it('explains an executable OLP refuses to run', async () => {
  vi.mocked(listUnconfinedExecutables).mockResolvedValue([executable]);
  vi.mocked(reviewUnconfinedExecutable).mockRejectedValue(
    new ApiProblem({
      type: 'https://openllmproxy.dev/problems/plugin_abi_unsupported',
      title: 'Unprocessable Entity',
      status: 422,
      detail: 'The executable was built for plugin ABI 2; this OLP runs ABI 1.'
    })
  );
  render();
  await settle();
  button('Review reference').click();
  await settle();
  expect(host.querySelector('[role="alert"]')?.textContent).toContain(
    'built for plugin ABI 2'
  );
  expect(host.querySelector('.review')).toBeNull();
});
