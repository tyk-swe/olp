import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { sessionKeys } from '$lib/features/access/session/sessionKeys';
import { projectKeys } from '$lib/features/access/projects/projectKeys';
import DraftEditorProbe from '$lib/forms/test/DraftEditorProbe.svelte';
import { provider, providerSpec } from '$lib/forms/test/draftFixtures';
import { providerKeys } from './providerKeys';
import { listProviderModelPage } from './api/models';
import {
  listCredentialSlots,
  putCredentialSlot,
  validateCredentialSlot,
  type CredentialSlotPool
} from './api/credentials';
import {
  acceptCatalogSuggestions,
  listCatalogSuggestions,
  type CatalogSuggestionList
} from './api/catalog';

vi.mock('$app/navigation', () => ({ beforeNavigate: vi.fn(), goto: vi.fn() }));
vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/providers/provider-a') }
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./api/models', async (original) => ({
  ...(await original<typeof import('./api/models')>()),
  listProviderModelPage: vi.fn()
}));
vi.mock('./api/credentials', async (original) => ({
  ...(await original<typeof import('./api/credentials')>()),
  listCredentialSlots: vi.fn(),
  putCredentialSlot: vi.fn(),
  validateCredentialSlot: vi.fn()
}));
vi.mock('./api/catalog', async (original) => ({
  ...(await original<typeof import('./api/catalog')>()),
  listCatalogSuggestions: vi.fn(),
  acceptCatalogSuggestions: vi.fn()
}));

const pool: CredentialSlotPool = {
  etag: 'slots-v1',
  health: {},
  items: [
    {
      id: 'slot-a',
      name: 'Default slot',
      enabled: true,
      priority: 0,
      weight: 1,
      credential_version_id: 'credential-a',
      allowed_models: [],
      allowed_routes: [],
      allowed_api_keys: [],
      requests_per_minute: null,
      tokens_per_minute: null,
      max_concurrency: null
    }
  ]
};
const suggestions: CatalogSuggestionList = {
  catalog: {
    api_version: 'v1',
    key_id: 'catalog-key',
    model_count: 1,
    vendor_count: 1,
    sha256: 'a'.repeat(64),
    published_at: provider.created_at
  },
  vendor_id: 'openai',
  items: [
    {
      model_id: 'model-a',
      upstream_model: 'gpt-example',
      catalog_model: 'gpt-example',
      matched_by: 'id',
      facts: {
        canonical_model: 'gpt-example',
        context_length: 128000,
        max_output_tokens: null,
        input_modalities: [],
        output_modalities: [],
        supported_parameters: null,
        quantization: null,
        region: null,
        data_collection: null,
        zero_data_retention: null,
        deployment: null,
        source: 'catalog',
        observed_at: provider.created_at
      },
      changes: ['context_length'],
      capabilities: {},
      lifecycle: null,
      conflict: null
    }
  ]
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
const page = (value = provider) => ({
  provider: value,
  items: [],
  nextCursor: null
});

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
  client.setQueryData(providerKeys.credentials(provider.id), []);
  client.setQueryData(providerKeys.networkCredentials(provider.id), []);
  client.setQueryData(providerKeys.revisions(provider.id), {
    items: [],
    nextCursor: null
  });
  client.setQueryData(providerKeys.capabilityOptions('openai'), {
    capabilities: []
  });
  client.setQueryData(providerKeys.bulkModels(provider.id, provider.etag), []);
  client.setQueryData(providerKeys.slots(provider.id, provider.etag), pool);
  client.setQueryData(providerKeys.models(provider.id), page());
  client.setQueryData(
    providerKeys.catalogSuggestions(provider.id, provider.etag),
    suggestions
  );
  vi.mocked(listProviderModelPage).mockResolvedValue(page());
  vi.mocked(listCredentialSlots).mockResolvedValue(pool);
  vi.mocked(putCredentialSlot).mockResolvedValue();
  vi.mocked(validateCredentialSlot).mockResolvedValue();
  vi.mocked(listCatalogSuggestions).mockResolvedValue(suggestions);
  vi.mocked(acceptCatalogSuggestions).mockResolvedValue(provider);
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
    expect(host.textContent).toContain('Default slot');
  });
}
function button(label: string) {
  const node = [...host.querySelectorAll('button')].find(
    (value) => value.textContent?.trim() === label
  );
  if (!node) throw new Error('Missing button: ' + label);
  return node;
}
function click(label: string) {
  button(label).click();
  flushSync();
}
async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}
async function retire(kind: 'signout' | 'unmount' | 'reload') {
  if (kind === 'signout') await authLifecycle.principalInvalidated();
  else if (kind === 'unmount') {
    await unmount(component!);
    component = undefined;
  } else {
    const input = host.querySelector<HTMLInputElement>('#detail-name')!;
    input.value = 'Locally edited';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    const remote = { ...provider, etag: 'remote' };
    client.setQueryData(providerKeys.slots(remote.id, remote.etag), pool);
    client.setQueryData(providerKeys.bulkModels(remote.id, remote.etag), []);
    client.setQueryData(
      providerKeys.catalogSuggestions(remote.id, remote.etag),
      suggestions
    );
    client.setQueryData(providerKeys.models(remote.id), page(remote));
    flushSync();
    click('Reload');
    await vi.waitFor(() => {
      flushSync();
      expect(host.querySelector<HTMLInputElement>('#detail-name')!.value).toBe(
        provider.name
      );
    });
  }
}

