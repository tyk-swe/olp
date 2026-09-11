import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { apiClient } from '$lib/api/client';
import { apiKeyQueries } from '$lib/features/access/api-keys/apiKeyQueries';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import {
  updateProvider,
  getProvider,
  createProvider,
  probeProvider
} from '$lib/features/providers/api';
import {
  listProviderModelPage,
  listProviderModelInventory
} from '$lib/features/providers/models';
import {
  getRouteDraft,
  replaceRouteDraft,
  diffRouteRevisions
} from '$lib/features/routes/api';
import DraftEditorProbe from './test/DraftEditorProbe.svelte';
import { draft, provider, providerSpec } from './test/draftFixtures';

const navigation = vi.hoisted(() => ({ beforeNavigate: vi.fn() }));
vi.mock('$app/navigation', () => ({ ...navigation, goto: vi.fn() }));
vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/api-keys') }
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/providers/api', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/api')>()),
  getProvider: vi.fn(),
  createProvider: vi.fn(),
  probeProvider: vi.fn(),
  updateProvider: vi.fn()
}));
vi.mock('$lib/features/providers/models', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/models')>()),
  listProviderModelPage: vi.fn(),
  listProviderModelInventory: vi.fn()
}));
vi.mock('$lib/features/routes/api', async (original) => ({
  ...(await original<typeof import('$lib/features/routes/api')>()),
  getRouteDraft: vi.fn(),
  diffRouteRevisions: vi.fn(),
  replaceRouteDraft: vi.fn()
}));

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(routeKeys.draft(draft.id), draft);
  client.setQueryData(providerKeys.enabledModels(), []);
  client.setQueryData(providerKeys.kinds(), [providerSpec]);
  client.setQueryData(providerKeys.models(provider.id), {
    provider,
    items: [],
    nextCursor: null
  });
  client.setQueryData(providerKeys.credentials(provider.id), []);
  client.setQueryData(providerKeys.revisions(provider.id), {
    items: [],
    nextCursor: null
  });
  client.setQueryData(providerKeys.capabilityOptions('openai'), {
    capabilities: []
  });
  vi.mocked(listProviderModelPage).mockResolvedValue({
    provider,
    items: [],
    nextCursor: null
  });
  vi.mocked(getRouteDraft).mockResolvedValue(draft);
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
  window.history.replaceState({}, '', '/');
  vi.restoreAllMocks();
});

function render(kind: 'route' | 'provider' | 'key' | 'history' | 'wizard') {
  component = mount(DraftEditorProbe, {
    target: host,
    props: { client, kind }
  });
  flushSync();
}

function field(kind: 'route' | 'provider') {
  const input = host.querySelector<HTMLInputElement>(
    kind === 'route' ? '#route-slug' : '#detail-name'
  );
  if (!input) throw new Error('Draft editor did not render');
  return input;
}

