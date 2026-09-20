import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { apiClient } from '$lib/api/client';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import {
  listProviderModelInventory,
  type ProviderModelInventory
} from '$lib/features/providers/models';
import {
  activateRoute,
  createRouteDraft,
  deleteRouteDraft,
  getRouteDraft,
  replaceRouteDraft,
  simulateRoute,
  validateRoute,
  type RouteActivation,
  type RouteDraft
} from '$lib/features/routes/api';
import RouteLifetimeProbe from './test/RouteLifetimeProbe.svelte';
import { draft } from '$lib/forms/test/draftFixtures';

const navigation = vi.hoisted(() => ({
  callbacks: new Set<(event: { cancel: () => void }) => void>(),
  goto: vi.fn()
}));
// Emulate SvelteKit's beforeNavigate lifecycle: guards registered by a
// component stop firing once that component is destroyed.
vi.mock('$app/navigation', async () => {
  const { onDestroy } = await import('svelte');
  return {
    goto: navigation.goto,
    beforeNavigate: (callback: (event: { cancel: () => void }) => void) => {
      navigation.callbacks.add(callback);
      onDestroy(() => {
        navigation.callbacks.delete(callback);
      });
    }
  };
});
vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/routes/route-a') }
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/providers/models', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/models')>()),
  listProviderModelInventory: vi.fn()
}));
vi.mock('$lib/features/routes/api', async (original) => ({
  ...(await original<typeof import('$lib/features/routes/api')>()),
  getRouteDraft: vi.fn(),
  createRouteDraft: vi.fn(),
  replaceRouteDraft: vi.fn(),
  deleteRouteDraft: vi.fn(),
  validateRoute: vi.fn(),
  activateRoute: vi.fn(),
  simulateRoute: vi.fn()
}));

const draftA: RouteDraft = draft;
const draftB: RouteDraft = {
  ...draft,
  id: 'route-b',
  slug: 'second-route',
  etag: 'w1'
};
const drafts: Record<string, RouteDraft | Promise<RouteDraft>> = {};

const model: ProviderModelInventory = {
  available: true,
  metadata: {
    canonical_model: null,
    context_length: null,
    data_collection: null,
    deployment: null,
    input_modalities: ['text'],
    max_output_tokens: null,
    output_modalities: ['text'],
    quantization: null,
    region: null,
    source: null,
    observed_at: null,
    supported_parameters: null,
    zero_data_retention: null
  },
  provider_id: 'provider-a',
  provider_name: 'Original provider',
  provider_kind: 'openai',
  model: {
    id: 'model-a',
    upstream_model: 'test-model',
    display_name: 'test-model',
    enabled: true,
    capabilities: [
      {
        operation: 'generation',
        surface: 'openai',
        mode: 'streaming',
        source: 'certified'
      }
    ]
  }
};

const etagMismatch = () =>
  new ApiProblem({
    title: 'Conflict',
    status: 412,
    type: 'https://openllmproxy.dev/problems/etag_mismatch'
  });

const okResponse = () => new Response(null, { status: 200 });
const policyKey = (scope: string, id: string) => [
  'routing-policy',
  scope,
  id,
  ''
];

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  navigation.callbacks.clear();
  drafts['route-a'] = draftA;
  drafts['route-b'] = draftB;
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(routeKeys.draft(draftA.id), draftA);
  client.setQueryData(routeKeys.draft(draftB.id), draftB);
  client.setQueryData(providerKeys.enabledModels(), [model]);
  vi.mocked(getRouteDraft).mockImplementation(async (id) => {
    const value = drafts[id];
    if (!value) throw new ApiProblem({ title: 'Not found', status: 404 });
    return value;
  });
  vi.mocked(listProviderModelInventory).mockResolvedValue([model]);
  // The routing-policy child mounts with the draft editor; resolve its GETs
  // from whatever the test seeded under the matching policy key.
  vi.spyOn(apiClient, 'GET').mockImplementation(async (_path, init) => {
    const path = (
      init as { params?: { path?: { scope?: string; id?: string } } }
    )?.params?.path;
    const cached = client.getQueryData(
      policyKey(path?.scope ?? '', path?.id ?? '')
    );
    return {
      data: cached ?? { policy: {}, etag: 'p0' },
      response: okResponse()
    } as never;
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  window.history.replaceState({}, '', '/');
  vi.restoreAllMocks();
});

