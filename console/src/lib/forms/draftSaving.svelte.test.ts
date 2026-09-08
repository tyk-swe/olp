import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiProblem } from '$lib/api/http';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import { updateProvider } from '$lib/features/providers/api';
import { listProviderModelPage } from '$lib/features/providers/models';
import { getRouteDraft, replaceRouteDraft } from '$lib/features/routes/api';
import DraftEditorProbe from './test/DraftEditorProbe.svelte';
import { draft, provider, providerSpec } from './test/draftFixtures';

const navigation = vi.hoisted(() => ({ beforeNavigate: vi.fn() }));
vi.mock('$app/navigation', () => ({ ...navigation, goto: vi.fn() }));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));
vi.mock('$lib/features/providers/api', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/api')>()),
  updateProvider: vi.fn()
}));
vi.mock('$lib/features/providers/models', async (original) => ({
  ...(await original<typeof import('$lib/features/providers/models')>()),
  listProviderModelPage: vi.fn()
}));
vi.mock('$lib/features/routes/api', async (original) => ({
  ...(await original<typeof import('$lib/features/routes/api')>()),
  getRouteDraft: vi.fn(),
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
});

function render(kind: 'route' | 'provider') {
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