function edit(kind: 'route' | 'provider', value: string) {
  const input = field(kind);
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function button(label: string) {
  const result = [...host.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === label
  );
  if (!result) throw new Error(`Missing button: ${label}`);
  return result;
}

function save(kind: 'route' | 'provider') {
  if (kind === 'route') {
    host
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  } else {
    button('Save draft').dispatchEvent(
      new MouseEvent('click', { bubbles: true })
    );
  }
}

function navigationBlocked() {
  vi.spyOn(window, 'confirm').mockReturnValue(false);
  const cancel = vi.fn();
  const navigationEvent = { cancel };
  for (const [callback] of navigation.beforeNavigate.mock.calls)
    callback(navigationEvent);
  return cancel.mock.calls.length > 0;
}

function expectSavedRequest(
  kind: 'route' | 'provider',
  call: number,
  etag: string,
  value: string
) {
  expect(
    kind === 'route' ? replaceRouteDraft : updateProvider
  ).toHaveBeenNthCalledWith(
    call,
    kind === 'route' ? draft.id : provider.id,
    etag,
    expect.objectContaining(
      kind === 'route' ? { slug: value } : { name: value }
    )
  );
}

it('opens Route Studio when the model inventory finishes before the draft', async () => {
  client.removeQueries({ queryKey: routeKeys.draft(draft.id) });
  client.removeQueries({ queryKey: providerKeys.enabledModels() });
  const pendingDraft = Promise.withResolvers<typeof draft>();
  vi.mocked(getRouteDraft).mockReturnValue(pendingDraft.promise);
  vi.mocked(listProviderModelInventory).mockResolvedValue([]);
  render('route');
  await vi.waitFor(() => {
    expect(client.getQueryState(providerKeys.enabledModels())?.status).toBe(
      'success'
    );
  });
  pendingDraft.resolve(draft);
  await vi.waitFor(() => {
    flushSync();
    expect(field('route').value).toBe(draft.slug);
    expect(host.textContent).not.toContain('Loading Route Studio');
  });
});

it('preserves resolved connection options when returning to a saved wizard draft', async () => {
  const options = {
    vendor_id: 'openai',
    credential_headers: ['x-service-key'],
    limits: {
      requests_per_minute: 10,
      tokens_per_minute: null,
      max_concurrency: null
    },
    parameter_defaults: { temperature: 0.4 },
    models: {
      'model-a': {
        canonical_model: 'organization/model-a',
        input_modalities: ['text'],
        output_modalities: ['text'],
        context_length: 4096,
        max_output_tokens: 1024,
        supported_parameters: ['temperature'],
        quantization: null,
        region: null,
        data_collection: null,
        zero_data_retention: null,
        deployment: null,
        source: null,
        observed_at: null
      }
    }
  };
  const source = {
    ...provider,
    configuration: {
      ...provider.configuration,
      auth_mode: 'headers' as const,
      endpoint: 'https://provider.example/v1',
      options
    }
  };
  let saved = { ...source, id: 'provider-copy', name: 'Copied provider' };
  client.setQueryData(providerKeys.kinds(), [
    {
      ...providerSpec,
      fields: [{ field: 'endpoint', required: true, label: 'Endpoint' }],
      auth_modes: [
        ...providerSpec.auth_modes,
        { mode: 'headers', label: 'Custom headers', credential: 'required' }
      ]
    }
  ]);
  client.setQueryData(['provider-vendors'], []);
  vi.mocked(getProvider).mockResolvedValue(source);
  vi.mocked(createProvider).mockResolvedValue(saved.id);
  vi.mocked(updateProvider).mockImplementation(async () => {
    saved = { ...saved, etag: 'updated' };
    return saved;
  });
  vi.mocked(listProviderModelPage).mockImplementation(async () => ({
    provider: saved,
    items: [],
    nextCursor: null
  }));
  vi.mocked(probeProvider).mockResolvedValue({
    succeeded: true,
    detail: 'Ready',
    probe_type: 'model_listing',
    discovered_models: 0
  } as never);
  window.history.replaceState({}, '', `/?copy=${provider.id}`);
  render('wizard');
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector<HTMLInputElement>('#provider-name')?.value).toBe(
      `${provider.name} copy`
    );
  });
  const secret = host.querySelector<HTMLInputElement>('#provider-secret')!;
  secret.value = '{"x-service-key":"new-secret"}';
  secret.dispatchEvent(new Event('input', { bubbles: true }));
  button('Save and test connection')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => {
    flushSync();
    expect(button('Back').disabled).toBe(false);
  });
  button('Back').click();
  flushSync();
  button('Save and test connection')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(updateProvider).toHaveBeenCalledWith(
      saved.id,
      provider.etag,
      expect.objectContaining({
        configuration: expect.objectContaining({ options })
      })
    )
  );
});

