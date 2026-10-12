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
import { getProvider, probeProvider, type Provider } from './api/providers';
import { certificationPrerequisiteReady } from './providerEditor';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { parseNativeJSON } from '$lib/json/nativeJson';
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
  certificationPrerequisiteReady: vi.fn()
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
  vi.mocked(certificationPrerequisiteReady).mockReturnValue(true);
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
  vi.mocked(certifyProviderModel).mockResolvedValue({
    result: { results: [{ succeeded: true }] },
    etag: 'etag-1'
  } as never);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  await authLifecycle.principalInvalidated();
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
  if (!vi.mocked(setProviderModel).getMockImplementation())
    vi.mocked(setProviderModel).mockResolvedValue(provider);
  vi.mocked(listProviderModelPage).mockResolvedValue({
    provider,
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

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

async function renderValidation(
  onChanged = vi.fn(async () => {}),
  inventory = [modelWith([])],
  provider = providerOf('openai'),
  inventoryProvider = provider
) {
  vi.mocked(listProviderModelPage).mockResolvedValue({
    provider: inventoryProvider,
    items: inventory,
    nextCursor: null
  } as never);
  vi.mocked(getProviderCapabilityOptions).mockResolvedValue({
    capabilities: [
      { operation: 'generation', surface: 'openai', mode: 'unary' }
    ]
  } as ProviderCapabilityOptions);
  if (!vi.mocked(setProviderModel).getMockImplementation())
    vi.mocked(setProviderModel).mockResolvedValue(provider);
  const mounted = mount(ProviderBulkModelsProbe, {
    target: host,
    props: { client, provider, onChanged }
  });
  component = mounted;
  flushSync();
  await vi.waitFor(() =>
    expect(host.querySelector('input[type="checkbox"]')).not.toBeNull()
  );
  for (const input of host.querySelectorAll<HTMLInputElement>(
    'input[type="checkbox"]'
  ))
    input.click();
  flushSync();
  const validate = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent?.startsWith('Validate')
  )!;
  validate.click();
  flushSync();
  return { mounted, provider, validate };
}

it.each([
  'unmount',
  'principal',
  'provider',
  'endpoint',
  'credential'
] as const)(
  'fences mutation after a delayed provider read on %s retirement',
  async (boundary) => {
    const read = deferred<Provider>();
    vi.mocked(getProvider).mockReturnValue(read.promise);
    vi.mocked(certificationPrerequisiteReady).mockReturnValue(false);
    const { mounted, provider } = await renderValidation();
    await vi.waitFor(() => expect(getProvider).toHaveBeenCalledTimes(1));
    if (boundary === 'unmount') {
      await unmount(mounted);
      component = undefined;
    } else if (boundary === 'principal')
      await authLifecycle.principalInvalidated();
    else {
      mounted.replaceProvider(
        boundary === 'provider'
          ? { ...provider, id: 'provider-other' }
          : boundary === 'endpoint'
            ? {
                ...provider,
                configuration: {
                  ...provider.configuration,
                  endpoint: 'https://new.example/v1'
                }
              }
            : { ...provider, draft_credential_id: 'new-credential' }
      );
      flushSync();
    }
    read.resolve(provider);
    await read.promise;
    await Promise.resolve();
    flushSync();
    expect(probeProvider).not.toHaveBeenCalled();
    expect(setProviderModel).not.toHaveBeenCalled();
    expect(certifyProviderModel).not.toHaveBeenCalled();
  }
);

it.each(['endpoint', 'credential'] as const)(
  'retires cached review when a fresh read observes changed %s',
  async (boundary) => {
    const original = providerOf('openai');
    const current =
      boundary === 'endpoint'
        ? {
            ...original,
            configuration: {
              ...original.configuration,
              endpoint: 'https://new.example/v1'
            }
          }
        : { ...original, draft_credential_id: 'new-credential' };
    vi.mocked(getProvider).mockResolvedValue(current);
    vi.mocked(certificationPrerequisiteReady).mockReturnValue(false);
    await renderValidation(undefined, [
      modelWith([]),
      { ...modelWith([]), id: 'model-2' }
    ]);
    await vi.waitFor(() =>
      expect(host.textContent).toContain('The connection changed')
    );
    expect(getProvider).toHaveBeenCalledTimes(1);
    expect(probeProvider).not.toHaveBeenCalled();
    expect(setProviderModel).not.toHaveBeenCalled();
    expect(certifyProviderModel).not.toHaveBeenCalled();
  }
);

it('checks the reviewed draft again after an awaited probe', async () => {
  const original = providerOf('openai');
  vi.mocked(getProvider)
    .mockResolvedValueOnce(original)
    .mockResolvedValue({
      ...original,
      draft_credential_id: 'rotated-credential'
    });
  vi.mocked(certificationPrerequisiteReady).mockReturnValue(false);
  vi.mocked(probeProvider).mockResolvedValue({
    result: { succeeded: true },
    etag: original.etag
  } as Awaited<ReturnType<typeof probeProvider>>);
  await renderValidation();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('The connection changed')
  );
  expect(getProvider).toHaveBeenCalledTimes(2);
  expect(setProviderModel).not.toHaveBeenCalled();
  expect(certifyProviderModel).not.toHaveBeenCalled();
});