it.each(['signout', 'unmount', 'reload'] as const)(
  'fences a delayed pool validation after %s',
  async (retirement) => {
    const pending = Promise.withResolvers<void>();
    vi.mocked(validateCredentialSlot).mockReturnValueOnce(pending.promise);
    await render();
    click('Validate access');
    expect(validateCredentialSlot).toHaveBeenCalledWith(
      provider.id,
      'slot-a',
      'slots-v1'
    );
    await retire(retirement);
    const reads = vi.mocked(listCredentialSlots).mock.calls.length;
    pending.resolve();
    await settle();
    expect(listCredentialSlots).toHaveBeenCalledTimes(reads);
    expect(host.textContent).not.toContain(
      'Default slot: model access validated.'
    );
  }
);

it('does not publish a retired validation error', async () => {
  const pending = Promise.withResolvers<void>();
  vi.mocked(validateCredentialSlot).mockReturnValueOnce(pending.promise);
  await render();
  click('Validate access');
  await retire('signout');
  pending.reject(new Error('retired validation error'));
  await settle();
  expect(host.textContent).not.toContain('retired validation error');
});

it.each(['signout', 'unmount'] as const)(
  'does not continue a confirmed pool save after %s during parent refresh',
  async (retirement) => {
    const pending =
      Promise.withResolvers<
        Awaited<ReturnType<typeof listProviderModelPage>>
      >();
    vi.mocked(listProviderModelPage).mockReturnValueOnce(pending.promise);
    await render();
    click('Edit / rotate');
    button('Save credential')
      .closest('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    flushSync();
    await vi.waitFor(() =>
      expect(listProviderModelPage).toHaveBeenCalledOnce()
    );
    expect(putCredentialSlot).toHaveBeenCalledOnce();
    await retire(retirement);
    const reads = vi.mocked(listCredentialSlots).mock.calls.length;
    pending.resolve(page());
    await settle();
    expect(listCredentialSlots).toHaveBeenCalledTimes(reads);
  }
);

it('replays catalog acceptance with its original parent ETag after cache hydration', async () => {
  vi.mocked(acceptCatalogSuggestions).mockRejectedValueOnce(
    new TypeError('catalog response lost')
  );
  await render();
  const checkbox = host.querySelector<HTMLInputElement>(
    '.catalog-suggestions input[type="checkbox"]'
  )!;
  checkbox.checked = true;
  checkbox.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  click('Accept facts for 1 model');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('catalog response lost')
  );
  const original = vi.mocked(acceptCatalogSuggestions).mock.calls[0]!;
  const current = { ...provider, etag: 'observed-parent' };
  client.setQueryData(providerKeys.slots(current.id, current.etag), pool);
  client.setQueryData(providerKeys.bulkModels(current.id, current.etag), []);
  client.setQueryData(
    providerKeys.catalogSuggestions(current.id, current.etag),
    suggestions
  );
  client.setQueryData(providerKeys.models(current.id), page(current));
  flushSync();
  click('Accept facts for 1 model');
  await vi.waitFor(() =>
    expect(acceptCatalogSuggestions).toHaveBeenCalledTimes(2)
  );
  expect(vi.mocked(acceptCatalogSuggestions).mock.calls[1]).toEqual(original);
  expect(original[0]).toEqual({ id: provider.id, etag: provider.etag });
});
