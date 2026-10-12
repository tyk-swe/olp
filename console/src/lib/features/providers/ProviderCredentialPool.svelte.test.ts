import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Provider } from './api/providers';
import {
  listCredentialSlots,
  putCredentialSlot,
  type CredentialSlot
} from './api/credentials';
import ProviderCredentialPoolProbe from './test/ProviderCredentialPoolProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./api/credentials', async (original) => ({
  ...(await original<typeof import('./api/credentials')>()),
  listCredentialSlots: vi.fn(),
  putCredentialSlot: vi.fn()
}));

const slot: CredentialSlot = {
  id: 'slot-first',
  name: 'First',
  enabled: true,
  priority: 0,
  weight: 1,
  credential_version_id: 'credential-1',
  allowed_models: [],
  allowed_routes: [],
  allowed_api_keys: [],
  requests_per_minute: null,
  tokens_per_minute: null,
  max_concurrency: null
};
const otherSlot = { ...slot, id: 'slot-second', name: 'Second' };
const provider = { id: 'provider-1', etag: 'provider-etag-1' } as Provider;
let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
let onChanged: ReturnType<typeof vi.fn<() => Promise<void>>>;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listCredentialSlots).mockResolvedValue({
    etag: 'slots-etag-1',
    health: {},
    items: [slot, otherSlot]
  });
  onChanged = vi.fn<() => Promise<void>>(async () => {});
});
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});
async function render() {
  component = mount(ProviderCredentialPoolProbe, {
    target: host,
    props: { client, provider, onChanged }
  });
  await vi.waitFor(() => expect(host.textContent).toContain('First'));
  flushSync();
}
function button(text: string, index = 0) {
  return [...host.querySelectorAll<HTMLButtonElement>('button')].filter(
    (b) => b.textContent?.trim() === text
  )[index]!;
}
function edit(index = 0) {
  button('Edit / rotate', index).click();
  flushSync();
}
function typeSecret(value = 'one-draft-secret') {
  const input = host.querySelector<HTMLInputElement>('input[type="password"]')!;
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}
async function save() {
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();
  await vi.waitFor(() =>
    expect(button('Save credential').matches(':disabled')).toBe(false)
  );
  flushSync();
}

it.each(['cancel', 'another slot'])(
  'retires a secret receipt at %s while unchanged retries keep identity',
  async (boundary) => {
    vi.mocked(putCredentialSlot).mockRejectedValue(
      new TypeError('Response lost')
    );
    await render();
    edit();
    typeSecret();
    await save();
    await save();
    const first = vi.mocked(putCredentialSlot).mock.calls[0];
    expect(vi.mocked(putCredentialSlot).mock.calls[1]).toEqual(first);
    if (boundary === 'cancel') {
      button('Cancel').click();
      flushSync();
    } else {
      edit(1);
      expect(
        host.querySelector<HTMLInputElement>('input[type="password"]')!.value
      ).toBe('');
    }
    edit();
    typeSecret();
    await save();
    const fresh = vi.mocked(putCredentialSlot).mock.calls[2];
    expect(fresh.slice(0, 3)).toEqual(first.slice(0, 3));
    expect(fresh[3]).not.toBe(first[3]);
  }
);

it('ends the secret draft after a confirmed write even if observing the provider fails', async () => {
  vi.mocked(putCredentialSlot).mockResolvedValue(undefined);
  onChanged.mockRejectedValue(new Error('Refresh unavailable'));
  await render();
  edit();
  typeSecret();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'Credential staged, but refreshing failed.'
    )
  );
  expect(host.querySelector('form')).toBeNull();
  expect(host.textContent).toContain('Credential staged.');
  expect(putCredentialSlot).toHaveBeenCalledTimes(1);
});

it('starts a new intent for a changed secret while preserving a repeated unchanged retry', async () => {
  vi.mocked(putCredentialSlot).mockRejectedValue(
    new TypeError('Response lost')
  );
  await render();
  edit();
  typeSecret();
  await save();
  const first = vi.mocked(putCredentialSlot).mock.calls[0];
  typeSecret('changed-secret');
  await save();
  await save();
  const changed = vi.mocked(putCredentialSlot).mock.calls[1];
  expect(changed[2]).not.toEqual(first[2]);
  expect(changed[3]).not.toBe(first[3]);
  expect(vi.mocked(putCredentialSlot).mock.calls[2]).toEqual(changed);
});
