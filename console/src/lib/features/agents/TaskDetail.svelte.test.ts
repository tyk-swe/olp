import { SvelteMap } from 'svelte/reactivity';
import { expect, it, onTestFinished, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { findButton, mountWithState } from '#test/mount.js';
import TaskDetailPage from './TaskDetailPage.svelte';

it('clears remote task state on navigation and ignores the previous task response', async () => {
  const params = new SvelteMap([['taskId', 'first']]);
  vi.doMock('$app/state', () => ({
    page: {
      get params() {
        return Object.fromEntries(params);
      }
    }
  }));
  onTestFinished(() => vi.doUnmock('$app/state'));
  const { default: TaskRoute } =
    await import('../../../routes/(console)/activity/tasks/[taskId]/+page.svelte');
  const pending = Promise.withResolvers<Response>();
  const remote = vi
    .fn()
    .mockResolvedValueOnce(json({ id: 'first', status: { state: 'working' } }))
    .mockReturnValueOnce(pending.promise);
  serve(
    (request) =>
      new URL(request.url).pathname.endsWith('/remote')
        ? remote()
        : json({
            state: 'submitted',
            remote_fees: [],
            callback_models: [],
            callback_tools: []
          }),
    ['read', 'configure']
  );
  const { target } = mountWithState(TaskRoute);
  const fetchRemote = () => findButton(target, 'Fetch remote status');
  await eventually(() => expect(fetchRemote()).toBeDefined());
  fetchRemote()?.click();
  await eventually(() =>
    expect(target.textContent).toContain('Remote state working')
  );
  fetchRemote()?.click();
  await eventually(() => expect(remote).toHaveBeenCalledTimes(2));
  params.set('taskId', 'second');
  flushSync();
  await eventually(() => expect(target.textContent).toContain('second'));
  expect(target.textContent).not.toContain('Remote state working');
  const lateResponse = json({ id: 'first', status: { state: 'completed' } });
  pending.resolve(lateResponse);
  await eventually(() => expect(lateResponse.bodyUsed).toBe(true));
  flushSync();
  expect(target.textContent).not.toContain('Remote state completed');
  expect(fetchRemote()?.disabled).toBe(false);
});

it('serializes remote status fetching and cancellation', async () => {
  const status = Promise.withResolvers<Response>();
  const cancelled = Promise.withResolvers<Response>();
  const reads = vi.fn(() => status.promise);
  const cancels = vi.fn(() => cancelled.promise);
  serve(
    (request) => {
      const path = new URL(request.url).pathname;
      if (path.endsWith('/remote')) return reads();
      if (path.endsWith('/cancel')) return cancels();
      return json({
        state: 'submitted',
        remote_fees: [],
        callback_models: [],
        callback_tools: []
      });
    },
    ['read', 'configure']
  );
  onTestFinished(() => {
    status.resolve(json({ id: 'task', status: { state: 'working' } }));
    cancelled.resolve(json({ id: 'task', status: { state: 'canceled' } }));
  });
  const { target } = mountWithState(TaskDetailPage, { taskID: 'task' });
  const button = (label: string) => findButton(target, label);
  await eventually(() => expect(button('Fetch remote status')).toBeDefined());
  button('Cancel remote task')?.click();
  flushSync();
  button('Fetch remote status')?.click();
  await eventually(() => expect(reads).toHaveBeenCalledOnce());
  expect(button('Confirm cancel remote task')?.disabled).toBe(true);
  button('Confirm cancel remote task')?.click();
  expect(cancels).not.toHaveBeenCalled();
  status.resolve(json({ id: 'task', status: { state: 'working' } }));
  await eventually(() =>
    expect(button('Confirm cancel remote task')?.disabled).toBe(false)
  );
  button('Confirm cancel remote task')?.click();
  await eventually(() => expect(cancels).toHaveBeenCalledOnce());
  expect(button('Fetch remote status')?.disabled).toBe(true);
  button('Fetch remote status')?.click();
  expect(reads).toHaveBeenCalledOnce();
  cancelled.resolve(json({ id: 'task', status: { state: 'canceled' } }));
  await eventually(() =>
    expect(target.textContent).toContain('Remote state canceled')
  );
  await eventually(() =>
    expect(button('Fetch remote status')?.disabled).toBe(false)
  );
});
