// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it } from 'vitest';
import NavigationProbe from '$lib/components/test/NavigationProbe.svelte';
import { operationsFor } from '$lib/features/access/session/test/grants';

let host: HTMLElement;
let client: QueryClient;

beforeEach(() => {
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  client.setQueryData(['service-capabilities'], {
    local_login_enabled: true,
    oidc_login_enabled: false,
    gateway_available: true
  });
});

afterEach(() => {
  client.clear();
  host.remove();
});

function links(global: boolean) {
  const component = mount(NavigationProbe, {
    target: host,
    props: {
      client,
      grant: {
        operations: operationsFor('operator', global),
        access_scope: global ? 'global' : 'assigned'
      }
    }
  });
  flushSync();
  const labels = [...host.querySelectorAll('a')].map((link) =>
    link.textContent?.trim()
  );
  void unmount(component);
  return labels;
}

it('offers installation pages only to installation-wide members', () => {
  const global = links(true);
  const assigned = links(false);
  for (const page of ['Audit', 'Settings', 'Access']) {
    expect(global).toContain(page);
    expect(assigned).not.toContain(page);
  }
  for (const page of ['Providers', 'Routes', 'API Keys', 'Requests']) {
    expect(assigned).toContain(page);
  }
});
