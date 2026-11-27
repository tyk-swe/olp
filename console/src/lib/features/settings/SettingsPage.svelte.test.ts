import { expect, it, onTestFinished, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { mountWithState, setFieldValue } from '#test/mount.js';
import SettingsPage from './SettingsPage.svelte';

const setting = {
  key: 'retention.usage_days',
  value: '30',
  etag: 'etag-1',
  updated_at: '2026-01-01T00:00:00Z',
  updated_by: 'owner'
};

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
  onTestFinished(() => release.resolve());
  const { target } = mountWithState(SettingsPage, {
    view: 'retention' as const
  });
  const input = () => {
    const found = target.querySelector('input');
    if (!found) throw new Error('Missing setting input');
    return found;
  };
  const edit = (value: string) => setFieldValue(input(), value);
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
  await eventually(() => expect(input().value).toBe('30'));
  edit('45');
  save();
  await pending.promise;
  edit('60');
  release.resolve();
  await eventually(() => expect(saveButton()?.disabled).toBe(false));
  expect(input().value).toBe('60');
  save();
  await eventually(() => expect(writes).toHaveBeenCalledTimes(2));
  const [request, body] = writes.mock.calls[1];
  expect(request.headers.get('If-Match')).toBe('"etag-2"');
  expect(body.value).toBe('60');
  await eventually(() => expect(saveButton()).toBeUndefined());
});
