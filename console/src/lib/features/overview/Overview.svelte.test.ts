import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import type { FixedRole } from '$lib/features/access/session/authorization';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import { requestKeys } from '$lib/features/usage/history/requestKeys';
import { apiKeyQueries } from '$lib/features/access/api-keys/apiKeyQueries';
import OverviewProbe from './test/OverviewProbe.svelte';

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount>;

function establish(role: FixedRole) {
  authLifecycle.establishSession({
    user: {
      id: 'overview-user',
      email: 'test@example.com',
      display_name: 'Test',
      role
    },
    csrf_token: 'overview-test'
  });
}

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { staleTime: Infinity, retry: false } }
  });
  client.setQueryData(providerKeys.all(), []);
  client.setQueryData(routeKeys.all(), []);
  client.setQueryData(apiKeyQueries.hasNonrevoked(), false);
  client.setQueryData(requestKeys.overview(), { items: [] });
  establish('owner');
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host.remove();
  await authLifecycle.principalInvalidated();
});

it('updates setup actions and viewing links when the mounted principal changes', () => {
  component = mount(OverviewProbe, { target: host, props: { client } });
  flushSync();
  expect(host.querySelector('.page-heading a')?.getAttribute('href')).toBe(
    '/providers/new'
  );
  establish('developer');
  flushSync();
  expect(host.querySelector('.page-heading a')?.getAttribute('href')).toBe(
    '/api-keys/new'
  );
  expect(host.querySelector('.checklist a[href="/providers/new"]')).toBeNull();
  expect(host.querySelector('.checklist a[href="/providers"]')).not.toBeNull();
  establish('viewer');
  flushSync();
  expect(host.querySelector('.page-heading a')).toBeNull();
  expect(host.querySelector('.checklist a[href="/api-keys/new"]')).toBeNull();
});

it('shows a compact completion summary and requests action for a configured gateway', () => {
  client.setQueryData(providerKeys.all(), [
    { active_revision: 1, enabled_model_count: 1 }
  ]);
  client.setQueryData(routeKeys.all(), [{ id: 'route' }]);
  client.setQueryData(apiKeyQueries.hasNonrevoked(), true);
  component = mount(OverviewProbe, { target: host, props: { client } });
  flushSync();
  expect(host.querySelector('h1')?.textContent).toBe('Gateway overview');
  expect(host.querySelector('.page-heading a')?.getAttribute('href')).toBe(
    '/requests'
  );
  expect(host.querySelector('.complete-summary')).not.toBeNull();
  expect(host.querySelector('.checklist ol')).toBeNull();
});

it('keeps setup actions indeterminate while required data is loading', () => {
  client.setDefaultOptions({
    queries: { enabled: false, retry: false, staleTime: Infinity }
  });
  client.removeQueries({ queryKey: providerKeys.all() });
  component = mount(OverviewProbe, { target: host, props: { client } });
  flushSync();
  expect(host.querySelector('.page-heading a')).toBeNull();
  expect(host.querySelector('.completion')?.textContent).toContain('Checking');
  expect(
    host.querySelector('[role="progressbar"]')?.hasAttribute('aria-valuenow')
  ).toBe(false);
});

it('offers retry without presenting stale setup data as complete', () => {
  client.setDefaultOptions({
    queries: { enabled: false, retry: false, staleTime: Infinity }
  });
  client
    .getQueryCache()
    .find({ queryKey: providerKeys.all() })!
    .setState({ status: 'error', error: new Error('Unavailable') });
  component = mount(OverviewProbe, { target: host, props: { client } });
  flushSync();
  expect(host.querySelector('.page-heading a')).toBeNull();
  expect(host.querySelector('.check-error')?.textContent).toContain(
    'Try again'
  );
  expect(host.querySelector('.completion')?.textContent).toContain('Unknown');
  expect(host.querySelector('.complete-summary')).toBeNull();
});
