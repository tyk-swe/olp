import { useAction, useMutation } from './action.svelte';
import { authLifecycle } from '#lib/session/lifecycle.js';
import type { AuthenticatedSession } from '#lib/session/state.js';
import { expect, it, vi } from 'vitest';
import { mountState } from '#test/mount.js';

it('replays an unanswered submission with its request ID but never reuses it after a completed send', async () => {
  const { value: mutation } = mountState(() => useMutation(), { attach: true });
  const requestIds: string[] = [];
  const lost = Promise.withResolvers<number>();
  const replay = mutation.submit('write', (_input, requestId) => {
    requestIds.push(requestId);
    return lost.promise;
  });
  lost.reject(new TypeError('network lost'));
  expect(await replay).toBe(false);

  const recovered = Promise.withResolvers<number>();
  const retried = mutation.submit('write', (_input, requestId) => {
    requestIds.push(requestId);
    return recovered.promise;
  });
  recovered.resolve(1);
  expect(await retried).toBe(true);
  expect(requestIds[0]).toBe(requestIds[1]);

  const settled = Promise.withResolvers<number>();
  const next = mutation.submit(
    'write',
    (_input, requestId) => {
      requestIds.push(requestId);
      return settled.promise;
    },
    () => {
      throw new Error('downstream failure');
    }
  );
  settled.resolve(2);
  expect(await next).toBe(false);

  const fresh = Promise.withResolvers<number>();
  const again = mutation.submit('write', (_input, requestId) => {
    requestIds.push(requestId);
    return fresh.promise;
  });
  fresh.resolve(3);
  expect(await again).toBe(true);
  expect(requestIds[3]).not.toBe(requestIds[2]);
  expect(requestIds[3]).not.toBe(requestIds[0]);
});

it('retires a pending send with the old principal so its late resolution neither accepts nor clears the new principal’s write', async () => {
  const retire: { current?: () => void } = {};
  vi.spyOn(authLifecycle, 'onPrincipalChange').mockImplementation(
    (listener) => {
      retire.current = listener;
      return () => {};
    }
  );
  const { value: action } = mountState(() => useAction(), { attach: true });
  const stale = Promise.withResolvers<string>();
  const staleAccept = vi.fn();
  const staleRun = action.run(() => stale.promise, staleAccept);
  expect(action.busy).toBe(true);

  retire.current?.();
  expect(action.busy).toBe(false);

  const fresh = Promise.withResolvers<string>();
  const freshAccept = vi.fn();
  const freshRun = action.run(() => fresh.promise, freshAccept);
  expect(action.busy).toBe(true);

  stale.resolve('late');
  expect(await staleRun).toBe(false);
  expect(staleAccept).not.toHaveBeenCalled();
  expect(action.busy).toBe(true);

  fresh.resolve('done');
  expect(await freshRun).toBe(true);
  expect(freshAccept).toHaveBeenCalledOnce();
  expect(action.busy).toBe(false);
});

it('keeps an authentication-scoped action busy through its own transition and stops accepting after unmount', async () => {
  const mounted = mountState(() => useAction(undefined, 'authentication'), {
    attach: true
  });
  const action = mounted.value;
  const session: AuthenticatedSession = {
    user: {
      id: 'owner',
      email: 'owner@example.test',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: []
    },
    csrf_token: 'token'
  };
  const first = Promise.withResolvers<string>();
  const accept = vi.fn();
  const firstRun = action.run(() => first.promise, accept);
  expect(action.busy).toBe(true);

  authLifecycle.establishSession(session);
  expect(action.busy).toBe(true);

  first.resolve('signed in');
  expect(await firstRun).toBe(true);
  expect(accept).toHaveBeenCalledOnce();
  expect(action.busy).toBe(false);

  const second = Promise.withResolvers<string>();
  const secondAccept = vi.fn();
  const secondRun = action.run(() => second.promise, secondAccept);
  await mounted.unmount();
  second.resolve('too late');
  expect(await secondRun).toBe(false);
  expect(secondAccept).not.toHaveBeenCalled();
});
