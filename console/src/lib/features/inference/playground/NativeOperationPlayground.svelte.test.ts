import { flushSync, mount, unmount } from 'svelte';
import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import NativeOperationPlayground from './NativeOperationPlayground.svelte';
import { eventually } from '../../../../../tests/unit/support/eventually.js';

afterEach(() => vi.unstubAllGlobals());

type Delivery = (path: string, init: RequestInit) => Promise<Response>;

function playground(deliver: Delivery) {
  const send = vi.fn(deliver);
  serve(send as unknown as (request: Request) => Promise<Response>);
  const target = document.createElement('div');
  document.body.append(target);
  const component = mount(NativeOperationPlayground, {
    target,
    props: {
      route: 'sample',
      operation: 'rerank',
      requestText: '{"query":"green","documents":["square"]}'
    }
  });
  onTestFinished(async () => {
    await unmount(component);
    target.remove();
  });
  flushSync();
  const key = target.querySelector<HTMLInputElement>('input[type=password]')!;
  key.value = 'olp_fixture';
  key.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  const button = (label: string) =>
    [...target.querySelectorAll('button')].find((candidate) =>
      candidate.textContent.includes(label)
    );
  return { target, send, button };
}

const alert = (target: HTMLElement) =>
  target.querySelector('[role=alert]')?.textContent ?? '';

it('runs one public operation at a time, shows its result and clears it', async () => {
  const answers: ((response: Response) => void)[] = [];
  const { target, send, button } = playground(
    () => new Promise<Response>((resolve) => answers.push(resolve))
  );

  button('Run billable operation')!.click();
  flushSync();
  const running = button('Running public operation')!;
  expect(running.disabled).toBe(true);
  running.click();
  await eventually(() => expect(send).toHaveBeenCalledTimes(1));
  expect(send.mock.calls[0]![0]).toBe('/native/rerank/models/sample');
  expect(new Headers(send.mock.calls[0]![1].headers).get('Authorization')).toBe(
    'Bearer olp_fixture'
  );

  answers[0]!(json({ results: [] }));
  await eventually(() =>
    expect(target.textContent).toContain('Operation result · rerank')
  );
  expect(button('Run billable operation')!.disabled).toBe(false);
  expect(alert(target)).toBe('');

  button('Clear result')!.click();
  flushSync();
  expect(target.textContent).not.toContain('Operation result');
  expect(button('Clear result')).toBeUndefined();
});

it('says why an operation was refused and replaces the last outcome on the next run', async () => {
  const replies = [json({}, 403), json({ results: [] })];
  const { target, button } = playground(async () => replies.shift()!);

  button('Run billable operation')!.click();
  await eventually(() => expect(alert(target)).toContain('not admitted (403)'));
  expect(target.textContent).not.toContain('Operation result');
  expect(button('Run billable operation')!.disabled).toBe(false);

  button('Run billable operation')!.click();
  await eventually(() =>
    expect(target.textContent).toContain('Operation result · rerank')
  );
  expect(alert(target)).toBe('');
});

it('stops a delivery on request, says so and allows another run', async () => {
  const { target, button } = playground(
    (_, init) =>
      new Promise<Response>((_, reject) =>
        init.signal!.addEventListener('abort', () =>
          reject(new Error('Delivery stopped.'))
        )
      )
  );

  button('Run billable operation')!.click();
  flushSync();
  button('Cancel delivery')!.click();

  await eventually(() => expect(alert(target)).toBe('Delivery stopped.'));
  expect(button('Cancel delivery')).toBeUndefined();
  expect(button('Run billable operation')!.disabled).toBe(false);
  expect(target.textContent).not.toContain('Operation result');
});