it('detects changed native schema property order before following a fresh provider read', async () => {
  const configuration = (properties: string) =>
    parseNativeJSON(
      `{"kind":"openai","options":{"parameter_defaults":{"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object","properties":{${properties}}}}}}}}`
    ) as Provider['configuration'];
  const original = {
    ...providerOf('openai'),
    configuration: configuration('"1":{"type":"string"},"0":{"type":"string"}')
  };
  const current = {
    ...original,
    configuration: configuration('"0":{"type":"string"},"1":{"type":"string"}')
  };
  expect(JSON.stringify(current.configuration)).toBe(
    JSON.stringify(original.configuration)
  );
  vi.mocked(getProvider).mockResolvedValue(current);
  vi.mocked(certificationPrerequisiteReady).mockReturnValue(false);
  await renderValidation(undefined, undefined, original);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('The connection changed')
  );
  expect(probeProvider).not.toHaveBeenCalled();
  expect(setProviderModel).not.toHaveBeenCalled();
  expect(certifyProviderModel).not.toHaveBeenCalled();
});

it('stops after a delayed probe without reading or mutating another phase', async () => {
  const probe = deferred<Awaited<ReturnType<typeof probeProvider>>>();
  vi.mocked(getProvider).mockResolvedValue(providerOf('openai'));
  vi.mocked(certificationPrerequisiteReady).mockReturnValue(false);
  vi.mocked(probeProvider).mockReturnValue(probe.promise);
  await renderValidation();
  await vi.waitFor(() => expect(probeProvider).toHaveBeenCalledTimes(1));
  [...host.querySelectorAll<HTMLButtonElement>('button')]
    .find((button) => button.textContent?.includes('Stop validation'))!
    .click();
  probe.resolve({ result: { succeeded: true }, etag: 'etag-1' } as Awaited<
    ReturnType<typeof probeProvider>
  >);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Stopped after 0 models')
  );
  expect(getProvider).toHaveBeenCalledTimes(1);
  expect(setProviderModel).not.toHaveBeenCalled();
  expect(certifyProviderModel).not.toHaveBeenCalled();
});

it('retries observation after confirmed validation without declaring or certifying again', async () => {
  vi.mocked(getProvider).mockResolvedValue(providerOf('openai'));
  const onChanged = vi
    .fn(async () => {})
    .mockRejectedValueOnce(new Error('refresh unavailable'));
  const { validate } = await renderValidation(onChanged);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('refresh unavailable')
  );
  expect(validate.disabled).toBe(true);
  expect(setProviderModel).toHaveBeenCalledTimes(1);
  expect(certifyProviderModel).toHaveBeenCalledTimes(1);
  [...host.querySelectorAll<HTMLButtonElement>('button')]
    .find((button) => button.textContent?.trim() === 'Retry refresh')!
    .click();
  await vi.waitFor(() => expect(onChanged).toHaveBeenCalledTimes(2));
  await vi.waitFor(() => expect(validate.disabled).toBe(false));
  expect(setProviderModel).toHaveBeenCalledTimes(1);
  expect(certifyProviderModel).toHaveBeenCalledTimes(1);
});

it('does not overwrite concurrently edited model declarations with a freshly adopted parent version', async () => {
  const original = providerOf('openai');
  vi.mocked(getProvider).mockResolvedValue({
    ...original,
    etag: 'edited-model-version'
  });
  await renderValidation(undefined, [
    modelWith([{ operation: 'generation', surface: 'openai', mode: 'unary' }])
  ]);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('The connection changed')
  );
  expect(setProviderModel).not.toHaveBeenCalled();
  expect(certifyProviderModel).not.toHaveBeenCalled();
});

