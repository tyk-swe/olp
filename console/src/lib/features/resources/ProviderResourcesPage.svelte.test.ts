// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, expect, it, vi } from 'vitest';
import * as api from './api';
import ProviderResourcesProbe from './test/ProviderResourcesProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listProviderResources: vi.fn()
}));
vi.mock('./AgentResources.svelte', () => ({ default: () => ({}) }));
vi.mock('./NativeResources.svelte', () => ({ default: () => ({}) }));
vi.mock('./RetrievalBindings.svelte', () => ({ default: () => ({}) }));

let host: HTMLDivElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

afterEach(async () => {
  if (component) await unmount(component);
  client?.clear();
  host?.remove();
});

it('refreshes the selected settlement state, expiry and errors', async () => {
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  let resource: api.ProviderResource = {
    id: `fine_tuning_job_${'1'.repeat(32)}`,
    kind: 'fine_tuning_job',
    route: 'training',
    provider_id: 'provider',
    provider_name: 'Provider',
    api_key_id: 'key',
    api_key_name: 'Key',
    created_at: '2026-10-24T09:00:00Z',
    updated_at: '2026-10-24T09:00:00Z',
    expires_at: '2026-10-24T10:00:00Z',
    state: 'active',
    upstream_id: 'native-job',
    upstream_model: 'model',
    retained_settlement: {
      state: 'evidence_missing',
      missing_evidence: true,
      remote_terminated: true,
      next_observation_at: '2026-10-24T10:00:00Z',
      last_error: 'usage_evidence_missing'
    }
  };
  vi.mocked(api.listProviderResources).mockImplementation(async () => ({
    items: [resource],
    nextCursor: null
  }));
  component = mount(ProviderResourcesProbe, {
    target: host,
    props: { client }
  });
  flushSync();
  const button = (name: string) =>
    [...host.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === name
    );
  await vi.waitFor(() => expect(button('Settlement controls')).toBeDefined());
  button('Settlement controls')!.click();
  flushSync();
  const controls = () =>
    host.querySelector('[aria-label="Retained resource settlement controls"]');
  expect(controls()?.textContent).toContain('usage_evidence_missing');

  resource = {
    ...resource,
    expires_at: '2026-10-24T11:00:00Z',
    retained_settlement: {
      ...resource.retained_settlement!,
      state: 'closed',
      missing_evidence: false,
      last_error: null
    }
  };
  button('Refresh')!.click();
  await vi.waitFor(() => {
    flushSync();
    expect(controls()?.textContent).toContain('State: closed');
    expect(controls()?.textContent).toContain('2026-10-24T11:00:00Z');
    expect(controls()?.textContent).not.toContain('usage_evidence_missing');
  });
});
