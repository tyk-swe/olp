import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { projectKeys } from '$lib/features/access/projects/projectKeys';
import { providerKeys } from './providerKeys';
import {
  activateProvider,
  createProvider,
  getProvider,
  probeProvider,
  updateProvider,
  type Provider,
  type ProviderProbe,
  type ProviderProbeReceipt
} from './api/providers';
import { rotateProviderCredential } from './api/credentials';
import {
  certifyProviderModel,
  getProviderCapabilityOptions,
  listProviderModelPage,
  type ProviderModel
} from './api/models';
import PluginProviderProbe from './test/PluginProviderProbe.svelte';
import { pluginSpec, referenceProfile } from './test/pluginFixtures';

vi.mock('./api/providers', async (original) => ({
  ...(await original<typeof import('./api/providers')>()),
  createProvider: vi.fn(),
  updateProvider: vi.fn(),
  probeProvider: vi.fn(),
  activateProvider: vi.fn(),
  getProvider: vi.fn()
}));
vi.mock('./api/credentials', async (original) => ({
  ...(await original<typeof import('./api/credentials')>()),
  rotateProviderCredential: vi.fn()
}));
vi.mock('./api/models', async (original) => ({
  ...(await original<typeof import('./api/models')>()),
  listProviderModelPage: vi.fn(),
  getProviderCapabilityOptions: vi.fn(),
  certifyProviderModel: vi.fn()
}));

const digest = 'e'.repeat(64);
const profile = referenceProfile(digest);
const succeeded: ProviderProbeReceipt = {
  result: {
    succeeded: true,
    detail: 'Connection reached.'
  } as ProviderProbe,
  etag: 'created'
};
const saved: Provider = {
  id: 'provider-account',
  name: 'Reference account',
  project_id: null,
  project_name: null,
  configuration: {
    kind: 'plugin',
    auth_mode: 'static_credential',
    profile_id: profile.id,
    profile_revision: digest,
    endpoint: 'https://api.example.com/v1',
    options: {
      vendor_id: null,
      limits: null,
      credential_headers: [],
      commitment: null,
      parameter_defaults: {},
      models: {}
    }
  },
  state: 'draft',
  connector_ready: true,
  pending_activation: true,
  etag: 'v1',
  created_at: '2026-09-27T06:00:00Z',
  updated_at: '2026-09-27T06:00:00Z',
  last_probe_at: '2026-09-27T06:01:00Z',
  last_probe_status: 'succeeded',
  model_count: 1,
  enabled_model_count: 1,
  capability_count: 1,
  certified_capability_count: 1,
  draft_credential_id: 'credential-1'
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
let provider: Provider;
let models: ProviderModel[];

beforeEach(() => {
  vi.resetAllMocks();
  window.history.replaceState({}, '', '/providers/new');
  authLifecycle.establishSession({
    user: {
      id: 'owner',
      email: 'owner@example.com',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'csrf-owner'
  });
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(providerKeys.kinds(), [
    {
      ...pluginSpec,
      fields: [{ field: 'model', label: 'Probe model', required: true }]
    }
  ]);
  client.setQueryData(providerKeys.profiles(), [profile]);
  client.setQueryData(providerKeys.vendors(), []);
  client.setQueryData(providerKeys.configurationSchemas(), {});
  client.setQueryData(projectKeys.memberships, []);
  provider = saved;
  models = [];
  vi.mocked(createProvider).mockResolvedValue(saved.id);
  vi.mocked(updateProvider).mockImplementation(async (_id, _etag, input) => {
    provider = {
      ...provider,
      name: input.name!,
      configuration: input.configuration!,
      etag: 'configured'
    };
    return provider;
  });
  vi.mocked(listProviderModelPage).mockImplementation(async () => ({
    provider,
    items: models,
    nextCursor: null
  }));
  vi.mocked(getProviderCapabilityOptions).mockResolvedValue({
    provider_kind: 'plugin',
    capabilities: []
  });
  vi.mocked(probeProvider).mockImplementation(async (current) => ({
    ...succeeded,
    etag: current.etag
  }));
  vi.mocked(rotateProviderCredential).mockResolvedValue();
  vi.mocked(activateProvider).mockImplementation(async () => {
    provider = { ...provider, state: 'active', etag: 'activated' };
    return 7;
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  await authLifecycle.principalInvalidated();
  client.clear();
  host.remove();
  window.history.replaceState({}, '', '/');
  vi.restoreAllMocks();
});

async function settle() {
  for (let i = 0; i < 3; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}
function set(selector: string, value: string, event = 'input') {
  const element = host.querySelector<HTMLInputElement | HTMLSelectElement>(
    selector
  )!;
  element.value = value;
  element.dispatchEvent(new Event(event, { bubbles: true }));
  flushSync();
}
function submit() {
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}
function button(text: string) {
  return [...host.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === text
  )!;
}
async function connect() {
  component = mount(PluginProviderProbe, { target: host, props: { client } });
  await settle();
  set('#provider-plugin-profile', `${profile.id}@${digest}`, 'change');
  set('#provider-name', saved.name);
  set('#initial-model', 'reference-model');
  set('#provider-secret', 'initial-secret');
}
async function created() {
  await connect();
  submit();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Declare upstream models');
  });
}
function back() {
  button('Back').click();
  flushSync();
}
async function activationStage() {
  await created();
  button('Continue to activation').click();
  flushSync();
  expect(button('Activate provider').disabled).toBe(false);
}
function cacheProvider() {
  client.setQueryData(providerKeys.models(provider.id, undefined), {
    provider,
    items: models,
    nextCursor: null
  });
  flushSync();
}

it('replays a committed rotation with its original parent ETag and key before any further PATCH', async () => {
  await created();
  back();
  set('#provider-secret', 'replacement-secret');
  vi.mocked(rotateProviderCredential).mockImplementationOnce(async () => {
    provider = { ...provider, etag: 'rotated' };
    throw new TypeError('committed response lost');
  });
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('committed response lost')
  );
  expect(updateProvider).toHaveBeenCalledOnce();
  const original = vi.mocked(rotateProviderCredential).mock.calls[0]!;
  expect(original[0]).toEqual({ id: saved.id, etag: 'configured' });
  // A background cache refresh cannot replace the unresolved parent precondition.
  cacheProvider();
  submit();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Declare upstream models');
  });
  expect(updateProvider).toHaveBeenCalledOnce();
  expect(vi.mocked(rotateProviderCredential).mock.calls[1]).toEqual(original);
  expect(createProvider).toHaveBeenCalledOnce();
});

