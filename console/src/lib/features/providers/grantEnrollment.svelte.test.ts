import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { json, route, serve } from '$lib/management/test/transport';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import type { GrantEnrollment, GrantEnrollmentCompletion } from './grants';
import { GrantEnrollmentState } from './grantEnrollment.svelte';

const provider = { id: 'provider-id', etag: 'provider-version' };
const enrollment: GrantEnrollment = {
  id: 'enrollment-id',
  provider_id: provider.id,
  slot_id: 'slot-id',
  expires_at: '2026-01-01T00:00:00Z',
  authorization_url: 'https://example.test/authorize',
  input: 'secret'
};
const deviceEnrollment: GrantEnrollment = {
  id: enrollment.id,
  provider_id: enrollment.provider_id,
  slot_id: enrollment.slot_id,
  expires_at: enrollment.expires_at,
  device: {
    interval: 5,
    user_code: 'device-code',
    verification_url: 'https://example.test/device'
  }
};
const completion: GrantEnrollmentCompletion = {
  provider_id: provider.id,
  credential_id: 'credential-id',
  credential_version: 2,
  etag: 'new-version',
  principal: 'operator'
};
const basePath = `/api/v1/providers/${provider.id}/grant-enrollments`;
const enrollmentPath = `${basePath}/${enrollment.id}`;

afterEach(() => {
  vi.unstubAllGlobals();
});

function grant() {
  const error = vi.fn();
  const completed = vi.fn();
  const ready = vi.fn((state: GrantEnrollmentState) => state);
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: {
      setup: () =>
        ready(
          new GrantEnrollmentState({ allowed: () => true, error, completed })
        )
    }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  return { state: ready.mock.results[0].value, error, completed };
}