for (const kind of ['route', 'provider'] as const) {
  describe(`${kind} draft saves`, () => {
    it('keeps intervening edits dirty and saves them next with the returned ETag', async () => {
      const pendingRoute = Promise.withResolvers<typeof draft>();
      const pendingProvider = Promise.withResolvers<typeof provider>();
      vi.mocked(replaceRouteDraft).mockReturnValueOnce(pendingRoute.promise);
      vi.mocked(updateProvider).mockReturnValueOnce(pendingProvider.promise);
      render(kind);
      edit(kind, 'submitted-value');
      save(kind);
      save(kind);
      flushSync();
      edit(kind, 'newer-value');
      const updated = { ...provider, name: 'submitted-value', etag: 'v2' };
      vi.mocked(listProviderModelPage).mockResolvedValue({
        provider: updated,
        items: [],
        nextCursor: null
      });
      pendingRoute.resolve({ ...draft, slug: 'submitted-value', etag: 'v2' });
      pendingProvider.resolve(updated);
      await vi.waitFor(() => {
        flushSync();
        expect(host.textContent).toContain(
          'Draft saved. You have additional unsaved changes.'
        );
      });
      expect(field(kind).value).toBe('newer-value');
      expect(navigationBlocked()).toBe(true);
      if (kind === 'route') {
        for (const label of [
          'Simulate order',
          'Validate draft',
          'Activate route'
        ])
          expect(button(label).disabled).toBe(true);
        expect(client.getQueryData(routeKeys.draft(draft.id))).toMatchObject({
          slug: 'submitted-value'
        });
        vi.mocked(replaceRouteDraft).mockResolvedValue({
          ...draft,
          slug: 'newer-value',
          etag: 'v3'
        });
      } else {
        vi.mocked(updateProvider).mockResolvedValue({
          ...provider,
          name: 'newer-value',
          etag: 'v3'
        });
        vi.mocked(listProviderModelPage).mockResolvedValue({
          provider: { ...provider, name: 'newer-value', etag: 'v3' },
          items: [],
          nextCursor: null
        });
      }
      const mutation = kind === 'route' ? replaceRouteDraft : updateProvider;
      expect(mutation).toHaveBeenCalledTimes(1);
      expectSavedRequest(kind, 1, 'v1', 'submitted-value');
      save(kind);
      await vi.waitFor(() => {
        flushSync();
        expect(button('Save draft').disabled).toBe(false);
      });
      expectSavedRequest(kind, 2, 'v2', 'newer-value');
      expect(navigationBlocked()).toBe(false);
      expect(field(kind).value).toBe('newer-value');
    });

    it('keeps edits conservatively dirty when reverted during a save', async () => {
      const pendingRoute = Promise.withResolvers<typeof draft>();
      const pendingProvider = Promise.withResolvers<typeof provider>();
      vi.mocked(replaceRouteDraft).mockReturnValue(pendingRoute.promise);
      vi.mocked(updateProvider).mockReturnValue(pendingProvider.promise);
      render(kind);
      edit(kind, 'submitted-value');
      save(kind);
      edit(kind, 'temporary-value');
      edit(kind, 'submitted-value');
      pendingRoute.resolve({ ...draft, slug: 'submitted-value', etag: 'v2' });
      const savedProvider = {
        ...provider,
        name: 'submitted-value',
        etag: 'v2'
      };
      vi.mocked(listProviderModelPage).mockResolvedValue({
        provider: savedProvider,
        items: [],
        nextCursor: null
      });
      pendingProvider.resolve(savedProvider);
      await vi.waitFor(() => {
        flushSync();
        expect(host.textContent).toContain(
          'Draft saved. You have additional unsaved changes.'
        );
      });
      expect(field(kind).value).toBe('submitted-value');
      expect(navigationBlocked()).toBe(true);
    });

    it.each([
      new TypeError('Connection lost'),
      new ApiProblem({ title: 'Invalid draft', status: 422 }),
      new ApiProblem({
        title: 'Conflict',
        status: 412,
        type: 'https://openllmproxy.dev/problems/etag_mismatch'
      })
    ])('preserves input and dirty state after $message', async (error) => {
      vi.mocked(replaceRouteDraft).mockRejectedValue(error);
      vi.mocked(updateProvider).mockRejectedValue(error);
      render(kind);
      edit(kind, 'unsaved-value');
      save(kind);
      await vi.waitFor(() => {
        flushSync();
        expect(host.textContent).toContain(
          error instanceof ApiProblem && error.problem.status === 412
            ? 'This item changed elsewhere.'
            : error.message
        );
      });
      expect(field(kind).value).toBe('unsaved-value');
      expect(navigationBlocked()).toBe(true);
      expect(button('Save draft').disabled).toBe(false);
    });
  });
}

it('retries a failed provider refresh without repeating the successful write', async () => {
  vi.mocked(updateProvider).mockResolvedValue({
    ...provider,
    name: 'saved-value',
    etag: 'v2'
  });
  vi.mocked(listProviderModelPage).mockRejectedValue(
    new TypeError('Refresh unavailable')
  );
  render('provider');
  edit('provider', 'saved-value');
  save('provider');
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Draft saved, but refreshing failed.');
  });
  expect(navigationBlocked()).toBe(false);
  edit('provider', 'newer-value');
  vi.mocked(listProviderModelPage).mockResolvedValue({
    provider: { ...provider, name: 'saved-value', etag: 'v2' },
    items: [],
    nextCursor: null
  });
  button('Retry refresh').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).not.toContain(
      'Draft saved, but refreshing failed.'
    );
    expect(button('Save draft').disabled).toBe(false);
  });
  expect(updateProvider).toHaveBeenCalledTimes(1);
  expect(field('provider').value).toBe('newer-value');
  expect(navigationBlocked()).toBe(true);
});