it('binds cached model declarations to their inventory version instead of newer parent props', async () => {
  const original = providerOf('openai');
  const parent = { ...original, etag: 'new-parent-version' };
  vi.mocked(getProvider).mockResolvedValue(parent);
  await renderValidation(undefined, [modelWith([])], parent, original);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('The connection changed')
  );
  expect(setProviderModel).not.toHaveBeenCalled();
  expect(certifyProviderModel).not.toHaveBeenCalled();
});

it('advances versions only from its own successful replies across multiple reviewed models', async () => {
  const initial = {
    ...providerOf('openai'),
    etag: '11111111-1111-4111-8111-111111111111'
  };
  const afterSet = { ...initial, etag: '22222222-2222-4222-8222-222222222222' };
  const afterCert = {
    ...initial,
    etag: '33333333-3333-4333-8333-333333333333'
  };
  vi.mocked(getProvider)
    .mockResolvedValueOnce(initial)
    .mockResolvedValue(afterCert);
  vi.mocked(setProviderModel)
    .mockResolvedValueOnce(afterSet)
    .mockResolvedValue(afterCert);
  vi.mocked(certifyProviderModel).mockResolvedValue({
    result: { results: [{ succeeded: true }] },
    etag: `"${afterCert.etag}"`
  } as never);
  await renderValidation(
    undefined,
    [modelWith([]), { ...modelWith([]), id: 'model-2' }],
    initial
  );
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Checked 2 models')
  );
  expect(setProviderModel).toHaveBeenCalledTimes(2);
  expect(
    vi.mocked(setProviderModel).mock.calls.map(([provider]) => provider.etag)
  ).toEqual([initial.etag, afterCert.etag]);
});

it('stops before a subsequent reviewed model when another actor advances the parent', async () => {
  const initial = providerOf('openai');
  vi.mocked(getProvider)
    .mockResolvedValueOnce(initial)
    .mockResolvedValue({ ...initial, etag: 'other-actor-version' });
  await renderValidation(undefined, [
    modelWith([]),
    { ...modelWith([]), id: 'model-2' }
  ]);
  await vi.waitFor(() =>
    expect(host.textContent).toContain('The connection changed')
  );
  expect(setProviderModel).toHaveBeenCalledTimes(1);
  expect(certifyProviderModel).toHaveBeenCalledTimes(1);
});

it.each(['declaration', 'certification'] as const)(
  'stops the version chain on an ambiguous %s reply until a new review',
  async (phase) => {
    vi.mocked(getProvider).mockResolvedValue(providerOf('openai'));
    if (phase === 'declaration')
      vi.mocked(setProviderModel).mockRejectedValue(
        new TypeError('reply lost')
      );
    else
      vi.mocked(certifyProviderModel).mockRejectedValue(
        new TypeError('reply lost')
      );
    const { validate } = await renderValidation(undefined, [
      modelWith([]),
      { ...modelWith([]), id: 'model-2' }
    ]);
    await vi.waitFor(() =>
      expect(host.textContent).toContain('Stopped after 0 models')
    );
    expect(validate.disabled).toBe(true);
    expect(getProvider).toHaveBeenCalledTimes(1);
    expect(setProviderModel).toHaveBeenCalledTimes(1);
    expect(certifyProviderModel).toHaveBeenCalledTimes(
      phase === 'declaration' ? 0 : 1
    );
  }
);

it('rejects an inventory assembled across multiple parent versions', async () => {
  const initial = providerOf('openai');
  vi.mocked(listProviderModelPage)
    .mockResolvedValueOnce({
      provider: initial,
      items: [modelWith([])],
      nextCursor: 'more'
    } as never)
    .mockResolvedValue({
      provider: { ...initial, etag: 'new-inventory-version' },
      items: [{ ...modelWith([]), id: 'model-2' }],
      nextCursor: null
    } as never);
  component = mount(ProviderBulkModelsProbe, {
    target: host,
    props: { client, provider: initial }
  });
  flushSync();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Models could not be loaded')
  );
  expect(listProviderModelPage).toHaveBeenCalledTimes(2);
  expect(host.querySelector('input[type="checkbox"]')).toBeNull();
  expect(setProviderModel).not.toHaveBeenCalled();
});
