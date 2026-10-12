import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { providerKeys } from './providerKeys';
import { sessionKeys } from '$lib/features/access/session/sessionKeys';
import { projectKeys } from '$lib/features/access/projects/projectKeys';
import {
  activateProvider,
  disableProvider,
  restoreProviderAsDraft,
  probeProvider,
  updateProvider,
  type Provider
} from './api/providers';
import { rotateProviderCredential } from './api/credentials';
import { listProviderModelPage, certifyProviderModel } from './api/models';
import DraftEditorProbe from '$lib/forms/test/DraftEditorProbe.svelte';
import {
  provider as baseProvider,
  providerSpec
} from '$lib/forms/test/draftFixtures';

vi.mock('$app/navigation', () => ({ beforeNavigate: vi.fn(), goto: vi.fn() }));
vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/providers/provider-a') }
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./api/providers', async (original) => ({
  ...(await original<typeof import('./api/providers')>()),
  activateProvider: vi.fn(),
  disableProvider: vi.fn(),
  restoreProviderAsDraft: vi.fn(),
  updateProvider: vi.fn(),
  probeProvider: vi.fn()
}));
vi.mock('./api/credentials', async (original) => ({
  ...(await original<typeof import('./api/credentials')>()),
  rotateProviderCredential: vi.fn()
}));
vi.mock('./api/models', async (original) => ({
  ...(await original<typeof import('./api/models')>()),
  listProviderModelPage: vi.fn(),
  certifyProviderModel: vi.fn()
}));

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
let provider: Provider;

function page(provider: Provider) {
  return { provider, items: [], nextCursor: null };
}
function seed(next: Provider) {
  provider = next;
  client.setQueryData(providerKeys.slots(next.id, next.etag), []);
  client.setQueryData(providerKeys.bulkModels(next.id, next.etag), []);
  client.setQueryData(providerKeys.models(next.id), page(next));
  flushSync();
}

beforeEach(() => {
  vi.resetAllMocks();
  authLifecycle.establishSession({
    user: {
      id: 'owner',
      email: 'owner@example.com',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'csrf'
  });
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(sessionKeys.serviceCapabilities, {
    local_login_enabled: true,
    oidc_login_enabled: false
  });
  client.setQueryData(projectKeys.memberships, []);
  client.setQueryData(providerKeys.profiles(), []);
  client.setQueryData(providerKeys.vendors(), []);
  client.setQueryData(providerKeys.configurationSchemas(), {});
  client.setQueryData(providerKeys.kinds(), [providerSpec]);
  client.setQueryData(providerKeys.enabledModels(), []);
  client.setQueryData(providerKeys.credentials(baseProvider.id), []);
  client.setQueryData(providerKeys.networkCredentials(baseProvider.id), []);
  client.setQueryData(providerKeys.revisions(baseProvider.id), {
    items: [],
    nextCursor: null
  });
  client.setQueryData(providerKeys.capabilityOptions('openai'), {
    capabilities: []
  });
  seed({
    ...baseProvider,
    connector_ready: true,
    model_count: 1,
    enabled_model_count: 1,
    capability_count: 1,
    certified_capability_count: 1,
    last_probe_at: baseProvider.updated_at,
    last_probe_status: 'succeeded'
  });
  vi.mocked(listProviderModelPage).mockImplementation(async () =>
    page(provider)
  );
  vi.mocked(activateProvider).mockResolvedValue(7);
  vi.mocked(disableProvider).mockResolvedValue(8);
  vi.mocked(restoreProviderAsDraft).mockResolvedValue(provider);
  vi.mocked(rotateProviderCredential).mockResolvedValue();
  vi.spyOn(window, 'confirm').mockReturnValue(true);
});
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  await authLifecycle.principalInvalidated();
  client.clear();
  host.remove();
  vi.restoreAllMocks();
});
async function render() {
  component = mount(DraftEditorProbe, {
    target: host,
    props: { client, kind: 'provider' }
  });
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('#detail-name')).not.toBeNull();
  });
}
function button(label: string) {
  const value = [...host.querySelectorAll('button')].find(
    (node) => node.textContent?.trim() === label
  );
  if (!value) throw new Error(`Missing button: ${label}`);
  return value;
}
function click(label: string) {
  button(label).click();
  flushSync();
}

it.each(['activate', 'disable', 'restore'] as const)(
  'retries observation without repeating a confirmed %s',
  async (action) => {
    if (action === 'disable')
      seed({
        ...provider,
        state: 'active',
        active_revision: 1,
        pending_activation: false
      });
    if (action === 'restore') seed({ ...provider, state: 'disabled' });
    await render();
    const label =
      action === 'activate'
        ? 'Activate changes'
        : action === 'disable'
          ? 'Disable provider'
          : 'Restore provider as draft';
    const send =
      action === 'activate'
        ? activateProvider
        : action === 'disable'
          ? disableProvider
          : restoreProviderAsDraft;
    vi.mocked(listProviderModelPage).mockRejectedValueOnce(
      new Error('refresh unavailable')
    );
    click(label);
    await vi.waitFor(() =>
      expect(host.textContent).toContain('refreshing failed')
    );
    expect(button(label).disabled).toBe(true);
    expect(send).toHaveBeenCalledTimes(1);
    click('Retry refresh');
    await vi.waitFor(() =>
      expect(host.textContent).not.toContain('Retry refresh')
    );
    expect(send).toHaveBeenCalledTimes(1);
  }
);

