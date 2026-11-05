import SinksPage from './SinksPage.svelte';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '$lib/management/test/transport';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import SinkEditor from './SinkEditor.svelte';
import type { Sink } from './api';

afterEach(() => vi.unstubAllGlobals());

it('preserves the signing credential when editing other fields and supports explicit removal', async () => {
  const writes = vi.fn<(body: unknown, etag: string | null) => void>();
  const saved = vi.fn();
  const current: Sink = {
    id: 'sink',
    etag: 'observed-version',
    name: 'Signed facts',
    type: 'https',
    project_id: null,
    destination: 'https://export.example.test/facts',
    streams: ['requests'],
    enabled: false,
    has_credential: true,
    retired_at: null,
    delivery: { delivered: 0, failed: 0, expired: 0, last_delivered_at: null }
  };
  serve(
    async (request) => {
      writes(await request.json(), request.headers.get('If-Match'));
      return json({ ...current, etag: 'saved-version' });
    },
    ['read', 'settings']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(SinkEditor, {
      target,
      props: { current, projects: [], saved },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  flushSync();
  const name = target.querySelectorAll('input')[0];
  name.value = 'Renamed facts';
  name.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(saved).toHaveBeenCalledOnce());
  expect(writes.mock.calls[0][0]).toMatchObject({ name: 'Renamed facts' });
  expect(writes.mock.calls[0][0]).not.toHaveProperty('credential');
  expect(writes.mock.calls[0][1]).toBe('"observed-version"');
  const remove = [...target.querySelectorAll('label')]
    .find((label) =>
      label.textContent
        ?.replace(/\s+/g, ' ')
        .includes('Remove signing credential')
    )
    ?.querySelector('input');
  expect(remove).toBeDefined();
  if (remove) {
    remove.checked = true;
    remove.dispatchEvent(new Event('change', { bubbles: true }));
  }
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(saved).toHaveBeenCalledTimes(2));
  expect(writes.mock.calls[1][0]).toHaveProperty('credential', null);
  expect(writes.mock.calls[1][1]).toBe('"saved-version"');
});

it.each(['refresh', 'conflict'])(
  'preserves export edits and offers reload after %s',
  async (trigger) => {
    const current: Sink = {
      id: 'sink',
      etag: 'before',
      name: 'Original',
      type: 'https',
      project_id: null,
      destination: 'https://export.example.test/facts',
      streams: ['requests'],
      enabled: false,
      has_credential: false,
      retired_at: null,
      delivery: { delivered: 0, failed: 0, expired: 0, last_delivered_at: null }
    };
    const load = vi.fn(() => json({ items: [current] }));
    const writes = vi.fn<(request: Request) => void>();
    serve(
      (request) => {
        if (request.method === 'PUT') {
          writes(request);
          return json(
            {
              title: 'Changed elsewhere',
              status: 412,
              type: 'https://openllmproxy.dev/problems/etag_mismatch'
            },
            412
          );
        }
        return new URL(request.url).pathname.endsWith('/projects')
          ? json({ items: [] })
          : load();
      },
      ['read', 'settings']
    );
    const target = document.createElement('div');
    const setup = vi.fn(() =>
      mount(SinksPage, { target, context: getAllContexts() })
    );
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    const button = (name: string) =>
      [...target.querySelectorAll('button')].find(
        (item) => item.textContent?.trim() === name
      );
    await vi.waitFor(() => expect(button('Edit export')).toBeDefined());
    button('Edit export')?.click();
    flushSync();
    const name = target.querySelector('form input') as HTMLInputElement;
    name.value = 'My draft';
    name.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    load.mockImplementation(() =>
      json({ items: [{ ...current, name: 'Other operator', etag: 'remote' }] })
    );
    if (trigger === 'refresh') {
      button('Refresh delivery counts')?.click();
      await vi.waitFor(() =>
        expect(target.querySelector('article h2')?.textContent).toBe(
          'Other operator'
        )
      );
    }
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'My draft'
    );
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(writes).toHaveBeenCalledOnce());
    expect(writes.mock.calls[0][0].headers.get('If-Match')).toBe('"before"');
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Changed elsewhere')
    );
    await vi.waitFor(() =>
      expect(button('Reload latest')?.disabled).toBe(false)
    );
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'My draft'
    );
    button('Reload latest')?.click();
    flushSync();
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'Other operator'
    );
  }
);