function render(props: {
  routeId?: string;
  keyed?: boolean;
  policy?: boolean;
  onPolicySaved?: (etag: string, previousEtag: string) => void | Promise<void>;
}) {
  component = mount(RouteLifetimeProbe, {
    target: host,
    props: { client, ...props }
  });
  flushSync();
}

function field() {
  const input = host.querySelector<HTMLInputElement>('#route-slug');
  if (!input) throw new Error('Route slug input did not render');
  return input;
}

function policyField() {
  const area = host.querySelector<HTMLTextAreaElement>(
    'textarea[id^="routing-policy-"]'
  );
  if (!area) throw new Error('Routing policy textarea did not render');
  return area;
}

function policySection(section: string, id: string) {
  const area = host.querySelector<HTMLTextAreaElement>(
    `#policy-${section}-route-draft-${id}-json`
  );
  if (!area) throw new Error('Policy section input did not render');
  return area;
}

function saveButton() {
  const result = host.querySelector<HTMLButtonElement>(
    'form.studio button[type="submit"]'
  );
  if (!result) throw new Error('Save button did not render');
  return result;
}

function flags() {
  const probe = host.querySelector('[data-probe="policy-flags"]');
  if (!probe) throw new Error('Policy flag probe did not render');
  return probe.textContent ?? '';
}

function button(label: string) {
  const result = [...host.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === label
  );
  if (!result) throw new Error(`Missing button: ${label}`);
  return result;
}

