import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import SettingsPage from './SettingsPage.svelte';

const setting = {
  key: 'retention.usage_days',
  value: '30',
  etag: 'etag-1',
  updated_at: '2026-01-01T00:00:00Z',
  updated_by: 'owner'
};

afterEach(() => vi.unstubAllGlobals());

it('rebases a draft edited during a save onto the returned etag', async () => {
  const pending = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const writes = vi.fn<(request: Request, body: { value: string }) => void>();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/auth/capabilities')
        return json({ local_login_enabled: true, oidc_login_enabled: false });
      if (request.method === 'PUT') {
        const body = (await request.json()) as { value: string };
        writes(request, body);
        if (writes.mock.calls.length === 1) {
          pending.resolve();
          await release.promise;
          return json({ ...setting, value: body.value, etag: 'etag-2' });
        }
        return json({ ...setting, value: body.value, etag: 'etag-3' });
      }
      return json({ items: [setting] });
    },
    ['read', 'settings']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(SettingsPage, {
      target,
      props: { view: 'retention' as const },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    release.resolve();
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  const input = () => {
    const found = target.querySelector('input');
    if (!found) throw new Error('Missing setting input');
    return found;
  };
  const edit = (value: string) => {
    input().value = value;
    input().dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
  };
  const save = () => {
    const form = target.querySelector('form');
    if (!form) throw new Error('Missing setting form');
    form.dispatchEvent(
      new SubmitEvent('submit', { bubbles: true, cancelable: true })
    );
    flushSync();
  };
  const saveButton = () =>
    [...target.querySelectorAll('button')].find((item) =>
      ['Save', 'Saving…'].includes(item.textContent?.trim() ?? '')
    );
  flushSync();
  await vi.waitFor(() => expect(input().value).toBe('30'));
  edit('45');
  save();
  await pending.promise;
  edit('60');
  release.resolve();
  await vi.waitFor(() => expect(saveButton()?.disabled).toBe(false));
  expect(input().value).toBe('60');
  save();
  await vi.waitFor(() => expect(writes).toHaveBeenCalledTimes(2));
  const [request, body] = writes.mock.calls[1];
  expect(request.headers.get('If-Match')).toBe('"etag-2"');
  expect(body.value).toBe('60');
  await vi.waitFor(() => expect(saveButton()).toBeUndefined());
});
