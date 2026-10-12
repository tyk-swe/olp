import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { listRoutes } from '$lib/features/routes/api';
import { provider as baseProvider } from '$lib/forms/test/draftFixtures';
import { ApiProblem } from '$lib/api/http';
import {
  activateProvider,
  disableProvider,
  restoreProviderAsDraft,
  type ProviderSummary
} from './api/providers';
import ProviderBulkActionsProbe from './test/ProviderBulkActionsProbe.svelte';

vi.mock('$lib/features/routes/api', () => ({ listRoutes: vi.fn() }));
vi.mock('./api/providers', () => ({
  activateProvider: vi.fn(),
  disableProvider: vi.fn(),
  restoreProviderAsDraft: vi.fn()
}));
let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;
const selected = [
  { id: 'provider-one', etag: 'reviewed-one', name: 'One' },
  { id: 'provider-two', etag: 'reviewed-two', name: 'Two' }
] as ProviderSummary[];
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
  vi.mocked(listRoutes).mockResolvedValue([]);
  vi.mocked(disableProvider).mockResolvedValue(1);
  vi.mocked(activateProvider).mockResolvedValue(1);
  vi.mocked(restoreProviderAsDraft).mockImplementation(async (input) => ({
    ...baseProvider,
    id: input.id
  }));
});
afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  await authLifecycle.principalInvalidated();
  client.clear();
  host.remove();
});
function button(label: string) {
  const found = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (button) => button.textContent?.trim() === label
  );
  if (!found) throw Error(`Missing ${label}`);
  return found;
}
function click(label: string) {
  button(label).click();
  flushSync();
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}
async function renderReview(onChanged = vi.fn(async () => {})) {
  const mounted = mount(ProviderBulkActionsProbe, {
    target: host,
    props: { client, selected, onChanged }
  });
  component = mounted;
  flushSync();
  click('Disable selected');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('No published routes reference')
  );
  return mounted;
}
it.each(['unmount', 'principal', 'selection'] as const)(
  'never changes the next reviewed provider after %s retirement',
  async (boundary) => {
    const response = deferred<number>();
    vi.mocked(disableProvider).mockReturnValueOnce(response.promise);
    const onChanged = vi.fn(async () => {});
    const mounted = await renderReview(onChanged);
    click('Apply disable to reviewed connections');
    await vi.waitFor(() => expect(disableProvider).toHaveBeenCalledTimes(1));
    if (boundary === 'unmount') {
      await unmount(mounted);
      component = undefined;
    } else if (boundary === 'principal')
      await authLifecycle.principalInvalidated();
    else {
      mounted.replaceSelection([selected[1]]);
      flushSync();
    }
    response.resolve(1);
    await response.promise;
    await Promise.resolve();
    flushSync();
    expect(disableProvider).toHaveBeenCalledTimes(1);
    expect(onChanged).not.toHaveBeenCalled();
  }
);
it('refreshes confirmed changes without resubmitting any reviewed resource', async () => {
  const onChanged = vi
    .fn(async () => {})
    .mockRejectedValueOnce(new Error('refresh unavailable'));
  await renderReview(onChanged);
  click('Apply disable to reviewed connections');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Refreshing failed: refresh unavailable')
  );
  expect(disableProvider).toHaveBeenCalledTimes(2);
  expect(button('Apply disable to reviewed connections').disabled).toBe(true);
  click('Retry refresh');
  await vi.waitFor(() => expect(onChanged).toHaveBeenCalledTimes(2));
  await vi.waitFor(() =>
    expect(host.textContent).not.toContain('Retry refresh')
  );
  expect(disableProvider).toHaveBeenCalledTimes(2);
});
it('replays only the unresolved resource with its original key and reviewed ETag', async () => {
  vi.mocked(disableProvider).mockImplementation(async (provider) => {
    if (
      provider.id === 'provider-two' &&
      vi.mocked(disableProvider).mock.calls.length === 2
    )
      throw new TypeError('response lost');
    return 1;
  });
  const mounted = await renderReview();
  click('Apply disable to reviewed connections');
  await vi.waitFor(() => expect(host.textContent).toContain('response lost'));
  await vi.waitFor(() =>
    expect(button('Apply disable to reviewed connections').disabled).toBe(false)
  );
  const original = vi.mocked(disableProvider).mock.calls[1];
  mounted.replaceSelection(
    selected.map((provider) => ({
      ...provider,
      etag: 'refreshed-after-commit'
    }))
  );
  flushSync();
  click('Apply disable to reviewed connections');
  await vi.waitFor(() => expect(disableProvider).toHaveBeenCalledTimes(3));
  expect(vi.mocked(disableProvider).mock.calls[2]).toEqual(original);
  expect(vi.mocked(disableProvider).mock.calls[0][0]).toEqual({
    id: 'provider-one',
    etag: 'reviewed-one'
  });
  expect(original[0]).toEqual({ id: 'provider-two', etag: 'reviewed-two' });
});

it('adopts a newly reviewed ETag only for a definitely refused target', async () => {
  vi.mocked(disableProvider)
    .mockRejectedValueOnce(
      new ApiProblem({ status: 412, title: 'Review changed connection' })
    )
    .mockResolvedValue(1);
  const mounted = await renderReview();
  click('Apply disable to reviewed connections');
  await vi.waitFor(() =>
    expect(button('Apply disable to reviewed connections').disabled).toBe(false)
  );
  expect(disableProvider).toHaveBeenCalledTimes(2);
  const rejected = vi.mocked(disableProvider).mock.calls[0];
  mounted.replaceSelection(
    selected.map((provider) => ({ ...provider, etag: 'reviewed-new' }))
  );
  flushSync();
  click('Disable selected');
  click('Apply disable to reviewed connections');
  await vi.waitFor(() => expect(disableProvider).toHaveBeenCalledTimes(3));
  expect(vi.mocked(disableProvider).mock.calls[2][0]).toEqual({
    id: 'provider-one',
    etag: 'reviewed-new'
  });
  expect(vi.mocked(disableProvider).mock.calls[2][1]).not.toBe(rejected[1]);
});

it('keeps earlier ambiguity sticky when a later retry is definitely refused', async () => {
  vi.mocked(disableProvider)
    .mockResolvedValueOnce(1)
    .mockRejectedValueOnce(new TypeError('response lost'))
    .mockRejectedValueOnce(
      new ApiProblem({ status: 412, title: 'Review changed connection' })
    )
    .mockResolvedValue(1);
  const mounted = await renderReview();
  click('Apply disable to reviewed connections');
  await vi.waitFor(() =>
    expect(button('Apply disable to reviewed connections').disabled).toBe(false)
  );
  const original = vi.mocked(disableProvider).mock.calls[1];
  click('Apply disable to reviewed connections');
  await vi.waitFor(() => expect(disableProvider).toHaveBeenCalledTimes(3));
  await vi.waitFor(() =>
    expect(button('Apply disable to reviewed connections').disabled).toBe(false)
  );
  mounted.replaceSelection(
    selected.map((provider) => ({ ...provider, etag: 'refreshed' }))
  );
  flushSync();
  click('Disable selected');
  click('Apply disable to reviewed connections');
  await vi.waitFor(() => expect(disableProvider).toHaveBeenCalledTimes(4));
  expect(vi.mocked(disableProvider).mock.calls[2]).toEqual(original);
  expect(vi.mocked(disableProvider).mock.calls[3]).toEqual(original);
});
