import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { listProjectMemberships } from '$lib/features/access/projects/api';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import type { ProviderKindCapability } from './api/models';
import { listProviderVendors, type ProviderVendor } from './api/providers';
import { listProviderProfiles } from './api/profiles';
import type { ProviderDraft } from './providerEditor';
import ProviderConnectorFormProbe from './test/ProviderConnectorFormProbe.svelte';

vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', () => ({
  listProjectMemberships: vi.fn()
}));
vi.mock('./api/providers', () => ({ listProviderVendors: vi.fn() }));
vi.mock('./api/profiles', async (original) => ({
  ...(await original<typeof import('./api/profiles')>()),
  listProviderProfiles: vi.fn(),
  getConfigurationSchemas: vi.fn()
}));

const spec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'OpenAI-compatible',
  description: 'OpenAI-compatible HTTPS API',
  default_auth_mode: 'api_key',
  auth_modes: [
    { mode: 'api_key', label: 'Stored API key', credential: 'required' }
  ],
  fields: [{ field: 'endpoint', label: 'HTTPS endpoint', required: true }],
  presets: [
    {
      id: 'groq',
      label: 'Groq',
      description: 'Groq',
      auth_mode: 'api_key',
      endpoint: 'https://api.groq.com/openai/v1',
      documentation_label: 'Groq',
      documentation_url: 'https://example.test/groq',
      maintainer: 'Groq',
      discovery: true,
      placeholder: false,
      profile_id: null,
      profile_revision: null
    }
  ]
};

function vendor(id: string, name: string): ProviderVendor {
  return {
    id,
    name,
    connector: 'openai_compatible',
    authentication: ['api_key'],
    discovery: true,
    documentation_url: 'https://example.test',
    operations: ['generation'],
    parameters: [],
    dialects: ['openai-chat', 'openai-responses'],
    unsupported_parameters: []
  };
}

let host: HTMLElement;
let client: QueryClient;
let component: { current(): ProviderDraft } | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(useRole).mockReturnValue({
    can: () => true,
    allows: () => true,
    role: 'owner',
    globalScope: true,
    user: null
  });
  vi.mocked(listProjectMemberships).mockResolvedValue([]);
  vi.mocked(listProviderProfiles).mockResolvedValue([]);
  vi.mocked(listProviderVendors).mockResolvedValue([
    vendor('openai_compatible', 'OpenAI-compatible'),
    vendor('groq', 'Groq')
  ]);
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
});

function choose(select: HTMLSelectElement, value: string) {
  select.value = value;
  select.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
}

it('chooses the generic OpenAI-compatible vendor without a preset', async () => {
  component = mount(ProviderConnectorFormProbe, {
    target: host,
    props: { client, spec }
  }) as unknown as { current(): ProviderDraft };
  flushSync();
  const select = host.querySelector<HTMLSelectElement>('#provider-vendor')!;
  await vi.waitFor(() => expect(select.options.length).toBe(3));

  choose(select, 'groq');
  expect(component.current().presetId).toBe('groq');
  expect(component.current().endpoint).toBe('https://api.groq.com/openai/v1');

  choose(select, 'openai_compatible');
  const draft = component.current();
  expect(draft.endpoint).toBe('');
  expect(draft.credential).toBe('');
  expect(draft.options?.vendor_id).toBe('openai_compatible');
  expect(draft.name).toBe('OpenAI-compatible');
  expect(select.value).toBe('openai_compatible');
});

it('requires a SageMaker endpoint in a fresh draft with no vendor selection', () => {
  component = mount(ProviderConnectorFormProbe, {
    target: host,
    props: {
      client,
      spec: {
        ...spec,
        kind: 'sagemaker',
        label: 'Amazon SageMaker AI',
        default_auth_mode: 'default_chain',
        auth_modes: [
          {
            mode: 'default_chain',
            label: 'AWS credential chain',
            credential: 'forbidden'
          }
        ],
        fields: [
          { field: 'cloud_region', label: 'Region', required: true },
          { field: 'model', label: 'Probe model', required: true }
        ],
        presets: []
      }
    }
  }) as unknown as { current(): ProviderDraft };
  flushSync();
  const input = host.querySelector<HTMLInputElement>('#initial-model')!;
  expect(input.required).toBe(true);
  expect(host.querySelector('label[for="initial-model"]')?.textContent).toBe(
    'SageMaker endpoint'
  );
  expect(component.current().presetId).toBe('');
  expect(component.current().options?.vendor_id).toBeFalsy();
});