it('serializes route reload with saves and other reloads', async () => {
  vi.mocked(replaceRouteDraft).mockRejectedValue(
    new ApiProblem({
      title: 'Conflict',
      status: 412,
      type: 'https://openllmproxy.dev/problems/etag_mismatch'
    })
  );
  render('route');
  edit('route', 'local-value');
  save('route');
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('This item changed elsewhere.');
  });
  const pending = Promise.withResolvers<typeof draft>();
  vi.mocked(getRouteDraft).mockReturnValue(pending.promise);
  const reload = button('Reload');
  reload.dispatchEvent(new MouseEvent('click', { bubbles: true }));
  reload.dispatchEvent(new MouseEvent('click', { bubbles: true }));
  save('route');
  flushSync();
  expect(getRouteDraft).toHaveBeenCalledTimes(1);
  expect(replaceRouteDraft).toHaveBeenCalledTimes(1);
  expect(button('Save draft').disabled).toBe(true);
  pending.resolve({ ...draft, slug: 'remote-value', etag: 'v2' });
  await vi.waitFor(() => {
    flushSync();
    expect(field('route').value).toBe('remote-value');
    expect(button('Save draft').disabled).toBe(false);
  });
  expect(navigationBlocked()).toBe(false);
});

it('does not submit an invalid route draft', () => {
  render('route');
  edit('route', '');
  save('route');
  flushSync();
  expect(replaceRouteDraft).not.toHaveBeenCalled();
  expect(navigationBlocked()).toBe(true);
  expect(button('Save draft').disabled).toBe(false);
});

it('preserves dirty route fields and advances their ETag after a policy save', async () => {
  const policyKey = ['routing-policy', 'route-draft', draft.id, draft.etag];
  client.setQueryData(policyKey, { policy: {}, etag: draft.etag });
  const updated = { ...draft, etag: 'policy-saved' };
  const put = vi.spyOn(apiClient, 'PUT').mockResolvedValue({
    data: { policy: {}, etag: updated.etag },
    response: new Response(null, { status: 200 })
  } as never);
  vi.spyOn(apiClient, 'GET').mockResolvedValue({
    data: { policy: {}, etag: updated.etag },
    response: new Response(null, { status: 200 })
  } as never);
  vi.mocked(getRouteDraft).mockResolvedValue(updated);
  render('route');
  edit('route', 'unsaved-target-route');
  button('Save routing policy')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => {
    flushSync();
    expect(put).toHaveBeenCalled();
    expect(host.textContent).toContain('Routing policy staged.');
  });
  expect(field('route').value).toBe('unsaved-target-route');
  expect(navigationBlocked()).toBe(true);
  vi.mocked(replaceRouteDraft).mockResolvedValue({
    ...updated,
    slug: 'unsaved-target-route',
    etag: 'draft-saved'
  });
  save('route');
  await vi.waitFor(() =>
    expectSavedRequest('route', 1, updated.etag, 'unsaved-target-route')
  );
});

it('blocks route publication and navigation until policy edits finish saving', async () => {
  client.setQueryData(['routing-policy', 'route-draft', draft.id, draft.etag], {
    policy: {},
    etag: draft.etag
  });
  const updated = { ...draft, etag: 'saved-policy' };
  const refreshed = Promise.withResolvers<typeof draft>();
  vi.mocked(getRouteDraft).mockReturnValue(refreshed.promise);
  vi.spyOn(apiClient, 'PUT').mockResolvedValue({
    data: { policy: {}, etag: updated.etag },
    response: new Response(null, { status: 200 })
  } as never);
  vi.spyOn(apiClient, 'GET').mockResolvedValue({
    data: { policy: {}, etag: updated.etag },
    response: new Response(null, { status: 200 })
  } as never);
  render('route');
  const labels = ['Simulate order', 'Validate draft', 'Activate route'];
  for (const label of labels) expect(button(label).disabled).toBe(false);
  const policy = host.querySelector<HTMLTextAreaElement>(
    '#routing-policy-route-draft-route-a'
  )!;
  policy.value = '{"constraints":{"require_zero_data_retention":true}}';
  policy.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  for (const label of labels) expect(button(label).disabled).toBe(true);
  expect(navigationBlocked()).toBe(true);
  button('Save routing policy')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(getRouteDraft).toHaveBeenCalled());
  flushSync();
  for (const label of labels) expect(button(label).disabled).toBe(true);
  refreshed.resolve(updated);
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Routing policy staged.');
  });
  for (const label of labels) expect(button(label).disabled).toBe(false);
  expect(navigationBlocked()).toBe(false);
});

