import { expect, it } from 'vitest';
import { MutationIntent, MutationIntents } from './mutationIntent';
import { parseNativeJSON, stringifyNativeJSON } from '$lib/json/nativeJson';

it('replays unchanged intent with its exact constructed payload', () => {
  const owner = new MutationIntent();
  const first = owner.prepare({ days: 7 }, () => ({ expiresAt: 'first' }));
  const retry = owner.prepare({ days: 7 }, () => ({ expiresAt: 'later' }));
  expect(retry).toEqual(first);
});

it('starts a new identity after input changes or a known commit', () => {
  const owner = new MutationIntent();
  const first = owner.prepare({ name: 'first' });
  const changed = owner.prepare({ name: 'second' });
  expect(changed.requestId).not.toBe(first.requestId);
  owner.reset();
  expect(owner.prepare({ name: 'second' }).requestId).not.toBe(
    changed.requestId
  );
});

it('owns its submitted arrays independently of later draft mutation', () => {
  const owner = new MutationIntent();
  const values = { scopes: ['read'] };
  const first = owner.prepare(values);
  values.scopes.push('configure');
  expect(first.input.scopes).toEqual(['read']);
  expect(owner.prepare(values).requestId).not.toBe(first.requestId);
});

it('retains exact native numbers and member ordering in its owned submission', () => {
  const source = '{"1":123456789012345678901,"0":-0,"val":1e3}';
  const owner = new MutationIntent();
  const first = owner.prepare({ configuration: parseNativeJSON(source) });
  expect(stringifyNativeJSON(first.input.configuration)).toBe(source);
  expect(
    owner.prepare({ configuration: parseNativeJSON(source) }).requestId
  ).toBe(first.requestId);
});

it('keeps simultaneous failed bulk writes independently retryable', async () => {
  const owners = new MutationIntents();
  const receipts = new Map<string, string>();
  const fail = async (input: { id: string }, requestId: string) => {
    receipts.set(input.id, requestId);
    throw new TypeError('response lost');
  };
  await Promise.allSettled([
    owners.submit('first', { id: 'first' }, fail),
    owners.submit('second', { id: 'second' }, fail)
  ]);
  for (const id of ['first', 'second']) {
    await owners.submit(id, { id }, async (_input, requestId) => {
      expect(requestId).toBe(receipts.get(id));
    });
  }
});

it('does not let an older write success clear a newer submission', async () => {
  const owners = new MutationIntents();
  let finish!: () => void;
  const older = owners.submit('resource', { value: 'old' }, async () => {
    await new Promise<void>((resolve) => {
      finish = resolve;
    });
  });
  let newerId = '';
  await expect(
    owners.submit('resource', { value: 'new' }, async (_input, requestId) => {
      newerId = requestId;
      throw new TypeError('response lost');
    })
  ).rejects.toThrow('response lost');
  finish();
  await older;
  await owners.submit(
    'resource',
    { value: 'new' },
    async (_input, requestId) => {
      expect(requestId).toBe(newerId);
    }
  );
});