it('refuses changed input while a rotation is uncertain and resumes only the original receipt', async () => {
  await created();
  back();
  set('#provider-secret', 'replacement-secret');
  vi.mocked(rotateProviderCredential).mockRejectedValueOnce(
    new TypeError('response lost')
  );
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('response lost'));
  const original = vi.mocked(rotateProviderCredential).mock.calls[0]!;
  set('#provider-secret', 'different-secret');
  submit();
  await settle();
  expect(host.textContent).toContain('Retry its original fields');
  expect(rotateProviderCredential).toHaveBeenCalledOnce();
  expect(updateProvider).toHaveBeenCalledOnce();
  set('#provider-secret', 'replacement-secret');
  submit();
  await vi.waitFor(() =>
    expect(rotateProviderCredential).toHaveBeenCalledTimes(2)
  );
  expect(vi.mocked(rotateProviderCredential).mock.calls[1]).toEqual(original);
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Declare upstream models');
  });
  back();
  set('#provider-secret', 'different-secret');
  submit();
  await vi.waitFor(() =>
    expect(rotateProviderCredential).toHaveBeenCalledTimes(3)
  );
  expect(vi.mocked(rotateProviderCredential).mock.calls[2]![2]).not.toBe(
    original[2]
  );
});

it('retries observation after a confirmed configuration update without repeating that update', async () => {
  await created();
  back();
  set('#provider-name', 'Changed connection');
  vi.mocked(listProviderModelPage).mockRejectedValue(
    new TypeError('refresh unavailable')
  );
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('refresh unavailable')
  );
  expect(host.textContent).toContain('Provider saved.');
  vi.mocked(listProviderModelPage).mockImplementation(async () => ({
    provider,
    items: [],
    nextCursor: null
  }));
  submit();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Declare upstream models');
  });
  expect(updateProvider).toHaveBeenCalledOnce();
  expect(rotateProviderCredential).not.toHaveBeenCalled();
});

it.each(['refresh', 'probe'])(
  'retries %s after a confirmed create without creating another provider',
  async (failure) => {
    await connect();
    if (failure === 'refresh')
      vi.mocked(listProviderModelPage).mockRejectedValue(
        new TypeError('observation unavailable')
      );
    else
      vi.mocked(probeProvider).mockRejectedValueOnce(
        new TypeError('observation unavailable')
      );
    submit();
    await vi.waitFor(() =>
      expect(host.textContent).toContain('observation unavailable')
    );
    expect(
      host.querySelector<HTMLInputElement>('#provider-secret')!.value
    ).toBe('');
    vi.mocked(listProviderModelPage).mockImplementation(async () => ({
      provider,
      items: [],
      nextCursor: null
    }));
    submit();
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Declare upstream models');
    });
    expect(createProvider).toHaveBeenCalledOnce();
  }
);

