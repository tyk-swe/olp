import { mount, unmount, flushSync } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import LoginPage from './LoginPage.svelte';
import { authenticationCapabilities } from '$lib/features/access/session/auth';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({
  page: { url: new URL('https://console.test/login') }
}));
vi.mock('$lib/features/access/session/auth', () => ({
  authenticationCapabilities: vi.fn(),
  beginOidcLogin: vi.fn(),
  login: vi.fn()
}));
vi.mock('$lib/features/access/session/lifecycle', () => ({
  authLifecycle: { abortAuthenticationWork: vi.fn(), authenticate: vi.fn() }
}));
let host: HTMLElement;
let component: ReturnType<typeof mount>;
beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
});
afterEach(async () => {
  await unmount(component);
  host.remove();
});

it('distinguishes capability failure from disabled sign-in and retries accessibly', async () => {
  vi.mocked(authenticationCapabilities)
    .mockRejectedValueOnce(new Error('Service unavailable'))
    .mockResolvedValueOnce({
      local_login_enabled: true,
      oidc_login_enabled: true
    });
  component = mount(LoginPage, { target: host });
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Sign-in options could not be loaded')
  );
  expect(host.textContent).not.toContain(
    'No sign-in method is currently available'
  );
  const retry = [...host.querySelectorAll('button')].find(
    (b) => b.textContent?.trim() === 'Retry'
  );
  expect(retry).toBeDefined();
  retry!.click();
  await vi.waitFor(() => {
    flushSync();
    expect(host.querySelector('input[type="password"]')).not.toBeNull();
    expect(host.textContent).toContain('Continue with single sign-on');
  });
  expect(authenticationCapabilities).toHaveBeenCalledTimes(2);
  expect(host.querySelector('[role="alert"]')).toBeNull();
});

it('shows administrative disabled state only after successful capability retrieval', async () => {
  vi.mocked(authenticationCapabilities).mockResolvedValue({
    local_login_enabled: false,
    oidc_login_enabled: false
  });
  component = mount(LoginPage, { target: host });
  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'No sign-in method is currently available'
    )
  );
  expect(host.textContent).not.toContain('could not be loaded');
});
