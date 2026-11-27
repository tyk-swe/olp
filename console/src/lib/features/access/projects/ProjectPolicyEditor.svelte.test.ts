import { assert, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  clickButton as clickTargetButton,
  findButton as findTargetButton,
  mountWithState
} from '#test/mount.js';
import ProjectLimitTemplatesPanel from './ProjectLimitTemplatesPanel.svelte';
import ProjectRouteGroupsPanel from './ProjectRouteGroupsPanel.svelte';

const projectId = '00000000-0000-4000-8000-000000000010';

function mountPanel(
  Panel: typeof ProjectRouteGroupsPanel | typeof ProjectLimitTemplatesPanel
) {
  return mountWithState(Panel, { projectId }, { attach: true }).target;
}

it('keeps a conflicting edit until reload and saves with the refreshed etag', async () => {
  const initial = { groups: { chat: ['alpha'] }, etag: 'loaded' };
  const refreshed = { groups: { chat: ['alpha', 'gamma'] }, etag: 'refreshed' };
  const saved = { groups: { chat: ['alpha', 'gamma', 'beta'] }, etag: 'saved' };
  const final = { groups: {}, etag: 'final' };
  const revisions = [initial, refreshed, saved, final];
  const conflict = 'The saved policy changed. Reload it before saving.';
  const write = vi
    .fn<(request: Request) => Response>(() => json(final))
    .mockReturnValueOnce(
      json(
        {
          type: 'https://openllmproxy.dev/problems/etag_mismatch',
          title: 'Precondition Failed',
          status: 412,
          detail: conflict
        },
        412
      )
    )
    .mockReturnValueOnce(json(saved));
  serve((request) =>
    request.method === 'PUT'
      ? write(request)
      : json(revisions[write.mock.calls.length] ?? final)
  );

  const target = mountPanel(ProjectRouteGroupsPanel);
  const memberRoutes = () => {
    const field = [...target.querySelectorAll('label')].find(
      (label) => label.textContent.trim() === 'Member routes'
    )?.control;
    assert.instanceOf(field, HTMLTextAreaElement);
    return field;
  };
  const findButton = (label: string) => findTargetButton(target, label);
  const clickButton = (label: string) => clickTargetButton(target, label);
  const saveLabel = 'Save route groups';
  const alert = () => target.querySelector('[role="alert"]');
  const waitForSuccessfulSave = () =>
    eventually(() => {
      expect(findButton(saveLabel)?.disabled).toBe(false);
      expect(alert()).toBeNull();
    });
  const editAndSave = (routes: string) => {
    const field = memberRoutes();
    flushSync(() => {
      field.value = routes;
      field.dispatchEvent(new Event('input', { bubbles: true }));
    });
    clickButton(saveLabel);
  };

  await eventually(() => expect(memberRoutes().value).toBe('alpha'));
  editAndSave('beta');
  await eventually(() => expect(alert()?.textContent).toBe(conflict));
  expect(memberRoutes().value).toBe('beta');

  clickButton('Reload saved policy');
  await eventually(() => expect(memberRoutes().value).toBe('alpha, gamma'));
  expect(alert()).toBeNull();

  editAndSave('alpha gamma beta');
  await waitForSuccessfulSave();
  expect(memberRoutes().value).toBe('alpha, gamma, beta');
  clickButton('Remove group chat');
  clickButton(saveLabel);
  await waitForSuccessfulSave();

  const submitted = await Promise.all(
    write.mock.calls.map(async ([request]) => [
      request.headers.get('If-Match'),
      await request.json()
    ])
  );
  expect(submitted).toEqual([
    ['"loaded"', { groups: { chat: ['beta'] } }],
    ['"refreshed"', { groups: saved.groups }],
    ['"saved"', { groups: final.groups }]
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
  const remove = await eventually(() => {
    const found = findTargetButton(target, 'Remove template');
    expect(found).toBeDefined();
    return found!;
  });
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
  await eventually(() => expect(saved).toHaveBeenCalledOnce());
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