it('replays an ambiguous activation with the original key and ETag after cache refresh', async () => {
  vi.mocked(activateProvider)
    .mockRejectedValueOnce(new TypeError('response lost'))
    .mockResolvedValue(7);
  await render();
  click('Activate changes');
  await vi.waitFor(() => expect(host.textContent).toContain('response lost'));
  const original = vi.mocked(activateProvider).mock.calls[0];
  seed({ ...provider, etag: 'observed-after-commit' });
  click('Activate changes');
  await vi.waitFor(() => expect(activateProvider).toHaveBeenCalledTimes(2));
  expect(vi.mocked(activateProvider).mock.calls[1]).toEqual(original);
  expect(original[0]).toEqual({ id: provider.id, etag: 'v1' });
});

it('starts a new activation after a confirmed deliberate draft save, rather than cache hydration', async () => {
  vi.mocked(activateProvider)
    .mockRejectedValueOnce(new TypeError('old reply lost'))
    .mockResolvedValue(8);
  vi.mocked(updateProvider).mockImplementation(async (_id, _etag, input) => {
    provider = {
      ...provider,
      name: input.name ?? provider.name,
      etag: 'new-draft',
      pending_activation: true
    };
    return provider;
  });
  await render();
  click('Activate changes');
  await vi.waitFor(() => expect(host.textContent).toContain('old reply lost'));
  const previous = vi.mocked(activateProvider).mock.calls[0];
  seed({
    ...provider,
    state: 'active',
    active_revision: 1,
    pending_activation: false,
    etag: 'observed-after-old-commit'
  });
  const name = host.querySelector<HTMLInputElement>('#detail-name')!;
  name.value = 'Deliberately revised draft';
  name.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  click('Save draft');
  await vi.waitFor(() => {
    flushSync();
    expect(button('Activate changes').disabled).toBe(false);
  });
  click('Activate changes');
  await vi.waitFor(() => expect(activateProvider).toHaveBeenCalledTimes(2));
  const next = vi.mocked(activateProvider).mock.calls[1];
  expect(next[0]).toEqual({ id: provider.id, etag: 'new-draft' });
  expect(next[1]).not.toBe(previous[1]);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('runtime generation 8')
  );
});

it('replays a secret rotation with the original key and ETag after cache refresh', async () => {
  vi.mocked(rotateProviderCredential)
    .mockRejectedValueOnce(new TypeError('response lost'))
    .mockResolvedValue();
  await render();
  const input = host.querySelector<HTMLInputElement>(
    '.credential-form input[type="password"]'
  )!;
  input.value = 'pasted-secret';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  expect(button('Stage rotation').disabled).toBe(false);
  button('Stage rotation')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();
  await vi.waitFor(() => expect(host.textContent).toContain('response lost'));
  const original = vi.mocked(rotateProviderCredential).mock.calls[0];
  seed({ ...provider, etag: 'observed-after-commit' });
  button('Stage rotation')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();
  await vi.waitFor(() =>
    expect(rotateProviderCredential).toHaveBeenCalledTimes(2)
  );
  expect(vi.mocked(rotateProviderCredential).mock.calls[1]).toEqual(original);
  await vi.waitFor(() => {
    flushSync();
    expect(input.value).toBe('');
  });
});

it.each(['signout', 'unmount'] as const)(
  'does not certify after its prerequisite probe finishes following %s',
  async (retirement) => {
    let finish!: (value: never) => void;
    vi.mocked(probeProvider).mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        })
    );
    seed({ ...provider, last_probe_at: null, last_probe_status: null });
    client.setQueryData(providerKeys.models(provider.id), {
      provider,
      nextCursor: null,
      items: [
        {
          id: 'model-a',
          upstream_model: 'model',
          display_name: 'Model',
          enabled: true,
          capabilities: [
            {
              family: 'chat_completions',
              operation: 'generation',
              input: ['text'],
              output: ['text']
            }
          ]
        }
      ]
    });
    vi.mocked(listProviderModelPage).mockResolvedValue(
      client.getQueryData(providerKeys.models(provider.id)) as never
    );
    await render();
    click('Server-certify capabilities');
    await vi.waitFor(() => expect(probeProvider).toHaveBeenCalledTimes(1));
    if (retirement === 'signout') await authLifecycle.principalInvalidated();
    else {
      await unmount(component!);
      component = undefined;
    }
    finish({
      result: { succeeded: true, detail: 'ready' },
      etag: provider.etag
    } as never);
    await new Promise((resolve) => setTimeout(resolve, 0));
    flushSync();
    expect(certifyProviderModel).not.toHaveBeenCalled();
  }
);