it.each(['constraints', 'defaults'] as const)(
  'blocks saving and publishing malformed nested policy %s until corrected',
  async (key) => {
    client.setQueryData(
      ['routing-policy', 'route-draft', draft.id, draft.etag],
      {
        policy: {},
        etag: draft.etag
      }
    );
    const other = key === 'constraints' ? 'defaults' : 'constraints';
    const corrected = { require_zero_data_retention: true };
    const policy = { [key]: corrected, [other]: { require_parameters: true } };
    const updated = { ...draft, etag: 'saved-nested-policy' };
    vi.mocked(getRouteDraft).mockResolvedValue(updated);
    const put = vi.spyOn(apiClient, 'PUT').mockResolvedValue({
      data: { policy, etag: updated.etag },
      response: new Response(null, { status: 200 })
    } as never);
    vi.spyOn(apiClient, 'GET').mockResolvedValue({
      data: { policy, etag: updated.etag },
      response: new Response(null, { status: 200 })
    } as never);
    render('route');
    const nested = (section: string) =>
      host.querySelector<HTMLTextAreaElement>(
        `#policy-${section}-route-draft-route-a-json`
      )!;
    const invalid = nested(key);
    invalid.value = '{"strategy":"price"';
    invalid.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    nested(other).value = JSON.stringify(policy[other]);
    nested(other).dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    expect(invalid.value).toBe('{"strategy":"price"');
    for (const label of [
      'Save routing policy',
      'Simulate order',
      'Validate draft',
      'Activate route'
    ]) {
      expect(button(label).disabled).toBe(true);
    }
    expect(navigationBlocked()).toBe(true);
    const form = button('Save routing policy').closest('form')!;
    form.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
    await Promise.resolve();
    expect(put).not.toHaveBeenCalled();
    expect(host.textContent).toContain('Enter valid JSON objects');
    invalid.value = JSON.stringify(corrected);
    invalid.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    expect(button('Save routing policy').disabled).toBe(false);
    form.dispatchEvent(
      new Event('submit', { bubbles: true, cancelable: true })
    );
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Routing policy staged.');
    });
    expect(put).toHaveBeenCalledWith(
      '/api/v3/routing-policies/{scope}/{id}',
      expect.objectContaining({ body: policy })
    );
    expect(navigationBlocked()).toBe(false);
  }
);

it('reloads malformed nested policy input even when the saved JSON is unchanged', async () => {
  const saved = { policy: {}, etag: draft.etag };
  client.setQueryData(
    ['routing-policy', 'route-draft', draft.id, draft.etag],
    saved
  );
  vi.spyOn(apiClient, 'GET').mockResolvedValue({
    data: saved,
    response: new Response(null, { status: 200 })
  } as never);
  render('route');
  const input = host.querySelector<HTMLTextAreaElement>(
    '#policy-defaults-route-draft-route-a-json'
  )!;
  input.value = '{';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  expect(button('Save routing policy').disabled).toBe(true);
  button('Reload policy').click();
  await vi.waitFor(() => {
    flushSync();
    expect(input.value).toBe('{}');
    expect(button('Save routing policy').disabled).toBe(false);
  });
  expect(navigationBlocked()).toBe(false);
  expect(button('Activate route').disabled).toBe(false);
});

