import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
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
  vi.unstubAllGlobals();
  await authLifecycle.principalInvalidated();
});

it('counts the gateway with one overview request and no collection lists', async () => {
  const requests = captureRequests((request) => {
    switch (new URL(request.url).pathname) {
      case '/api/v1/overview':
        return jsonResponse({
          active_providers: 120,
          active_routes: 80,
          enabled_models: 300,
          usable_api_key: true
        });
      case '/api/v1/requests':
        return jsonResponse({ items: [], next_cursor: null });
      case '/api/v1/auth/capabilities':
        return jsonResponse({
          local_login_enabled: true,
          oidc_login_enabled: false,
          retention_enforced: true
        });
    }
    return jsonResponse({ title: 'Not found', status: 404 }, { status: 404 });
  });
  client.removeQueries();
  component = mount(OverviewProbe, { target: host, props: { client } });
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('120 active');
    expect(host.textContent).toContain('80 active');
  });
  const paths = requests.map((request) => new URL(request.url).pathname);
  expect(paths.filter((path) => path === '/api/v1/overview')).toHaveLength(1);
  for (const collection of [
    '/api/v1/providers',
    '/api/v1/provider-models',
    '/api/v1/routes',
    '/api/v1/route-drafts',
    '/api/v1/api-keys'
  ])
    expect(paths).not.toContain(collection);
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
