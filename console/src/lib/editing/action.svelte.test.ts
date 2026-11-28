import { useAction, useMutation } from './action.svelte';
import { formProblem, validated } from './validation';
import { ApiProblem } from '#lib/management/http.js';
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
  const { value: action } = mountState(() => useAction<'name'>(), {
    attach: true
  });
  const stale = Promise.withResolvers<string>();
  const staleAccept = vi.fn();
  const staleRun = action.run(() => stale.promise, staleAccept);
  expect(action.busy).toBe(true);

  action.check(validated({ name: 'Enter a name.' }, () => null));
  retire.current?.();
  expect(action.busy).toBe(false);
  expect(action.fieldErrors).toEqual({});

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

function refusal(messages: Record<string, string>) {
  return new ApiProblem({
    title: 'Validation failed',
    status: 422,
    detail: 'The request was refused.',
    errors: Object.fromEntries(
      Object.entries(messages).map(([field, message]) => [
        field,
        [{ code: 'validation_failed', message }]
      ])
    )
  });
}

it('reports a client message that no field can show until the next check', () => {
  const { value: action } = mountState(() => useAction<'name'>(), {
    attach: true
  });

  expect(action.check(formProblem('Set a name or a template.'))).toBe(false);
  expect(action.error).toBe('Set a name or a template.');
  expect(action.fieldErrors).toEqual({});

  action.check(validated({ name: 'Enter a name.' }, () => null));
  expect(action.error).toBe('');
});

it('dismisses a reported failure without giving up the retry of the same write', async () => {
  const { value: mutation } = mountState(() => useMutation<'name'>(), {
    attach: true
  });
  const requestIds: string[] = [];
  const lose = () =>
    mutation.submit(
      'write',
      (_input, requestId) => {
        requestIds.push(requestId);
        return Promise.reject(refusal({ display_name: 'Taken.' }));
      },
      undefined,
      { serverFields: { display_name: 'name' } }
    );

  await lose();
  mutation.dismiss();
  expect(mutation.fieldErrors).toEqual({});
  await lose();
  mutation.reset();
  await lose();
  expect(requestIds[1]).toBe(requestIds[0]);
  expect(requestIds[2]).not.toBe(requestIds[1]);

  mutation.check(formProblem('Choose something.'));
  mutation.dismiss();
  expect(mutation.error).toBe('');
});

it('reports client and server field errors in one place and keeps a message for what no field shows', async () => {
  const { value: action } = mountState(() => useAction<'name'>(), {
    attach: true
  });
  const serverFields = { display_name: 'name' } as const;
  const refuse = (cause: Error) =>
    action.run(() => Promise.reject(cause), undefined, { serverFields });

  await refuse(refusal({ display_name: 'That name is taken.' }));
  expect(action.fieldErrors).toEqual({ name: 'That name is taken.' });
  expect(action.error).toBe('');

  await refuse(refusal({ display_name: 'Taken.', quota: 'Full.' }));
  expect(action.fieldErrors).toEqual({ name: 'Taken.' });
  expect(action.error).toBe('The request was refused.');

  const checked = validated({ name: 'Enter a name.' }, () => 'unused');
  expect(action.check(checked)).toBe(false);
  expect(action.fieldErrors).toEqual({ name: 'Enter a name.' });
  expect(action.error).toBe('');

  await refuse(new TypeError('network lost'));
  expect(action.fieldErrors).toEqual({});
  expect(action.error).toBe('network lost');

  expect(action.check(validated({ name: undefined }, () => 'typed'))).toBe(
    true
  );
  expect(action.error).toBe('');

  await action.run(
    () => Promise.resolve(),
    () => {
      throw refusal({ display_name: 'From a later request.' });
    },
    { serverFields }
  );
  expect(action.fieldErrors).toEqual({});
  expect(action.error).toBe('The request was refused.');
});
