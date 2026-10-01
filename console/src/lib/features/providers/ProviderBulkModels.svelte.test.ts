import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  certifyProviderModel,
  getProviderCapabilityOptions,
  listProviderModelPage,
  setProviderModel,
  type ProviderCapabilityOptions,
  type ProviderModel
} from './api/models';
import { getProvider, type Provider } from './api/providers';
import ProviderBulkModelsProbe from './test/ProviderBulkModelsProbe.svelte';

vi.mock('./api/models', () => ({
  certifyProviderModel: vi.fn(),
  getProviderCapabilityOptions: vi.fn(),
  listProviderModelPage: vi.fn(),
  setProviderModel: vi.fn()
}));
vi.mock('./api/providers', () => ({
  getProvider: vi.fn(),
  probeProvider: vi.fn()
}));
vi.mock('./providerEditor', () => ({
  certificationPrerequisiteReady: () => true
}));
vi.mock('./api/profiles', () => ({ listProviderProfiles: vi.fn() }));

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

function providerOf(kind: string): Provider {
  return {
    id: 'provider-1',
    etag: 'etag-1',
    name: 'Provider',
    configuration: { kind, options: {} }
  } as unknown as Provider;
}

function modelWith(capabilities: unknown[]): ProviderModel {
  return {
    id: 'model-1',
    display_name: 'Model one',
    upstream_model: 'model-one',
    capabilities
  } as unknown as ProviderModel;
}

type Tuple = ProviderCapabilityOptions['capabilities'][number];

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(certifyProviderModel).mockResolvedValue({
    results: [{ succeeded: true }]
  } as never);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

/** Validates the only model and returns the capabilities it was given. */
async function validate(
  kind: string,
  model: ProviderModel,
  capabilities: Tuple[],
  operation?: string
) {
  const provider = providerOf(kind);
  vi.mocked(getProvider).mockResolvedValue(provider);
  vi.mocked(setProviderModel).mockResolvedValue(provider);
  vi.mocked(listProviderModelPage).mockResolvedValue({
    items: [model],
    nextCursor: null
  } as never);
  vi.mocked(getProviderCapabilityOptions).mockResolvedValue({
    capabilities
  } as ProviderCapabilityOptions);
  component = mount(ProviderBulkModelsProbe, {
    target: host,
    props: { client, provider }
  });
  flushSync();
  await vi.waitFor(() =>
    expect(host.querySelector('input[type="checkbox"]')).not.toBeNull()
  );
  if (operation) {
    const select = host.querySelector<HTMLSelectElement>('#bulk-operation')!;
    select.value = operation;
    select.dispatchEvent(new Event('change', { bubbles: true }));
  }
  host.querySelector<HTMLInputElement>('input[type="checkbox"]')!.click();
  flushSync();
  [...host.querySelectorAll<HTMLButtonElement>('button')]
    .find((button) => button.textContent?.startsWith('Validate'))!
    .click();
  await vi.waitFor(() => expect(certifyProviderModel).toHaveBeenCalled());
  return vi.mocked(setProviderModel).mock.calls[0][3];
}

it('keeps configured capabilities without response-only members', async () => {
  const sent = await validate(
    'openai',
    modelWith([
      {
        operation: 'generation',
        surface: 'openai',
        mode: 'unary',
        source: 'certified',
        certified_at: '2026-01-01T00:00:00Z'
      }
    ]),
    []
  );
  expect(sent).toEqual([
    { operation: 'generation', surface: 'openai', mode: 'unary' }
  ]);
  expect(sent[0]).not.toHaveProperty('source');
  expect(sent[0]).not.toHaveProperty('certified_at');
});

const geminiOptions = [
  { operation: 'generation', surface: 'gemini', mode: 'unary' },
  { operation: 'generation', surface: 'openai', mode: 'unary' },
  { operation: 'token_count', surface: 'gemini', mode: 'unary' },
  { operation: 'embeddings', surface: 'openai', mode: 'unary' }
] as Tuple[];

it('suggests embeddings on another surface when the native one lacks them', async () => {
  const sent = await validate(
    'gemini',
    modelWith([]),
    geminiOptions,
    'embeddings'
  );
  expect(sent).toEqual([
    { operation: 'embeddings', surface: 'openai', mode: 'unary' }
  ]);
});

it('still prefers the native surface when it offers the operation', async () => {
  const sent = await validate(
    'gemini',
    modelWith([]),
    geminiOptions,
    'generation'
  );
  expect(sent).toEqual([
    { operation: 'generation', surface: 'gemini', mode: 'unary' }
  ]);
});
