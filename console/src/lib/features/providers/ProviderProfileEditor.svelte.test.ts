import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ProviderKindCapability } from './models';
import { emptyProviderOptions, providerEditValues } from './providerEditor';
import {
  getConfigurationSchemas,
  listProviderProfiles,
  type ProviderProfile
} from './profiles';
import ProviderProfileEditorProbe from './test/ProviderProfileEditorProbe.svelte';

vi.mock('./profiles', async (original) => ({
  ...(await original<typeof import('./profiles')>()),
  listProviderProfiles: vi.fn(),
  getConfigurationSchemas: vi.fn()
}));

const compatibleSpec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'OpenAI-compatible',
  description: 'OpenAI-compatible HTTPS API',
  default_auth_mode: 'api_key',
  auth_modes: [
    { mode: 'api_key', label: 'Stored API key', credential: 'required' }
  ],
  fields: [{ field: 'endpoint', label: 'HTTPS endpoint', required: true }],
  presets: []
};

const compatibleChat: ProviderProfile = {
  id: 'compatible-chat',
  revision: '1',
  label: 'Compatible Chat Completions',
  kind: 'openai_compatible',
  dialect: 'openai-chat',
  dialect_revision: '1',
  hosting: 'direct-compatible',
  authentication: ['api_key'],
  transport: 'https',
  operations: ['generation'],
  operation_dialects: { generation: 'openai-chat' },
  default_schemas: {},
  semantic_headers: [],
  query_settings: [],
  documentation: 'https://example.test/compatible-chat'
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

/** An Automatic provider that carries parameter defaults. */
function render() {
  const values = providerEditValues(
    {
      name: 'Compatible',
      configuration: {
        kind: 'openai_compatible',
        auth_mode: 'api_key',
        endpoint: 'https://compatible.example/v1',
        options: { ...emptyProviderOptions(), parameter_defaults: { seed: 7 } }
      }
    },
    compatibleSpec
  );
  component = mount(ProviderProfileEditorProbe, {
    target: host,
    props: { client, values }
  });
  flushSync();
  return values;
}

function profileSelect() {
  return host.querySelector<HTMLSelectElement>('#probe-profile')!;
}

function button(name: string) {
  return [...host.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === name
  );
}

beforeEach(() => {
  vi.mocked(listProviderProfiles).mockResolvedValue([compatibleChat]);
  vi.mocked(getConfigurationSchemas).mockResolvedValue({});
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

describe('provider profile editor', () => {
  it('shows a provider without a profile as an Automatic provider', async () => {
    render();
    await vi.waitFor(() => expect(profileSelect().disabled).toBe(false));

    const select = profileSelect();
    expect(select.value).toBe('');
    expect(select.selectedOptions[0].textContent).toBe(
      'Automatic provider · no profile selected'
    );
    expect(host.querySelector('.profile-summary strong')?.textContent).toBe(
      'Automatic provider'
    );
    expect(
      [...host.querySelectorAll('summary')].map((item) => item.textContent)
    ).toContain('Parameter defaults');
    expect(host.textContent).not.toMatch(/legacy/i);
  });

  it('asks to remove Automatic provider parameter defaults before a profile is used', async () => {
    const values = render();
    await vi.waitFor(() => expect(profileSelect().options).toHaveLength(2));

    const select = profileSelect();
    select.value = 'compatible-chat@1';
    select.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();

    expect(values.profileId).toBe('compatible-chat');
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
      'Parameter defaults belong to Automatic providers'
    );
    expect(values.document?.issue).toContain(
      'Parameter defaults belong to Automatic providers'
    );
    expect(host.querySelector('.profile-summary strong')?.textContent).toBe(
      'Compatible Chat Completions'
    );

    button('Remove parameter defaults')!.click();
    flushSync();

    expect(values.document?.at(['options', 'parameter_defaults'])).toBe(
      undefined
    );
    expect(values.document?.issue).toBeNull();
    expect(button('Remove parameter defaults')).toBeUndefined();
    expect(host.textContent).not.toMatch(/legacy/i);
  });
});
