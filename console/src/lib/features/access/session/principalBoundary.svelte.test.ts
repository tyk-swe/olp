import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, expect, it, vi } from 'vitest';
import { authLifecycle } from './lifecycle';
import type { AuthenticatedSession } from './state';
import PrincipalBoundaryProbe from './test/PrincipalBoundaryProbe.svelte';

const guards = vi.hoisted(
  () => new Set<(navigation: { cancel: () => void }) => void>()
);
vi.mock('$app/navigation', async () => {
  const { onMount } = await import('svelte');
  return {
    goto: vi.fn(),
    beforeNavigate(callback: (navigation: { cancel: () => void }) => void) {
      onMount(() => {
        guards.add(callback);
        return () => {
          guards.delete(callback);
        };
      });
    }
  };
});
vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/settings') }
}));
vi.mock('$lib/components/AppShell.svelte', async () => ({
  default: (await import('./test/PrincipalShell.svelte')).default
}));
vi.mock('./api', () => ({
  currentSession: vi.fn(),
  authenticationCapabilities: vi.fn(async () => ({
    local_login_enabled: true,
    oidc_login_enabled: false
  })),
  logout: vi.fn()
}));
import { currentSession } from './api';

let view: ReturnType<typeof mount> | undefined;
let target: HTMLElement;
let client: QueryClient;
afterEach(async () => {
  if (view) await unmount(view);
  view = undefined;
  await authLifecycle.principalInvalidated();
  client?.clear();
  target?.remove();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it('retires authority-owned drafts and navigation guards while preserving CSRF-only refreshes', async () => {
  const session: AuthenticatedSession = {
    user: {
      id: 'owner',
      email: 'owner@example.com',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: ['read', 'configure']
    },
    csrf_token: 'csrf-original'
  };
  authLifecycle.establishSession(session);
  vi.mocked(currentSession).mockResolvedValue({
    ...session,
    installation_name: 'OLP',
    operations: ['read', 'configure']
  } as Awaited<ReturnType<typeof currentSession>>);
  target = document.createElement('div');
  document.body.append(target);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  view = mount(PrincipalBoundaryProbe, { target, props: { client } });
  await vi.waitFor(() => {
    flushSync();
    expect(target.querySelector('input')).not.toBeNull();
  });
  const original = target.querySelector<HTMLInputElement>('input')!;
  original.value = 'prior-authority-secret';
  original.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  authLifecycle.establishSession({ ...session, csrf_token: 'csrf-rotated' });
  flushSync();
  expect(target.querySelector('input')).toBe(original);
  expect(original.value).toBe('prior-authority-secret');
  authLifecycle.establishSession({
    ...session,
    user: { ...session.user, operations: ['configure', 'read'] }
  });
  flushSync();
  expect(target.querySelector('input')).toBe(original);
  expect(original.value).toBe('prior-authority-secret');
  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  const cancel = vi.fn();
  for (const guard of guards) guard({ cancel });
  expect(cancel).toHaveBeenCalledTimes(1);
  authLifecycle.establishSession({
    ...session,
    user: { ...session.user, access_scope: 'assigned' }
  });
  flushSync();
  expect(target.querySelector('input')).not.toBe(original);
  expect(target.querySelector<HTMLInputElement>('input')!.value).toBe('');
  expect(guards.size).toBe(1);
  cancel.mockClear();
  confirm.mockClear();
  for (const guard of guards) guard({ cancel });
  expect(cancel).not.toHaveBeenCalled();
  expect(confirm).not.toHaveBeenCalled();
  const assignedDraft = target.querySelector<HTMLInputElement>('input')!;
  assignedDraft.value = 'assigned-authority-secret';
  assignedDraft.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  authLifecycle.establishSession({
    ...session,
    user: { ...session.user, access_scope: 'assigned', operations: ['read'] }
  });
  flushSync();
  expect(target.querySelector('input')).not.toBe(assignedDraft);
  expect(target.querySelector<HTMLInputElement>('input')!.value).toBe('');
  expect(guards.size).toBe(1);
});