it('retries only observation after a confirmed activation', async () => {
  await activationStage();
  vi.mocked(listProviderModelPage).mockRejectedValue(
    new TypeError('activation refresh unavailable')
  );
  button('Activate provider').click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('activation refresh unavailable')
  );
  expect(host.textContent).toContain(
    'Provider activated in runtime generation 7.'
  );
  vi.mocked(listProviderModelPage).mockImplementation(async () => ({
    provider,
    items: [],
    nextCursor: null
  }));
  button('Activate provider').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Now build a stable route slug.');
  });
  expect(activateProvider).toHaveBeenCalledOnce();
});

it('pins an uncertain activation receipt across cache ETag refreshes', async () => {
  await activationStage();
  vi.mocked(activateProvider).mockRejectedValueOnce(
    new TypeError('activation response lost')
  );
  button('Activate provider').click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('activation response lost')
  );
  const original = vi.mocked(activateProvider).mock.calls[0]!;
  provider = { ...provider, etag: 'refreshed' };
  cacheProvider();
  button('Activate provider').click();
  await vi.waitFor(() => expect(activateProvider).toHaveBeenCalledTimes(2));
  expect(vi.mocked(activateProvider).mock.calls[1]).toEqual(original);
});

it('retires progress and delayed activation callbacks when adding another connection', async () => {
  await activationStage();
  const pending = Promise.withResolvers<number>();
  vi.mocked(activateProvider).mockReturnValueOnce(pending.promise);
  button('Activate provider').click();
  await vi.waitFor(() => expect(activateProvider).toHaveBeenCalledOnce());
  provider = { ...provider, state: 'active' };
  cacheProvider();
  button('Add another connection').click();
  flushSync();
  const observed = vi.mocked(listProviderModelPage).mock.calls.length;
  pending.resolve(8);
  await settle();
  expect(host.querySelector<HTMLInputElement>('#provider-name')!.value).toBe(
    ''
  );
  expect(host.querySelector<HTMLInputElement>('#provider-secret')!.value).toBe(
    ''
  );
  expect(host.textContent).not.toContain('runtime generation 8');
  expect(listProviderModelPage).toHaveBeenCalledTimes(observed);
});

it('does not apply a delayed copy after adding another connection', async () => {
  window.history.replaceState({}, '', '/providers/new?copy=source');
  const pending = Promise.withResolvers<Provider>();
  vi.mocked(getProvider).mockReturnValueOnce(pending.promise);
  await activationStage();
  button('Activate provider').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Now build a stable route slug.');
  });
  button('Add another connection').click();
  flushSync();
  pending.resolve({ ...saved, name: 'Stale source' });
  await settle();
  expect(host.querySelector<HTMLInputElement>('#provider-name')!.value).toBe(
    ''
  );
  expect(host.textContent).not.toContain('Stale source');
});

