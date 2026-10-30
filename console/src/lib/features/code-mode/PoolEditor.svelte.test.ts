// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, expect, it, vi } from 'vitest';
import type { CodeAccount } from '$lib/api/code-mode';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import PoolEditorProbe from './test/PoolEditorProbe.svelte';

const accountId = '01980000-0000-7000-8000-000000000010';
const otherId = '01980000-0000-7000-8000-000000000011';
const pool = {
  id: '01980000-0000-7000-8000-000000000020',
  project_id: '01980000-0000-7000-8000-000000000001',
  name: 'Weighted pool',
  kind: 'shared' as const,
  owner_user_id: null,
  account_ids: [accountId, otherId],
  account_weights: { [accountId]: 3, [otherId]: 1 },
  api_key_ids: [],
  etag: '01980000-0000-7000-8000-000000000021'
};
const accounts: CodeAccount[] = [accountId, otherId].map((id, index) => ({
  id,
  project_id: pool.project_id,
  provider_id: id,
  credential_id: id,
  principal: `principal-${index}`,
  name: `Account ${index + 1}`,
  adapter: 'codex',
  enabled: true,
  eligible: true,
  models: ['fixture-model'],
  health: 'unknown',
  grant_state: 'current',
  allowance: null,
  etag: pool.etag
}));
let component: ReturnType<typeof mount> | undefined;
let host: HTMLDivElement;
const client = new QueryClient({
  defaultOptions: { queries: { retry: false } }
});

afterEach(async () => {
  if (component) await unmount(component);
  client.clear();
  host?.remove();
  vi.unstubAllGlobals();
});

it('edits selected account weights and drops the weight of a removed account', async () => {
  authLifecycle.establishSession({
    user: {
      id: accountId,
      email: 'owner@example.test',
      display_name: 'Owner',
      role: 'owner',
      access_scope: 'global',
      operations: []
    },
    csrf_token: 'fixture'
  });
  const writes: unknown[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (request: Request) => {
      if (request.method === 'PUT') {
        writes.push(await request.json());
        return Response.json(pool);
      }
      return Response.json({ items: [], next_cursor: null });
    })
  );
  host = document.createElement('div');
  document.body.append(host);
  component = mount(PoolEditorProbe, {
    target: host,
    props: { client, current: pool, accounts }
  });
  flushSync();
  const weight = host.querySelector<HTMLInputElement>(
    `#code-weight-${accountId}`
  );
  expect(weight?.value).toBe('3');
  weight!.value = '7';
  weight!.dispatchEvent(new Event('input', { bubbles: true }));
  host
    .querySelector<HTMLInputElement>(
      `input[type="checkbox"][value="${otherId}"]`
    )!
    .click();
  flushSync();
  host
    .querySelector('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(writes).toEqual([
      expect.objectContaining({
        account_ids: [accountId],
        account_weights: { [accountId]: 7 }
      })
    ])
  );
});