describe('provider grant enrollment', () => {
  it('starts once for the selected slot using the displayed provider revision', async () => {
    const handler = vi.fn(async (request: Request) => {
      if (request.method === 'DELETE') {
        route(request, 'DELETE', enrollmentPath);
        return new Response(null, { status: 204 });
      }
      route(request, 'POST', basePath);
      expect(request.headers.get('If-Match')).toBe(provider.etag);
      expect(await request.json()).toEqual({ slot_id: enrollment.slot_id });
      return json(enrollment);
    });
    serve(handler, ['read', 'configure']);
    const { state } = grant();

    await state.start(provider, enrollment.slot_id);
    await state.start(provider, enrollment.slot_id);

    expect(state.enrollment).toEqual(enrollment);
    expect(state.busy).toBe('');
    expect(handler).toHaveBeenCalledOnce();
    await state.cancel();
  });

  it('requires input and clears the pasted secret before sending it once', async () => {
    const handler = vi.fn(async (request: Request) => {
      if (new URL(request.url).pathname === basePath) return json(enrollment);
      route(request, 'POST', `${enrollmentPath}/continue`);
      expect(state.input).toBe('');
      expect(await request.json()).toEqual({ input: 'upstream-secret' });
      return json(completion);
    });
    serve(handler, ['read', 'configure']);
    const { state, error, completed } = grant();
    await state.start(provider);

    state.input = '   ';
    await state.continue();
    expect(error).toHaveBeenLastCalledWith(
      'Paste the callback URL, code or key the upstream issued.'
    );
    expect(handler).toHaveBeenCalledTimes(1);

    state.input = ' upstream-secret ';
    await state.continue();
    await state.continue();
    expect(handler).toHaveBeenCalledTimes(2);
    expect(state.input).toBe('');
    expect(state.enrollment).toBeNull();
    expect(completed).toHaveBeenCalledOnce();
    expect(completed).toHaveBeenCalledWith(
      completion,
      expect.objectContaining({ secret: true })
    );
  });

  it('keeps polling a pending device grant and completes it when approved', async () => {
    const poll = vi
      .fn()
      .mockReturnValueOnce(json({ status: 'pending', interval: 9 }))
      .mockReturnValueOnce(json({ status: 'completed', completion }));
    serve(
      (request) => {
        if (new URL(request.url).pathname === basePath)
          return json(deviceEnrollment);
        route(request, 'POST', `${enrollmentPath}/poll`);
        return poll();
      },
      ['read', 'configure']
    );
    const { state, completed } = grant();
    await state.start(provider);

    await expect(state.poll()).resolves.toBe(9);
    expect(state.enrollment).toEqual(deviceEnrollment);
    await expect(state.poll()).resolves.toBeNull();
    expect(state.enrollment).toBeNull();
    expect(completed).toHaveBeenCalledWith(
      completion,
      expect.objectContaining({ secret: false })
    );
  });

  it.each([
    ['denied', 'was denied upstream'],
    ['expired', 'expired before it was approved']
  ])('ends a device grant that is %s', async (status, reason) => {
    serve(
      (request) => {
        if (new URL(request.url).pathname === basePath)
          return json(deviceEnrollment);
        route(request, 'POST', `${enrollmentPath}/poll`);
        return json({ status });
      },
      ['read', 'configure']
    );
    const { state, error, completed } = grant();
    await state.start(provider);

    await expect(state.poll()).resolves.toBeNull();
    expect(state.enrollment).toBeNull();
    expect(error).toHaveBeenLastCalledWith(
      `The device sign-in ${reason}. Start enrollment again.`
    );
    expect(completed).not.toHaveBeenCalled();
  });

  it('keeps a device grant on a transient error and ends it on refusal', async () => {
    const poll = vi
      .fn()
      .mockReturnValueOnce(json({ title: 'Try again', status: 503 }, 503))
      .mockReturnValueOnce(json({ title: 'Access denied', status: 403 }, 403));
    serve(
      (request) => {
        if (new URL(request.url).pathname === basePath)
          return json(deviceEnrollment);
        route(request, 'POST', `${enrollmentPath}/poll`);
        return poll();
      },
      ['read', 'configure']
    );
    const { state, error } = grant();
    await state.start(provider);

    await expect(state.poll()).resolves.toBe(5);
    expect(state.enrollment).toEqual(deviceEnrollment);
    expect(error).not.toHaveBeenCalledWith('Try again');
    await expect(state.poll()).resolves.toBeNull();
    expect(state.enrollment).toBeNull();
    expect(error).toHaveBeenLastCalledWith('Access denied');
  });

  it('clears a pasted secret when an enrollment is canceled', async () => {
    const cancel = vi.fn(() => new Response(null, { status: 204 }));
    serve(
      (request) => {
        if (new URL(request.url).pathname === basePath) return json(enrollment);
        route(request, 'DELETE', enrollmentPath);
        return cancel();
      },
      ['read', 'configure']
    );
    const { state } = grant();
    await state.start(provider);
    state.input = 'upstream-secret';

    await state.cancel();

    expect(cancel).toHaveBeenCalledOnce();
    expect(state.input).toBe('');
    expect(state.enrollment).toBeNull();
    expect(state.busy).toBe('');
  });

  it('discards and cancels an enrollment that arrives after a principal change', async () => {
    const gate = new AbortController();
    const release = new Promise<void>((resolve) =>
      gate.signal.addEventListener('abort', () => resolve(), { once: true })
    );
    const start = vi.fn(async () => {
      await release;
      return json(enrollment);
    });
    const cancel = vi.fn(() => new Response(null, { status: 204 }));
    serve(
      (request) => {
        if (new URL(request.url).pathname === basePath) return start();
        route(request, 'DELETE', enrollmentPath);
        return cancel();
      },
      ['read', 'configure']
    );
    const { state, completed } = grant();
    const pending = state.start(provider);
    await vi.waitFor(() => expect(start).toHaveBeenCalledOnce());

    authLifecycle.establishSession({
      user: {
        id: 'another-owner',
        email: 'another@example.test',
        display_name: 'Another owner',
        role: 'owner',
        access_scope: 'global',
        operations: ['read', 'configure']
      },
      csrf_token: 'fixture-csrf'
    });
    gate.abort();
    await pending;

    await vi.waitFor(() => expect(cancel).toHaveBeenCalledOnce());
    expect(state.enrollment).toBeNull();
    expect(state.busy).toBe('');
    expect(completed).not.toHaveBeenCalled();
  });
});