describe.each(['signout', 'unmount'] as const)(
  'retired by %s',
  (retirement) => {
    async function retire() {
      if (retirement === 'signout') await authLifecycle.principalInvalidated();
      else {
        await unmount(component!);
        component = undefined;
      }
      flushSync();
    }
    it('ignores a delayed create without issuing observation or probe requests', async () => {
      await connect();
      const pending = Promise.withResolvers<string>();
      vi.mocked(createProvider).mockReturnValueOnce(pending.promise);
      submit();
      await vi.waitFor(() => expect(createProvider).toHaveBeenCalledOnce());
      await retire();
      pending.resolve(saved.id);
      await settle();
      expect(listProviderModelPage).not.toHaveBeenCalled();
      expect(probeProvider).not.toHaveBeenCalled();
      expect(host.textContent).not.toContain('Provider saved.');
    });
    it('does not rotate after a delayed configuration response reaches a retired scope', async () => {
      await created();
      back();
      set('#provider-secret', 'replacement-secret');
      const pending = Promise.withResolvers<Provider>();
      vi.mocked(updateProvider).mockReturnValueOnce(pending.promise);
      submit();
      await vi.waitFor(() => expect(updateProvider).toHaveBeenCalledOnce());
      const observed = vi.mocked(listProviderModelPage).mock.calls.length;
      await retire();
      pending.resolve({ ...saved, etag: 'configured' });
      await settle();
      expect(rotateProviderCredential).not.toHaveBeenCalled();
      expect(listProviderModelPage).toHaveBeenCalledTimes(observed);
    });
    it('does not observe after a delayed rotation response reaches a retired scope', async () => {
      await created();
      back();
      set('#provider-secret', 'replacement-secret');
      const pending = Promise.withResolvers<void>();
      vi.mocked(rotateProviderCredential).mockReturnValueOnce(pending.promise);
      submit();
      await vi.waitFor(() =>
        expect(rotateProviderCredential).toHaveBeenCalledOnce()
      );
      const observed = vi.mocked(listProviderModelPage).mock.calls.length;
      await retire();
      pending.resolve();
      await settle();
      expect(listProviderModelPage).toHaveBeenCalledTimes(observed);
      expect(host.textContent).not.toContain('Provider saved.');
    });
    it('ignores a delayed probe without advancing the old wizard', async () => {
      await connect();
      const pending = Promise.withResolvers<ProviderProbeReceipt>();
      vi.mocked(probeProvider).mockReturnValueOnce(pending.promise);
      submit();
      await vi.waitFor(() => expect(probeProvider).toHaveBeenCalledOnce());
      await retire();
      pending.resolve(succeeded);
      await settle();
      expect(host.textContent).not.toContain('Declare upstream models');
    });
    it('ignores a delayed copy result', async () => {
      window.history.replaceState({}, '', '/providers/new?copy=source');
      const pending = Promise.withResolvers<Provider>();
      vi.mocked(getProvider).mockReturnValueOnce(pending.promise);
      await connect();
      await vi.waitFor(() => expect(getProvider).toHaveBeenCalledOnce());
      await retire();
      pending.resolve({ ...saved, name: 'Stale source' });
      await settle();
      expect(host.textContent).not.toContain('Stale source');
    });
    it('does not certify after its prerequisite probe completes in a retired scope', async () => {
      models = [
        {
          id: 'model-1',
          display_name: 'Reference model',
          upstream_model: 'reference-model',
          enabled: true,
          capabilities: [
            {
              operation: 'chat',
              surface: 'openai.chat_completions',
              mode: 'unary',
              source: 'declared'
            }
          ]
        } as ProviderModel
      ];
      await created();
      provider = { ...provider, last_probe_at: null, last_probe_status: null };
      cacheProvider();
      const pending = Promise.withResolvers<ProviderProbeReceipt>();
      vi.mocked(probeProvider).mockReturnValueOnce(pending.promise);
      button('Server-certify capabilities').click();
      await vi.waitFor(() => expect(probeProvider).toHaveBeenCalledTimes(2));
      const observed = vi.mocked(listProviderModelPage).mock.calls.length;
      await retire();
      pending.resolve(succeeded);
      await settle();
      expect(certifyProviderModel).not.toHaveBeenCalled();
      expect(listProviderModelPage).toHaveBeenCalledTimes(observed);
    });
  }
);

it('permits editing after a definite activation refusal', async () => {
  await activationStage();
  vi.mocked(activateProvider).mockRejectedValueOnce(
    new ApiProblem({
      status: 422,
      type: 'about:blank',
      title: 'Certification required.'
    })
  );
  button('Activate provider').click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Certification required.')
  );
  button('Back').click();
  flushSync();
  expect(host.textContent).toContain('Declare upstream models');
});

it('permits correcting a refused credential without repeating the saved configuration', async () => {
  await created();
  back();
  set('#provider-secret', 'invalid-secret');
  vi.mocked(rotateProviderCredential).mockRejectedValueOnce(
    new ApiProblem({
      status: 422,
      type: 'about:blank',
      title: 'Credential refused.'
    })
  );
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Credential refused.')
  );
  const original = vi.mocked(rotateProviderCredential).mock.calls[0]!;
  set('#provider-secret', 'corrected-secret');
  submit();
  await vi.waitFor(() =>
    expect(rotateProviderCredential).toHaveBeenCalledTimes(2)
  );
  expect(updateProvider).toHaveBeenCalledOnce();
  const corrected = vi.mocked(rotateProviderCredential).mock.calls[1]!;
  expect(corrected[0]).toEqual(original[0]);
  expect(corrected[1]).toBe('corrected-secret');
  expect(corrected[2]).not.toBe(original[2]);
});

