import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { projectKeys } from '$lib/features/access/projects/projectKeys';
import { providerKeys } from './providerKeys';
import {
  createProvider,
  probeProvider,
  type Provider,
  type ProviderProbe
} from './api';
import { listProviderModelPage, type ProviderKindCapability } from './models';
import type { ProviderProfile } from './profiles';
import PluginProviderProbe from './test/PluginProviderProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  createProvider: vi.fn(),
  probeProvider: vi.fn()
}));
vi.mock('./models', async (original) => ({
  ...(await original<typeof import('./models')>()),
  listProviderModelPage: vi.fn()
}));

const digest = 'd'.repeat(64);

const openAiSpec: ProviderKindCapability = {
  kind: 'openai',
  label: 'OpenAI',
  description: 'Official OpenAI HTTPS API',
  default_auth_mode: 'api_key',
  auth_modes: [
    { mode: 'api_key', label: 'Stored API key', credential: 'required' }
  ],
  fields: [{ field: 'endpoint', label: 'Endpoint', required: false }],
  presets: []
};
const pluginSpec: ProviderKindCapability = {
  kind: 'plugin',
  label: 'Provider plugin',
  description: 'A profile an installed provider plugin supplies.',
  default_auth_mode: 'static_credential',
  auth_modes: [
    {
      mode: 'static_credential',
      label: 'Static credential',
      credential: 'required'
    }
  ],
  fields: [],
  presets: []
};

const referenceChat: ProviderProfile = {
  id: 'reference-chat',
  revision: digest,
  label: 'Reference Chat Completions',
  kind: 'plugin',
  dialect: 'openai-chat',
  dialect_revision: 'unversioned-2026-09-22',
  hosting: 'plugin',
  authentication: ['static_credential'],
  transport: 'http',
  operations: ['generation'],
  operation_dialects: { generation: 'openai-chat' },
  default_schemas: {},
  semantic_headers: ['Openai-Beta'],
  query_settings: [],
  documentation: '',
  strict: true,
  plugin: { digest, name: 'reference', version: '0.1.0' }
};
const openAiChat: ProviderProfile = {
  ...referenceChat,
  id: 'openai-chat',
  revision: '1',
  label: 'OpenAI Chat Completions',
  kind: 'openai',
  hosting: 'direct-openai',
  authentication: ['api_key'],
  plugin: undefined
};

const pluginProvider: Provider = {
  id: 'provider-plugin',
  name: 'Reference upstream',
  project_id: null,
  project_name: null,
  configuration: {
    kind: 'plugin',
    auth_mode: 'static_credential',
    profile_id: referenceChat.id,
    profile_revision: digest,
    endpoint: 'https://api.example.com/v1',
    options: {
      vendor_id: null,
      limits: null,
      credential_headers: [],
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
  model_count: 0,
  enabled_model_count: 0,
  capability_count: 0,
  certified_capability_count: 0
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  // Anything the test does not seed answers as absent, never the network.
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(JSON.stringify({ title: 'Not Found', status: 404 }), {
          status: 404,
          headers: { 'Content-Type': 'application/problem+json' }
        })
    )
  );
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(providerKeys.kinds(), [openAiSpec, pluginSpec]);
  client.setQueryData(['provider-vendors'], []);
  client.setQueryData(['provider-configuration-schemas'], {});
  client.setQueryData(projectKeys.memberships, []);
  client.setQueryData(['provider-profiles'], [openAiChat, referenceChat]);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
});

function render(providerId?: string) {
  component = mount(PluginProviderProbe, {
    target: host,
    props: { client, providerId }
  });
  flushSync();
}

