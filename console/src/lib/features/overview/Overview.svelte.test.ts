import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import type { FixedRole } from '$lib/features/access/session/authorization';
import { overviewKeys } from '$lib/features/overview/overviewKeys';
import { requestKeys } from '$lib/features/usage/history/requestKeys';
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
      role,
      access_scope: 'global'
    },
    csrf_token: 'overview-test'
  });
}

const emptySummary = {
  active_providers: 0,
  active_routes: 0,
  enabled_models: 0,
  usable_api_key: false
};

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { staleTime: Infinity, retry: false } }
  });
  client.setQueryData(overviewKeys.summary(), emptySummary);
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
  client.setQueryData(overviewKeys.summary(), {
    active_providers: 1,
    active_routes: 1,
    enabled_models: 2,
    usable_api_key: true
  });
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
  client.removeQueries({ queryKey: overviewKeys.root });
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
    .find({ queryKey: overviewKeys.summary() })!
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
