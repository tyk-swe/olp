import { afterEach, assert, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../../tests/unit/support/StateHarness.svelte';
import ProjectLimitTemplatesPanel from './ProjectLimitTemplatesPanel.svelte';
import ProjectRouteGroupsPanel from './ProjectRouteGroupsPanel.svelte';

afterEach(() => vi.unstubAllGlobals());

const projectId = '00000000-0000-4000-8000-000000000010';

function mountPanel(
  Panel: typeof ProjectRouteGroupsPanel | typeof ProjectLimitTemplatesPanel
) {
  const target = document.createElement('div');
  document.body.append(target);
  const setup = vi.fn(() =>
    mount(Panel, {
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
  return target;
}

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
  const target = mountPanel(ProjectRouteGroupsPanel);
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

it('saves edits to the remaining limit template with the loaded etag', async () => {
  const saved = vi.fn<(request: Request) => void>();
  const policy = {
    etag: 'loaded',
    templates: {
      retired: { requests_per_minute: 5 },
      standard: { daily_cost_limit: '2.50', cost_basis: 'charge' }
    }
  };
  serve((request) => {
    if (request.method === 'PUT') saved(request);
    return json(policy);
  });
  const target = mountPanel(ProjectLimitTemplatesPanel);
  const remove = await vi.waitUntil(() =>
    [...target.querySelectorAll('button')].find(
      (button) => button.textContent.trim() === 'Remove template'
    )
  );
  remove.click();
  flushSync();

  for (const [label, original, edited] of [
    ['Template name', 'standard', ' priority '],
    ['Daily budget (USD)', '2.50', '3.75']
  ]) {
    const field = [...target.querySelectorAll('label')].find(
      (item) => item.textContent.trim() === label
    )?.control;
    assert.instanceOf(field, HTMLInputElement);
    expect(field.value).toBe(original);
    field.value = edited;
    field.dispatchEvent(new Event('input', { bubbles: true }));
  }
  target.querySelector('form')?.requestSubmit();
  await vi.waitFor(() => expect(saved).toHaveBeenCalledOnce());
  const [request] = saved.mock.calls[0];
  expect(request.headers.get('If-Match')).toBe('"loaded"');
  expect(await request.json()).toEqual({
    templates: {
      priority: {
        requests_per_minute: null,
        tokens_per_minute: null,
        max_concurrency: null,
        daily_cost_limit: '3.75',
        weekly_cost_limit: null,
        monthly_cost_limit: null,
        cost_basis: 'charge'
      }
    }
  });
});
