import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  authenticationCapabilities,
  currentSession,
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
