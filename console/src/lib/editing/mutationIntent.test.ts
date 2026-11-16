import { expect, it, vi } from 'vitest';
import { MutationIntent } from './mutationIntent.js';

it('keeps its request identity when an unchanged submission is retried', async () => {
  const intent = new MutationIntent();
  const keys: string[] = [];
  const send = vi.fn(async (_input: { name: string }, requestId: string) => {
    keys.push(requestId);
    if (keys.length === 1) throw new Error('response lost');
    return 'saved';
  });
  await expect(intent.submit({ name: 'policy' }, send)).rejects.toThrow(
    'response lost'
  );
  await expect(intent.submit({ name: 'policy' }, send)).resolves.toBe('saved');
  expect(keys).toHaveLength(2);
  expect(keys[0]).toBeTruthy();
  expect(keys[1]).toBe(keys[0]);
});

it('mints a new request identity when the submission changes or completes', async () => {
  const intent = new MutationIntent();
  const keys: string[] = [];
  const send = vi.fn(async (_input: { name: string }, requestId: string) => {
    keys.push(requestId);
    return 'saved';
  });
  await intent.submit({ name: 'first' }, send);
  await intent.submit({ name: 'first' }, send);
  await intent.submit({ name: 'edited' }, send);
  expect(new Set(keys).size).toBe(3);
});

it('keeps the payload built for the pending submission, not a later rebuild', () => {
  const intent = new MutationIntent();
  let stamp = 0;
  const build = vi.fn(() => ({ stamp: ++stamp }));
  const first = intent.prepare({ name: 'policy' }, build);
  const second = intent.prepare({ name: 'policy' }, build);
  expect(build).toHaveBeenCalledOnce();
  expect(second).toEqual(first);
  const changed = intent.prepare({ name: 'edited' }, build);
  expect(changed.input).toEqual({ stamp: 2 });
  expect(changed.requestId).not.toBe(first.requestId);
});

it('drops the pending submission when reset', async () => {
  const intent = new MutationIntent();
  const keys: string[] = [];
  const failing = vi.fn(async (_input: { name: string }, requestId: string) => {
    keys.push(requestId);
    throw new Error('response lost');
  });
  await expect(intent.submit({ name: 'policy' }, failing)).rejects.toThrow(
    'response lost'
  );
  intent.reset();
  const send = vi.fn(async (_input: { name: string }, requestId: string) => {
    keys.push(requestId);
    return 'saved';
  });
  await intent.submit({ name: 'policy' }, send);
  expect(keys).toHaveLength(2);
  expect(keys[1]).not.toBe(keys[0]);
});
