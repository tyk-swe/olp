import { useAction, useMutation } from './action.svelte';
import { ApiProblem } from '#lib/management/http.js';
import { authLifecycle } from '#lib/session/lifecycle.js';
import type { AuthenticatedSession } from '#lib/session/state.js';
import { expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import StateHarness from '../../../tests/unit/support/StateHarness.svelte';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const etagMismatch = () =>
  new ApiProblem({
    type: 'https://openllmproxy.dev/problems/etag_mismatch',
    title: 'Precondition failed',
    status: 412
  });

function harness(setup: () => void) {
  return mount(StateHarness, { target: document.body, props: { setup } });
}

it('replays an unanswered submission with its request ID but never reuses it after a completed send', async () => {
  let mutation!: ReturnType<typeof useMutation>;
  const component = harness(() => {
    mutation = useMutation();
  });
  const requestIds: string[] = [];
  const lost = deferred<number>();
  const replay = mutation.submit('write', (_input, requestId) => {
    requestIds.push(requestId);
    return lost.promise;
  });
  lost.reject(new TypeError('network lost'));
  expect(await replay).toBe(false);

  const recovered = deferred<number>();
  const retried = mutation.submit('write', (_input, requestId) => {
    requestIds.push(requestId);
    return recovered.promise;
  });
  recovered.resolve(1);
  expect(await retried).toBe(true);
  expect(requestIds[0]).toBe(requestIds[1]);

  const settled = deferred<number>();
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

  const fresh = deferred<number>();
  const again = mutation.submit('write', (_input, requestId) => {
    requestIds.push(requestId);
    return fresh.promise;
  });
  fresh.resolve(3);
  expect(await again).toBe(true);
  expect(requestIds[3]).not.toBe(requestIds[2]);
  expect(requestIds[3]).not.toBe(requestIds[0]);
  await unmount(component);
});

it('retires a pending send with the old principal so its late resolution neither accepts nor clears the new principal’s write', async () => {
  let action!: ReturnType<typeof useAction>;
  const retire: { current?: () => void } = {};
  vi.spyOn(authLifecycle, 'onPrincipalChange').mockImplementation(
    (listener) => {
      retire.current = listener;
      return () => {};
    }
  );
  const component = harness(() => {
    action = useAction();
  });
  const stale = deferred<string>();
  const staleAccept = vi.fn();
  const staleRun = action.run(() => stale.promise, staleAccept);
  expect(action.busy).toBe(true);

  retire.current?.();
  expect(action.busy).toBe(false);

  const fresh = deferred<string>();
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
  await unmount(component);
});

it('keeps an authentication-scoped action busy through its own transition and stops accepting after unmount', async () => {
  let action!: ReturnType<typeof useAction>;
  const component = harness(() => {
    action = useAction({ lifetime: 'authentication' });
  });
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
  const first = deferred<string>();
  const accept = vi.fn();
  const firstRun = action.run(() => first.promise, accept);
  expect(action.busy).toBe(true);

  authLifecycle.establishSession(session);
  expect(action.busy).toBe(true);

  first.resolve('signed in');
  expect(await firstRun).toBe(true);
  expect(accept).toHaveBeenCalledOnce();
  expect(action.busy).toBe(false);

  const second = deferred<string>();
  const secondAccept = vi.fn();
  const secondRun = action.run(() => second.promise, secondAccept);
  await unmount(component);
  second.resolve('too late');
  expect(await secondRun).toBe(false);
  expect(secondAccept).not.toHaveBeenCalled();
});

it('ignores a run retired by its owner, aborts its signal and lets the next run start', async () => {
  let action!: ReturnType<typeof useAction>;
  const component = harness(() => {
    action = useAction();
  });
  const stale = deferred<string>();
  const staleAccept = vi.fn();
  const signals: AbortSignal[] = [];
  const staleRun = action.run((_current, signal) => {
    signals.push(signal);
    return stale.promise;
  }, staleAccept);
  action.error = 'left over';

  action.retire();
  expect(action.busy).toBe(false);
  expect(action.error).toBe('');
  expect(signals[0].aborted).toBe(true);

  const fresh = deferred<string>();
  const freshRun = action.run((_current, signal) => {
    signals.push(signal);
    return fresh.promise;
  });
  stale.reject(new Error('late failure'));
  expect(await staleRun).toBe(false);
  expect(staleAccept).not.toHaveBeenCalled();
  expect(action.error).toBe('');
  expect(action.key).toBe('submit');
  expect(signals[1].aborted).toBe(false);

  fresh.resolve('done');
  expect(await freshRun).toBe(true);
  expect(action.busy).toBe(false);
  await unmount(component);
});

it('drops the outcome of a run once the caller says its scope is gone', async () => {
  let action!: ReturnType<typeof useAction>;
  const scope = { id: 'first' };
  const component = harness(() => {
    action = useAction({ current: () => scope.id === 'first' });
  });
  const pending = deferred<string>();
  const accept = vi.fn();
  const observed: boolean[] = [];
  const run = action.run((current) => {
    observed.push(current());
    return pending.promise.finally(() => observed.push(current()));
  }, accept);
  scope.id = 'second';
  pending.reject(new Error('belongs to the first scope'));

  expect(await run).toBe(false);
  expect(observed).toEqual([true, false]);
  expect(accept).not.toHaveBeenCalled();
  expect(action.error).toBe('');
  await unmount(component);
});

it('raises only the conflict flag for a failure the caller claims and shows the message for the rest', async () => {
  let action!: ReturnType<typeof useAction>;
  const claimed: unknown[] = [];
  const editing = { existing: true };
  const component = harness(() => {
    action = useAction({
      conflict: (cause) => {
        claimed.push(cause);
        return editing.existing;
      }
    });
  });
  const mismatch = etagMismatch();
  expect(await action.run(() => Promise.reject(mismatch))).toBe(false);
  expect(claimed).toEqual([mismatch]);
  expect(action.conflict).toBe(true);
  expect(action.error).toBe('');
  expect(action.busy).toBe(false);

  editing.existing = false;
  expect(await action.run(() => Promise.reject(mismatch))).toBe(false);
  expect(action.conflict).toBe(false);
  expect(action.error).toBe('Precondition failed');

  expect(
    await action.run(
      () => Promise.resolve('sent'),
      () => {
        throw mismatch;
      }
    )
  ).toBe(false);
  expect(claimed).toHaveLength(2);
  await unmount(component);
});

it('lists the field issues of a rejected write and clears them when the next run starts', async () => {
  let action!: ReturnType<typeof useAction>;
  const begin = vi.fn();
  const component = harness(() => {
    action = useAction({ begin });
  });
  const invalid = new ApiProblem({
    type: 'about:blank',
    title: 'Validation failed',
    status: 422,
    errors: { name: [{ code: 'required', message: 'Enter a name.' }] }
  });
  expect(
    await action.run(() => Promise.reject(invalid), undefined, { key: 'save' })
  ).toBe(false);
  expect(action.issues).toEqual([
    { field: 'name', code: 'required', message: 'Enter a name.' }
  ]);
  expect(begin).toHaveBeenCalledOnce();

  const next = deferred<string>();
  const run = action.run(() => next.promise, undefined, { key: 'probe' });
  expect(action.key).toBe('probe');
  expect(action.issues).toEqual([]);
  expect(action.error).toBe('');
  expect(begin).toHaveBeenCalledTimes(2);

  expect(await action.run(() => Promise.resolve('blocked'))).toBe(false);
  expect(begin).toHaveBeenCalledTimes(2);
  next.resolve('done');
  expect(await run).toBe(true);
  await unmount(component);
});
