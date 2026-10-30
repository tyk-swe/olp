// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import type { CodeRoute } from '$lib/api/code-mode';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import RouteEditorProbe from './test/RouteEditorProbe.svelte';

it('validates ordered model pools and saves the explicit recovery policy', async () => {
  const route: CodeRoute = {
    id: '01980000-0000-7000-8000-000000000001',
    project_id: '01980000-0000-7000-8000-000000000002',
    pool_id: '01980000-0000-7000-8000-000000000003',
    etag: '01980000-0000-7000-8000-000000000004',
    slug: 'coding',
    models: ['native-main', 'native-alternate'],
    enabled: true,
    model_rewriting: true,
    aliases: { 'team-code': 'native-main' },
    initial_recovery_attempts: 0
  };
  authLifecycle.establishSession({
    user: {
      id: route.id,
      email: 'owner@example.test',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: []
    },
    csrf_token: 'fixture'
  });
  const fetch = vi.fn(async (request: Request) =>
    request.method === 'PUT'
      ? Response.json(route)
      : Response.json({ items: [], next_cursor: null })
  );
  vi.stubGlobal('fetch', fetch);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(RouteEditorProbe, {
    target: host,
    props: { client, current: route }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
    vi.unstubAllGlobals();
  });
  flushSync();
  const input = host.querySelector<HTMLTextAreaElement>('#code-model-pools')!;
  input.value = 'team-code=native-alternate,native-main';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'starting with the public model’s normal exact target'
    )
  );
  input.value = 'team-code=native-main,native-alternate';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  const recovery = host.querySelector<HTMLSelectElement>(
    '#code-recovery-attempts'
  )!;
  recovery.value = '1';
  recovery.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(
      fetch.mock.calls.find(([request]) => request.method === 'PUT')
    ).toBeDefined()
  );
  const write = fetch.mock.calls.find(
    ([request]) => request.method === 'PUT'
  )![0];
  expect(await write.json()).toEqual(
    expect.objectContaining({
      model_pools: { 'team-code': ['native-main', 'native-alternate'] },
      aliases: route.aliases,
      model_rewriting: true,
      initial_recovery_attempts: 1
    })
  );
});
