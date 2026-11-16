import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import ProjectRouteGroupsPanel from './ProjectRouteGroupsPanel.svelte';

afterEach(() => vi.unstubAllGlobals());

const projectId = '00000000-0000-4000-8000-000000000010';

it('saves edited fields against the latest saved etag', async () => {
  const saves = vi.fn<(ifMatch: string | null, body: unknown) => void>();
  let stored: unknown = { groups: { chat: ['alpha'] }, etag: 'loaded' };
  serve(async (request) => {
    if (request.method === 'PUT') {
      const body = await request.json();
      saves(request.headers.get('If-Match'), body);
      stored = { ...body, etag: `saved-${saves.mock.calls.length}` };
    }
    return json(stored);
  });
  const target = document.createElement('div');
  document.body.append(target);
  const setup = vi.fn(() =>
    mount(ProjectRouteGroupsPanel, {
      target,
      props: { projectId },
      context: getAllContexts()
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
    target.remove();
  });
  const save = async (count: number) => {
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { cancelable: true }));
    await vi.waitFor(() => {
      expect(saves).toHaveBeenCalledTimes(count);
      expect(target.querySelector('button[type="submit"]')?.textContent).toBe(
        'Save route groups'
      );
    });
  };

  const remove = await vi.waitFor(() => {
    const button = [...target.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === 'Remove group chat'
    );
    expect(button).toBeDefined();
    return button as HTMLButtonElement;
  });
  remove.click();
  flushSync();

  await save(1);
  await save(2);

  expect(saves.mock.calls).toEqual([
    ['"loaded"', { groups: {} }],
    ['"saved-1"', { groups: {} }]
  ]);
});