async function settle() {
  for (let i = 0; i < 3; i++)
    await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function choose(element: HTMLInputElement | HTMLSelectElement, value: string) {
  if (element instanceof HTMLInputElement && element.type === 'radio')
    element.checked = true;
  else element.value = value;
  element.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
}

function type(selector: string, value: string) {
  const input = host.querySelector<HTMLInputElement>(selector)!;
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function choosePluginKind() {
  choose(
    host.querySelector<HTMLInputElement>('input[name="kind"][value="plugin"]')!,
    'plugin'
  );
}

describe('provider wizard with a plugin profile', () => {
  it('offers approved plugin profiles with their build, then the static credential', async () => {
    const saved: Provider = { ...pluginProvider, id: 'provider-new' };
    vi.mocked(createProvider).mockResolvedValue(saved.id);
    vi.mocked(listProviderModelPage).mockResolvedValue({
      provider: saved,
      items: [],
      nextCursor: null
    });
    vi.mocked(probeProvider).mockResolvedValue({
      succeeded: true,
      detail: 'Reached the upstream; 1 models listed.',
      probe_type: 'models',
      discovered_models: 1
    } as ProviderProbe);
    render();
    await settle();

    choosePluginKind();
    expect(host.querySelector('#provider-endpoint')).toBeNull();
    expect(host.querySelector('#provider-profile')).toBeNull();
    const select = host.querySelector<HTMLSelectElement>(
      '#provider-plugin-profile'
    )!;
    const offered = [...select.querySelectorAll('optgroup option')];
    expect(offered.map((option) => option.textContent?.trim())).toEqual([
      'Reference Chat Completions · reference 0.1.0 · digest dddddddddddd'
    ]);
    expect(select.querySelector('optgroup')?.label).toBe(
      'reference 0.1.0 · digest dddddddddddd'
    );
    expect(host.querySelector('#provider-secret')).not.toBeNull();
    expect(host.querySelector('label[for="initial-model"]')?.textContent).toBe(
      'Probe model'
    );

    choose(select, `reference-chat@${digest}`);
    const pinned = host.querySelector('[aria-label="Pinned plugin"]')!;
    expect(pinned.textContent).toContain('reference 0.1.0');
    expect(pinned.textContent).toContain(digest);
    expect(pinned.textContent).toContain('Reference Chat Completions');

    type('#provider-name', 'Reference upstream');
    type('#initial-model', 'reference-model');
    type('#provider-secret', 'reference-secret');
    host
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(createProvider).toHaveBeenCalledOnce());
    const [input] = vi.mocked(createProvider).mock.calls[0]!;
    expect(input.configuration).toMatchObject({
      kind: 'plugin',
      auth_mode: 'static_credential',
      profile_id: 'reference-chat',
      profile_revision: digest
    });
    expect(input.configuration.endpoint ?? null).toBeNull();
    expect(input.credential).toBe('reference-secret');
    expect(input.model).toBe('reference-model');
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Declare upstream models');
    });
  });

  it('discovers the models of a profile that declares discovery, without a probe model', async () => {
    client.setQueryData(
      ['provider-profiles'],
      [openAiChat, { ...referenceChat, model_discovery: true }]
    );
    const saved: Provider = { ...pluginProvider, id: 'provider-new' };
    vi.mocked(createProvider).mockResolvedValue(saved.id);
    vi.mocked(listProviderModelPage).mockResolvedValue({
      provider: saved,
      items: [],
      nextCursor: null
    });
    vi.mocked(probeProvider).mockResolvedValue({
      succeeded: true,
      detail: 'Reached the upstream; 3 models listed.',
      probe_type: 'models',
      discovered_models: 3
    } as ProviderProbe);
    render();
    await settle();

    choosePluginKind();
    choose(
      host.querySelector<HTMLSelectElement>('#provider-plugin-profile')!,
      `reference-chat@${digest}`
    );
    expect(host.querySelector('label[for="initial-model"]')?.textContent).toBe(
      'Seed model (optional)'
    );
    type('#provider-name', 'Reference upstream');
    type('#provider-secret', 'reference-secret');
    host
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(createProvider).toHaveBeenCalledOnce());
    expect(vi.mocked(createProvider).mock.calls[0]![0].model).toBeUndefined();
    await vi.waitFor(() => {
      flushSync();
      expect(
        host.querySelector('#discovery-heading')?.textContent?.trim()
      ).toBe('Discover upstream models');
    });
    expect(host.querySelector('#manual-models-wizard')).toBeNull();
  });

  it('points to the Plugins page when no approved plugin offers a profile', async () => {
    client.setQueryData(['provider-profiles'], [openAiChat]);
    render();
    await settle();
    choosePluginKind();
    expect(
      host.querySelector<HTMLSelectElement>('#provider-plugin-profile')
        ?.disabled
    ).toBe(true);
    expect(host.textContent).toContain('No approved plugin offers a profile');
    expect(
      host.querySelector<HTMLAnchorElement>('a[href$="/plugins"]')
    ).not.toBeNull();
    host
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await settle();
    expect(createProvider).not.toHaveBeenCalled();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'plugin profile'
    );
  });
});

describe('provider detail', () => {
  it('shows the plugin a plugin provider pins', async () => {
    client.setQueryData(providerKeys.models(pluginProvider.id), {
      provider: pluginProvider,
      items: [],
      nextCursor: null
    });
    vi.mocked(listProviderModelPage).mockResolvedValue({
      provider: pluginProvider,
      items: [],
      nextCursor: null
    });
    render(pluginProvider.id);
    await settle();

    const pinned = host.querySelector('[aria-label="Pinned plugin"]')!;
    expect(pinned.textContent).toContain('reference 0.1.0');
    expect(pinned.textContent).toContain('Reference Chat Completions');
    expect(pinned.textContent).toContain(digest);
    expect(pinned.textContent).toContain('https://api.example.com/v1');
    expect(
      host.querySelector<HTMLSelectElement>('#detail-plugin-profile')?.value
    ).toBe(`reference-chat@${digest}`);
    expect(host.querySelector('#detail-endpoint')).toBeNull();
    expect(host.querySelector('#detail-profile')).toBeNull();
    expect(host.textContent).toContain('Recheck declared models');
  });

  it('runs upstream discovery for a plugin profile that declares it', async () => {
    client.setQueryData(
      ['provider-profiles'],
      [openAiChat, { ...referenceChat, model_discovery: true }]
    );
    client.setQueryData(providerKeys.models(pluginProvider.id), {
      provider: pluginProvider,
      items: [],
      nextCursor: null
    });
    vi.mocked(listProviderModelPage).mockResolvedValue({
      provider: pluginProvider,
      items: [],
      nextCursor: null
    });
    render(pluginProvider.id);
    await settle();

    expect(host.textContent).toContain('Run upstream discovery');
    expect(host.textContent).not.toContain('Recheck declared models');
    expect(host.querySelector('#manual-models-detail')).toBeNull();
  });
});
