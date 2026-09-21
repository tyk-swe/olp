import { mount, unmount } from 'svelte';
import { afterEach, expect, it, vi } from 'vitest';
import InvitationAcceptance from './InvitationAcceptance.svelte';
import {
  acceptInvitation,
  authenticationCapabilities
} from '$lib/features/access/session/auth';

vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));
vi.mock('$lib/features/access/session/auth', () => ({
  acceptInvitation: vi.fn(),
  authenticationCapabilities: vi.fn()
}));
vi.mock('$lib/features/access/session/lifecycle', () => ({
  authLifecycle: { authenticate: vi.fn(), abortAuthenticationWork: vi.fn() }
}));
let component: ReturnType<typeof mount>;
let host: HTMLElement;
afterEach(async () => {
  await unmount(component);
  host.remove();
  window.location.hash = '';
});

it('blocks unsupported SSO-only onboarding before submission and recovers when policy permits it', async () => {
  window.location.hash = '#token=one-time-token';
  vi.mocked(authenticationCapabilities)
    .mockResolvedValueOnce({
      local_login_enabled: false,
      notifications_active: false,
      oidc_login_enabled: true
    })
    .mockResolvedValueOnce({
      local_login_enabled: true,
      notifications_active: false,
      oidc_login_enabled: true
    });
  host = document.createElement('div');
  document.body.append(host);
  component = mount(InvitationAcceptance, { target: host });
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Your invitation has not been consumed')
  );
  expect(host.querySelector('form')).toBeNull();
  expect(acceptInvitation).not.toHaveBeenCalled();
  host.querySelector('button')!.click();
  await vi.waitFor(() =>
    expect(host.querySelector('input[type="password"]')).not.toBeNull()
  );
  expect(authenticationCapabilities).toHaveBeenCalledTimes(2);
});
