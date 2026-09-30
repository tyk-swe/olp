import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  authenticationCapabilities,
  currentSession,
  type AuthenticationCapabilities,
  type CurrentSession
} from '$lib/features/access/session/api';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { operationsFor } from '$lib/features/access/session/test/grants';
import ConsoleLayoutProbe from './test/ConsoleLayoutProbe.svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({
  page: { url: new URL('https://console.test/providers') }
}));
vi.mock('$lib/features/access/session/api', () => ({
  authenticationCapabilities: vi.fn(),
  currentSession: vi.fn(),
  logout: vi.fn()
}));
vi.mock('$lib/features/access/setup/api', () => ({ getSetupStatus: vi.fn() }));

const session: CurrentSession = {
  csrf_token: 'csrf-token',
  installation_name: 'Test installation',
  operations: operationsFor('owner'),
  user: {
    id: '11111111-1111-1111-1111-111111111111',
    email: 'owner@example.com',
    display_name: 'Owner',
    role: 'owner',
    access_scope: 'global',
    operations: operationsFor('owner')
  }
};

const capabilities: AuthenticationCapabilities = {
  local_login_enabled: true,
  oidc_login_enabled: false,
  notifications_active: false,
  gateway_available: true
};

let host: HTMLElement;
let client: QueryClient;
let detachQueryClient: () => void;
let component: ReturnType<typeof mount> | undefined;

function linkLabels() {
  return [...host.querySelectorAll('a')].map((link) =>
    link.textContent?.trim()
  );
}

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  // jsdom has no matchMedia; AppShell only listens for viewport crossings.
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn()
  }));
  client = new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: Infinity,
        queryKeyHashFn: (queryKey) => authLifecycle.queryKeyHash(queryKey)
      }
    }
  });
  detachQueryClient = authLifecycle.attachQueryClient(client);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  detachQueryClient();
  client.clear();
  host.remove();
});

it('fetches capabilities alongside the session and seeds the query cache', async () => {
  let sessionSettled = false;
  let resolveSession!: (session: CurrentSession) => void;
  const sessionRequest = new Promise<CurrentSession>((resolve) => {
    resolveSession = resolve;
  });
  void sessionRequest.then(() => {
    sessionSettled = true;
  });
  vi.mocked(currentSession).mockReturnValue(sessionRequest);
  vi.mocked(authenticationCapabilities).mockResolvedValue(capabilities);

  component = mount(ConsoleLayoutProbe, { target: host, props: { client } });
  flushSync();
  await vi.waitFor(() =>
    expect(authenticationCapabilities).toHaveBeenCalledTimes(1)
  );
  expect(sessionSettled).toBe(false);

  resolveSession(session);
  await vi.waitFor(() => {
    flushSync();
    expect(linkLabels()).toContain('Providers');
  });
  // The seeded capabilities satisfy the navigation query, so the endpoint is
  // not requested a second time.
  expect(authenticationCapabilities).toHaveBeenCalledTimes(1);
});

it('still authenticates and renders the shell when capabilities fail', async () => {
  vi.mocked(currentSession).mockResolvedValue(session);
  vi.mocked(authenticationCapabilities).mockRejectedValue(
    new Error('Service unavailable')
  );

  component = mount(ConsoleLayoutProbe, { target: host, props: { client } });
  flushSync();
  await vi.waitFor(() => {
    flushSync();
    expect(linkLabels()).toContain('Overview');
  });
  expect(host.textContent).toContain('Test installation');
});