it('shows before and after policies when only the routing policy changed', async () => {
  const constraints = {
    deny_data_collection: false,
    ignore: [],
    max_price: null,
    only: null,
    quantizations: null,
    regions: null,
    require_parameters: false,
    require_zero_data_retention: false
  };
  const before = {
    constraints: { ...constraints, ignore: ['vendor:deepseek'] },
    allowed_strategies: null,
    defaults: {
      ...constraints,
      allow_fallbacks: null,
      order: null,
      preferred_max_latency_ms: null,
      preferred_min_throughput: null,
      strategy: null
    }
  };
  const after = {
    ...before,
    constraints: { ...constraints, ignore: ['vendor:cohere'] }
  };
  client.setQueryData(routeKeys.revisions('route-a'), [
    {
      ...draft,
      route_id: 'route-a',
      id: 'revision-2',
      revision: 2,
      activated_at: draft.updated_at,
      activated_by: 'owner',
      source_draft_id: draft.id,
      routing_policy: after
    },
    {
      ...draft,
      route_id: 'route-a',
      id: 'revision-1',
      revision: 1,
      activated_at: draft.updated_at,
      activated_by: 'owner',
      source_draft_id: draft.id,
      routing_policy: before
    }
  ]);
  vi.mocked(diffRouteRevisions).mockResolvedValue({
    from_revision: 1,
    to_revision: 2,
    slug_changed: false,
    timeout_changed: false,
    max_attempts_changed: false,
    routing_policy_changed: true,
    routing_policy_before: before,
    routing_policy_after: after,
    operations_added: [],
    operations_removed: [],
    targets_added: [],
    targets_removed: [],
    targets_changed: []
  });
  render('history');
  button('Compare').click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('.policy-diff')).not.toBeNull();
  });
  expect(diffRouteRevisions).toHaveBeenCalledWith(
    'route-a',
    'revision-1',
    'revision-2'
  );
  const policies = [...host.querySelectorAll('.policy-diff pre')].map(
    (element) => JSON.parse(element.textContent ?? '{}')
  );
  expect(policies).toEqual([before, after]);
  expect(
    host.querySelector('[aria-label="Revision differences"]')?.textContent
  ).toContain('routing policy');
});

it('uses the credential editor snapshot ETag after a background refetch', async () => {
  const poolKey = ['provider-slots', provider.id, provider.etag];
  const slot = {
    id: 'slot-a',
    name: 'Original slot',
    enabled: true,
    priority: 0,
    weight: 1,
    allowed_models: [],
    allowed_routes: [],
    allowed_api_keys: []
  };
  client.setQueryData(poolKey, { items: [slot], etag: 'original-etag' });
  const put = vi
    .spyOn(apiClient, 'PUT')
    .mockRejectedValue(new Error('Conflict'));
  render('provider');
  button('Edit / rotate').click();
  flushSync();
  client.setQueryData(poolKey, {
    items: [{ ...slot, name: 'Changed elsewhere' }],
    etag: 'newer-etag'
  });
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('Changed elsewhere');
  });
  button('Save credential')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(put).toHaveBeenCalledWith(
      '/api/v3/providers/{provider_id}/credential-slots/{slot_id}',
      expect.objectContaining({
        headers: expect.objectContaining({ 'If-Match': 'original-etag' }),
        body: expect.objectContaining({
          slot: expect.objectContaining({ name: 'Original slot' })
        })
      })
    )
  );
});

it('locks credential fields until a pending save completes', async () => {
  client.setQueryData(['provider-slots', provider.id, provider.etag], {
    items: [
      {
        id: 'slot-a',
        name: 'Original slot',
        enabled: true,
        priority: 0,
        weight: 1,
        allowed_models: [],
        allowed_routes: [],
        allowed_api_keys: []
      }
    ],
    etag: provider.etag
  });
  const pending = Promise.withResolvers<never>();
  vi.spyOn(apiClient, 'PUT').mockReturnValue(pending.promise);
  render('provider');
  button('Edit / rotate').click();
  flushSync();
  const credential = host.querySelector<HTMLInputElement>(
    '.pool input[type="password"]'
  )!;
  credential.value = 'replacement-secret';
  credential.dispatchEvent(new Event('input', { bubbles: true }));
  button('Save credential')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();
  for (const input of host.querySelectorAll('.pool input'))
    expect(input.matches(':disabled')).toBe(true);
  expect(button('Cancel').matches(':disabled')).toBe(true);
  pending.reject(new Error('Conflict'));
  await vi.waitFor(() => {
    flushSync();
    expect(credential.matches(':disabled')).toBe(false);
  });
  expect(credential.value).toBe('replacement-secret');
});