function edit(value: string) {
  field().value = value;
  field().dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function editPolicy(value: string) {
  policyField().value = value;
  policyField().dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function save() {
  host
    .querySelector('form.studio')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

function submitPolicy() {
  button('Save routing policy')
    .closest('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

function openRoute(id: string) {
  button(`Open ${id}`).dispatchEvent(
    new MouseEvent('click', { bubbles: true })
  );
  flushSync();
}

function navigationBlocked() {
  vi.spyOn(window, 'confirm').mockReturnValue(false);
  const cancel = vi.fn();
  const event = { cancel };
  for (const callback of navigation.callbacks) callback(event);
  return cancel.mock.calls.length > 0;
}

async function hydrated(slug: string) {
  await vi.waitFor(() => {
    flushSync();
    expect(field().value).toBe(slug);
  });
}

async function settled() {
  await vi.waitFor(() => {
    flushSync();
    expect(saveButton().disabled).toBe(false);
  });
}

describe('route draft resource lifetime', () => {
  it.each([false, true])(
    'hydrates the target draft cleanly after accepted navigation (keyed=%s)',
    async (keyed) => {
      render({ keyed });
      await hydrated(draftA.slug);
      edit('discarded-a-slug');
      openRoute('route-b');
      await hydrated(draftB.slug);
      expect(host.textContent).not.toContain('discarded-a-slug');
      expect(host.querySelector('[role="alert"]')).toBeNull();
      expect(host.querySelector('.success-banner')).toBeNull();
      expect(navigationBlocked()).toBe(false);
      // B's own ETag must be in effect: a save on B sends B's snapshot.
      vi.mocked(replaceRouteDraft).mockResolvedValue({
        ...draftB,
        slug: 'second-route-v2',
        etag: 'w2'
      });
      edit('second-route-v2');
      save();
      await vi.waitFor(() =>
        expect(replaceRouteDraft).toHaveBeenCalledWith(
          'route-b',
          'w1',
          expect.objectContaining({ slug: 'second-route-v2' })
        )
      );
    }
  );

  it('keeps the edited draft and its ETag when navigation is cancelled', async () => {
    render({});
    await hydrated(draftA.slug);
    edit('kept-a-slug');
    expect(navigationBlocked()).toBe(true);
    expect(field().value).toBe('kept-a-slug');
    vi.mocked(replaceRouteDraft).mockResolvedValue({
      ...draftA,
      slug: 'kept-a-slug',
      etag: 'v2'
    });
    save();
    await vi.waitFor(() =>
      expect(replaceRouteDraft).toHaveBeenCalledWith(
        'route-a',
        'v1',
        expect.objectContaining({ slug: 'kept-a-slug' })
      )
    );
  });

  it.each([false, true])(
    'drops a save completing after the resource changed (keyed=%s)',
    async (keyed) => {
      const pending = Promise.withResolvers<RouteDraft>();
      vi.mocked(replaceRouteDraft).mockReturnValue(pending.promise);
      render({ keyed });
      await hydrated(draftA.slug);
      edit('stale-a-slug');
      save();
      openRoute('route-b');
      await hydrated(draftB.slug);
      pending.resolve({ ...draftA, slug: 'stale-a-slug', etag: 'v2' });
      await settled();
      expect(field().value).toBe(draftB.slug);
      expect(host.textContent).not.toContain('Draft saved.');
      expect(host.querySelector('[role="alert"]')).toBeNull();
      expect(navigationBlocked()).toBe(false);
      // The committed write still lands in the original draft's cache entry.
      expect(client.getQueryData(routeKeys.draft('route-a'))).toMatchObject({
        etag: 'v2'
      });
    }
  );

  it.each([false, true])(
    'drops a 412 completing after the resource changed (keyed=%s)',
    async (keyed) => {
      const pending = Promise.withResolvers<RouteDraft>();
      vi.mocked(replaceRouteDraft).mockReturnValue(pending.promise);
      render({ keyed });
      await hydrated(draftA.slug);
      edit('stale-a-slug');
      save();
      openRoute('route-b');
      await hydrated(draftB.slug);
      pending.reject(etagMismatch());
      await settled();
      expect(field().value).toBe(draftB.slug);
      expect(host.textContent).not.toContain('This item changed elsewhere.');
      expect(host.querySelector('[role="alert"]')).toBeNull();
      edit('b-local-slug');
      expect(host.textContent).not.toContain('This item changed elsewhere.');
      expect(navigationBlocked()).toBe(true);
    }
  );

  it('does not navigate when a pending create resolves after disposal', async () => {
    const pending = Promise.withResolvers<{ id: string }>();
    vi.mocked(createRouteDraft).mockReturnValue(pending.promise as never);
    render({ routeId: '' });
    await hydrated('default');
    edit('created-route');
    button('Add target').click();
    flushSync();
    save();
    await vi.waitFor(() => expect(createRouteDraft).toHaveBeenCalled());
    button('Unmount editor').click();
    flushSync();
    pending.resolve({ id: 'route-created' });
    await Promise.resolve();
    await Promise.resolve();
    flushSync();
    expect(navigation.goto).not.toHaveBeenCalled();
  });

  it.each(['unmount', 'navigate'] as const)(
    'does not navigate when a pending delete resolves after %s',
    async (mode) => {
      const pending = Promise.withResolvers<void>();
      vi.mocked(deleteRouteDraft).mockReturnValue(pending.promise);
      vi.spyOn(window, 'confirm').mockReturnValue(true);
      render({});
      await hydrated(draftA.slug);
      button('Delete draft').click();
      await vi.waitFor(() => expect(deleteRouteDraft).toHaveBeenCalled());
      if (mode === 'unmount') button('Unmount editor').click();
      else openRoute('route-b');
      flushSync();
      pending.resolve();
      await Promise.resolve();
      await Promise.resolve();
      flushSync();
      expect(navigation.goto).not.toHaveBeenCalled();
      if (mode === 'navigate') {
        await hydrated(draftB.slug);
        expect(host.querySelector('[role="alert"]')).toBeNull();
      }
    }
  );

  it('drops a stale reload completing after the resource changed', async () => {
    render({});
    await hydrated(draftA.slug);
    edit('a-conflicted-slug');
    // Force the conflict affordance so the reload button exists.
    vi.mocked(replaceRouteDraft).mockRejectedValue(etagMismatch());
    save();
    await vi.waitFor(() => {
      flushSync();
      expect(host.textContent).toContain('This item changed elsewhere.');
    });
    const pending = Promise.withResolvers<RouteDraft>();
    drafts['route-a'] = pending.promise;
    button('Reload').click();
    flushSync();
    openRoute('route-b');
    await hydrated(draftB.slug);
    pending.resolve({ ...draftA, slug: 'remote-a-slug', etag: 'v9' });
    await settled();
    expect(field().value).toBe(draftB.slug);
    expect(host.textContent).not.toContain('remote-a-slug');
    expect(navigationBlocked()).toBe(false);
  });

  it.each([
    [
      'Validate draft',
      'Validation passed',
      () => {
        const pending = Promise.withResolvers<never>();
        vi.mocked(validateRoute).mockReturnValue(pending.promise);
        return pending;
      }
    ],
    [
      'Activate route',
      'Route activated as revision',
      () => {
        const pending = Promise.withResolvers<never>();
        vi.mocked(activateRoute).mockReturnValue(pending.promise);
        return pending;
      }
    ],
    [
      'Simulate order',
      'Deterministic attempt order',
      () => {
        const pending = Promise.withResolvers<never>();
        vi.mocked(simulateRoute).mockReturnValue(pending.promise);
        return pending;
      }
    ]
  ] as const)(
    'drops a stale %s completing after the resource changed',
    async (label, noticeFragment, arrange) => {
      render({});
      await hydrated(draftA.slug);
      const pending = arrange();
      button(label).click();
      flushSync();
      openRoute('route-b');
      await hydrated(draftB.slug);
      pending.resolve({
        state: 'validated',
        etag: 'v2',
        revision: 7,
        draft_etag: 'v3',
        route_id: 'route-a',
        runtime_generation: { sequence: 4 },
        attempts: []
      } as never);
      await settled();
      expect(field().value).toBe(draftB.slug);
      expect(host.textContent).not.toContain(noticeFragment);
      expect(host.querySelector('[role="alert"]')).toBeNull();
    }
  );

  it('saves edits made during activation with the activation ETag', async () => {
    const pending = Promise.withResolvers<RouteActivation>();
    const refresh = Promise.withResolvers<RouteDraft>();
    vi.mocked(activateRoute).mockReturnValue(pending.promise);
    render({});
    await hydrated(draftA.slug);
    button('Activate route').click();
    await vi.waitFor(() => expect(activateRoute).toHaveBeenCalled());
    edit('edited-during-activation');
    drafts['route-a'] = refresh.promise;
    pending.resolve({
      draft_etag: 'v2',
      revision: 7,
      revision_id: 'revision-7',
      route_id: draftA.id,
      runtime_generation: { id: 'generation-4', sequence: 4 }
    });
    // Keep invalidation pending while the cache's activation ETag reconciles.
    await vi.waitFor(() => {
      flushSync();
      expect(getRouteDraft).toHaveBeenCalled();
      expect(client.getQueryData(routeKeys.draft(draftA.id))).toMatchObject({
        etag: 'v2'
      });
    });
    refresh.resolve({ ...draftA, etag: 'v2' });
    await settled();
    expect(field().value).toBe('edited-during-activation');
    expect(navigationBlocked()).toBe(true);
    expect(host.querySelector('.concurrent-notice')).toBeNull();
    vi.mocked(replaceRouteDraft).mockResolvedValue({
      ...draftA,
      slug: 'edited-during-activation',
      etag: 'v3'
    });
    save();
    await vi.waitFor(() =>
      expect(replaceRouteDraft).toHaveBeenCalledWith(
        draftA.id,
        'v2',
        expect.objectContaining({ slug: 'edited-during-activation' })
      )
    );
    await settled();
    expect(navigationBlocked()).toBe(false);
  });

  it('does not let a stale save release a newer save on the new resource', async () => {
    const stale = Promise.withResolvers<RouteDraft>();
    const fresh = Promise.withResolvers<RouteDraft>();
    vi.mocked(replaceRouteDraft)
      .mockReturnValueOnce(stale.promise)
      .mockReturnValueOnce(fresh.promise);
    render({});
    await hydrated(draftA.slug);
    edit('stale-a-slug');
    save();
    openRoute('route-b');
    await hydrated(draftB.slug);
    edit('b-slug');
    save();
    flushSync();
    expect(saveButton().disabled).toBe(true);
    // A's save resolving must not clear B's in-flight save state.
    stale.resolve({ ...draftA, slug: 'stale-a-slug', etag: 'v2' });
    await Promise.resolve();
    flushSync();
    expect(saveButton().disabled).toBe(true);
    fresh.resolve({ ...draftB, slug: 'b-slug', etag: 'w2' });
    await settled();
    expect(field().value).toBe('b-slug');
    expect(navigationBlocked()).toBe(false);
  });
});

describe('routing policy resource lifetime', () => {
  it('does not invoke the new resource callback or reset its edits when a stale policy save resolves', async () => {
    const onSaved = vi.fn();
    client.setQueryData(policyKey('route-draft', 'route-a'), {
      policy: { constraints: { only: ['vendor:a'] } },
      etag: 'pa1'
    });
    client.setQueryData(policyKey('route-draft', 'route-b'), {
      policy: { defaults: { strategy: 'weighted' } },
      etag: 'pb1'
    });
    const pending = Promise.withResolvers<{
      data: { policy: object; etag: string };
      response: Response;
    }>();
    vi.spyOn(apiClient, 'PUT').mockReturnValue(pending.promise as never);
    render({ policy: true, onPolicySaved: onSaved });
    await vi.waitFor(() => {
      flushSync();
      expect(policyField().value).toContain('vendor:a');
    });
    editPolicy('{"constraints":{"only":["vendor:local"]}}');
    submitPolicy();
    await vi.waitFor(() => expect(apiClient.PUT).toHaveBeenCalled());
    expect(flags()).toContain('busy:true');
    openRoute('route-b');
    await vi.waitFor(() => {
      flushSync();
      expect(policyField().value).toContain('weighted');
    });
    expect(flags()).toContain('busy:false');
    editPolicy('{"constraints":{"only":["vendor:b-local"]}}');
    pending.resolve({
      data: { policy: {}, etag: 'pa2' },
      response: okResponse()
    });
    await Promise.resolve();
    await Promise.resolve();
    flushSync();
    expect(onSaved).not.toHaveBeenCalled();
    expect(policyField().value).toContain('vendor:b-local');
    expect(flags()).toContain('dirty:true');
    expect(host.textContent).not.toContain('Routing policy staged.');
    expect(host.textContent).not.toContain('Routing policy published.');
  });

  it('does not apply a stale policy reload after the resource changed', async () => {
    client.setQueryData(policyKey('route-draft', 'route-a'), {
      policy: {},
      etag: 'pa1'
    });
    client.setQueryData(policyKey('route-draft', 'route-b'), {
      policy: { constraints: { ignore: ['vendor:b'] } },
      etag: 'pb1'
    });
    const refetch = Promise.withResolvers<{
      data: { policy: object; etag: string };
      response: Response;
    }>();
    render({ policy: true });
    await vi.waitFor(() => {
      flushSync();
      expect(policyField().value).toBe('{}');
    });
    // Malformed section input surfaces the reload affordance.
    policySection('defaults', 'route-a').value = '{';
    policySection('defaults', 'route-a').dispatchEvent(
      new Event('input', { bubbles: true })
    );
    flushSync();
    vi.spyOn(apiClient, 'GET').mockReturnValue(refetch.promise as never);
    button('Reload policy').click();
    openRoute('route-b');
    await vi.waitFor(() => {
      flushSync();
      expect(policyField().value).toContain('vendor:b');
    });
    refetch.resolve({
      data: {
        policy: { constraints: { only: ['vendor:remote'] } },
        etag: 'pa2'
      },
      response: okResponse()
    });
    await Promise.resolve();
    await Promise.resolve();
    flushSync();
    expect(policyField().value).toContain('vendor:b');
    expect(policyField().value).not.toContain('vendor:remote');
  });

  it('retires an in-flight policy save on scope change and on unmount', async () => {
    const onSaved = vi.fn();
    client.setQueryData(policyKey('route-draft', 'route-a'), {
      policy: {},
      etag: 'pa1'
    });
    client.setQueryData(policyKey('api-key', 'route-a'), {
      policy: {},
      etag: 'ka1'
    });
    const first = Promise.withResolvers<{
      data: { policy: object; etag: string };
      response: Response;
    }>();
    const second = Promise.withResolvers<{
      data: { policy: object; etag: string };
      response: Response;
    }>();
    vi.spyOn(apiClient, 'PUT')
      .mockReturnValueOnce(first.promise as never)
      .mockReturnValueOnce(second.promise as never);
    render({ policy: true, onPolicySaved: onSaved });
    await vi.waitFor(() => {
      flushSync();
      expect(policyField().value).toBe('{}');
    });
    submitPolicy();
    await vi.waitFor(() => expect(apiClient.PUT).toHaveBeenCalled());
    button('Scope api-key').click();
    flushSync();
    first.resolve({
      data: { policy: {}, etag: 'pa2' },
      response: okResponse()
    });
    await Promise.resolve();
    await Promise.resolve();
    flushSync();
    expect(onSaved).not.toHaveBeenCalled();
    expect(host.textContent).not.toContain('Routing policy published.');

    // The same guard applies when the component is destroyed outright.
    await vi.waitFor(() => {
      flushSync();
      expect(policyField().value).toBe('{}');
    });
    submitPolicy();
    await vi.waitFor(() =>
      expect(vi.mocked(apiClient.PUT).mock.calls.length).toBe(2)
    );
    button('Unmount editor').click();
    flushSync();
    second.resolve({
      data: { policy: {}, etag: 'ka2' },
      response: okResponse()
    });
    await Promise.resolve();
    await Promise.resolve();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it('keeps same-resource edits made during a policy save dirty', async () => {
    client.setQueryData(policyKey('route-draft', 'route-a'), {
      policy: {},
      etag: 'pa1'
    });
    const pending = Promise.withResolvers<{
      data: { policy: object; etag: string };
      response: Response;
    }>();
    vi.spyOn(apiClient, 'PUT').mockReturnValue(pending.promise as never);
    render({ policy: true });
    await vi.waitFor(() => {
      flushSync();
      expect(policyField().value).toBe('{}');
    });
    editPolicy('{"constraints":{"only":["vendor:first"]}}');
    submitPolicy();
    await vi.waitFor(() => expect(apiClient.PUT).toHaveBeenCalled());
    editPolicy('{"constraints":{"only":["vendor:second"]}}');
    // The refetch after save returns what the PUT persisted.
    client.setQueryData(policyKey('route-draft', 'route-a'), {
      policy: { constraints: { only: ['vendor:first'] } },
      etag: 'pa2'
    });
    pending.resolve({
      data: {
        policy: { constraints: { only: ['vendor:first'] } },
        etag: 'pa2'
      },
      response: okResponse()
    });
    await vi.waitFor(() => {
      flushSync();
      expect(flags()).toContain('busy:false');
    });
    expect(flags()).toContain('dirty:true');
    expect(policyField().value).toContain('vendor:second');
  });
});