it('retains rotation ambiguity after a later refusal and refuses replacing that receipt', async () => {
  await created();
  back();
  set('#provider-secret', 'original-secret');
  vi.mocked(rotateProviderCredential)
    .mockRejectedValueOnce(new TypeError('response lost'))
    .mockRejectedValueOnce(
      new ApiProblem({
        status: 403,
        type: 'about:blank',
        title: 'Replay disclosure refused.'
      })
    );
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('response lost'));
  const original = vi.mocked(rotateProviderCredential).mock.calls[0]!;
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Replay disclosure refused.')
  );
  set('#provider-secret', 'different-secret');
  submit();
  await settle();
  expect(host.textContent).toContain('Retry its original fields');
  expect(rotateProviderCredential).toHaveBeenCalledTimes(2);
  set('#provider-secret', 'original-secret');
  submit();
  await vi.waitFor(() =>
    expect(rotateProviderCredential).toHaveBeenCalledTimes(3)
  );
  expect(vi.mocked(rotateProviderCredential).mock.calls[2]).toEqual(original);
});

it('retains activation ambiguity after a later refusal', async () => {
  await activationStage();
  vi.mocked(activateProvider)
    .mockRejectedValueOnce(new TypeError('response lost'))
    .mockRejectedValueOnce(
      new ApiProblem({
        status: 403,
        type: 'about:blank',
        title: 'Replay disclosure refused.'
      })
    );
  button('Activate provider').click();
  await vi.waitFor(() => expect(host.textContent).toContain('response lost'));
  const original = vi.mocked(activateProvider).mock.calls[0]!;
  button('Activate provider').click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Replay disclosure refused.')
  );
  button('Back').click();
  flushSync();
  expect(host.textContent).toContain('Activation may already have committed');
  provider = { ...provider, etag: 'refreshed' };
  cacheProvider();
  button('Activate provider').click();
  await vi.waitFor(() => expect(activateProvider).toHaveBeenCalledTimes(3));
  expect(vi.mocked(activateProvider).mock.calls[2]).toEqual(original);
});

it('does not carry correction permission from an earlier refusal into a later ambiguous rotation', async () => {
  await created();
  back();
  set('#provider-secret', 'original-secret');
  vi.mocked(rotateProviderCredential)
    .mockRejectedValueOnce(
      new ApiProblem({
        status: 422,
        type: 'about:blank',
        title: 'Credential refused.'
      })
    )
    .mockRejectedValueOnce(new TypeError('response lost'));
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Credential refused.')
  );
  submit();
  await vi.waitFor(() => expect(host.textContent).toContain('response lost'));
  const uncertain = vi.mocked(rotateProviderCredential).mock.calls[1]!;
  set('#provider-secret', 'different-secret');
  submit();
  await settle();
  expect(rotateProviderCredential).toHaveBeenCalledTimes(2);
  expect(host.textContent).toContain('Retry its original fields');
  set('#provider-secret', 'original-secret');
  submit();
  await vi.waitFor(() =>
    expect(rotateProviderCredential).toHaveBeenCalledTimes(3)
  );
  expect(vi.mocked(rotateProviderCredential).mock.calls[2]).toEqual(uncertain);
});

it('replays an external credential rotation with the original binding and parent precondition', async () => {
  await created();
  back();
  const checkbox = [...host.querySelectorAll('label')]
    .find((label) =>
      label.textContent?.includes('Use an external credential version')
    )!
    .querySelector<HTMLInputElement>('input[type="checkbox"]')!;
  checkbox.checked = true;
  checkbox.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const reference = {
    store: 'gcp',
    secret_id: 'projects/project/secrets/provider',
    version: '1'
  };
  set(
    'textarea[aria-label="Pinned external credential reference"]',
    JSON.stringify(reference)
  );
  vi.mocked(rotateProviderCredential).mockImplementationOnce(async () => {
    provider = { ...provider, etag: 'external-rotated' };
    throw new TypeError('external rotation response lost');
  });
  submit();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('external rotation response lost')
  );
  const original = vi.mocked(rotateProviderCredential).mock.calls[0]!;
  expect(original[1]).toEqual(reference);
  cacheProvider();
  submit();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Declare upstream models');
  });
  expect(vi.mocked(rotateProviderCredential).mock.calls[1]).toEqual(original);
  expect(updateProvider).toHaveBeenCalledOnce();
});

it('preserves an operator-edited connection when a delayed copy returns', async () => {
  window.history.replaceState({}, '', '/providers/new?copy=source');
  const pending = Promise.withResolvers<Provider>();
  vi.mocked(getProvider).mockReturnValueOnce(pending.promise);
  await connect();
  pending.resolve({ ...saved, name: 'Stale source' });
  await settle();
  expect(host.querySelector<HTMLInputElement>('#provider-name')!.value).toBe(
    saved.name
  );
  expect(host.querySelector<HTMLInputElement>('#provider-secret')!.value).toBe(
    'initial-secret'
  );
  expect(host.textContent).not.toContain(
    'Creating a new draft from Stale source'
  );
});