it('preserves options input and its original ETag across provider refreshes', async () => {
  vi.mocked(updateProvider).mockRejectedValue(new Error('Conflict'));
  render('provider');
  const options = host.querySelector<HTMLTextAreaElement>('#provider-options')!;
  const local = '{"parameter_defaults":{"temperature":0.4}}';
  options.value = local;
  options.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  const remote = {
    ...provider,
    etag: 'newer-options',
    configuration: {
      ...provider.configuration,
      options: { parameter_defaults: { temperature: 0.8 } }
    }
  };
  client.setQueryData(providerKeys.models(provider.id), {
    provider: remote,
    items: [],
    nextCursor: null
  });
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain(
      'This provider changed while you were editing.'
    );
  });
  expect(options.value).toBe(local);
  expect(navigationBlocked()).toBe(true);
  button('Save options to draft')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(updateProvider).toHaveBeenCalledWith(
      provider.id,
      provider.etag,
      expect.objectContaining({
        configuration: expect.objectContaining({ options: JSON.parse(local) })
      })
    )
  );
  await vi.waitFor(() =>
    expect(button('Reload saved options').disabled).toBe(false)
  );
  button('Reload saved options').click();
  flushSync();
  expect(JSON.parse(options.value)).toEqual(remote.configuration.options);
});

it.each(['original-key', 'changed-key'])(
  'only adopts a routing-policy ETag from the API-key form baseline (%s)',
  async (policyEtag) => {
    const key = {
      id: 'key-a',
      name: 'Original key',
      etag: 'original-key',
      lookup_id: 'lookup-a',
      scopes: ['inference'],
      allowed_routes: [],
      created_by: 'owner-a',
      created_by_email: 'owner@test.example',
      created_at: '2026-07-12T12:00:00Z',
      budget: {
        daily: {
          accrued: '0',
          limit: null,
          window_ends_at: '2026-07-13T00:00:00Z'
        },
        monthly: {
          accrued: '0',
          limit: null,
          window_ends_at: '2026-08-01T00:00:00Z'
        },
        unpriced_attempts: 0
      }
    };
    client.setQueryData(apiKeyQueries.page(), {
      items: [key],
      nextCursor: null
    });
    client.setQueryData(routeKeys.all(), []);
    const policyKey = ['routing-policy', 'api-key', key.id, key.etag];
    client.setQueryData(policyKey, { policy: {}, etag: key.etag });
    vi.spyOn(apiClient, 'GET').mockResolvedValue({
      data: { policy: {}, etag: 'saved-policy' },
      response: new Response(null, { status: 200 })
    } as never);
    const saved = {
      data: { policy: {}, etag: 'saved-policy' },
      response: new Response(null, { status: 200 })
    };
    const pendingPolicy = Promise.withResolvers<typeof saved>();
    const put = vi
      .spyOn(apiClient, 'PUT')
      .mockReturnValue(pendingPolicy.promise as never);
    const patch = vi
      .spyOn(apiClient, 'PATCH')
      .mockRejectedValue(new Error('Conflict'));
    render('key');
    button('Edit').click();
    flushSync();
    client.setQueryData(policyKey, { policy: {}, etag: policyEtag });
    await vi.waitFor(() => {
      flushSync();
      expect(button('Save routing policy').disabled).toBe(false);
    });
    const keyName = host.querySelector<HTMLInputElement>('#key-name')!;
    keyName.value = 'Local key';
    keyName.dispatchEvent(new Event('input', { bubbles: true }));
    const policy = host.querySelector<HTMLTextAreaElement>(
      '#routing-policy-api-key-key-a'
    )!;
    policy.value = '{"constraints":{"require_zero_data_retention":true}}';
    policy.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    expect(button('Save and publish').disabled).toBe(true);
    button('Save and publish')
      .closest('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    expect(patch).not.toHaveBeenCalled();
    expect(policy.value).toContain('require_zero_data_retention');
    button('Save routing policy')
      .closest('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(put).toHaveBeenCalled());
    flushSync();
    expect(button('Save and publish').disabled).toBe(true);
    expect(button('Cancel').disabled).toBe(true);
    pendingPolicy.resolve(saved);
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('Routing policy published.');
    });
    expect(keyName.value).toBe('Local key');
    expect(button('Save and publish').disabled).toBe(false);
    button('Save and publish')
      .closest('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() =>
      expect(patch).toHaveBeenCalledWith(
        '/api/v3/api-keys/{api_key_id}',
        expect.objectContaining({
          params: expect.objectContaining({
            header: {
              'If-Match': policyEtag === key.etag ? 'saved-policy' : key.etag
            }
          })
        })
      )
    );
  }
);
