// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import type { CodeAccount, CodePool } from '$lib/api/code-mode';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import PoolEditorProbe from './test/PoolEditorProbe.svelte';

const accountId = '01980000-0000-7000-8000-000000000010';
const otherId = '01980000-0000-7000-8000-000000000011';
const pool: CodePool = {
  id: '01980000-0000-7000-8000-000000000020',
  project_id: '01980000-0000-7000-8000-000000000001',
  name: 'Weighted pool',
  kind: 'shared',
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
  const fetch = vi.fn(async (request: Request) => {
    if (request.method === 'PUT') return Response.json(pool);
    return Response.json({ items: [], next_cursor: null });
  });
  vi.stubGlobal('fetch', fetch);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(PoolEditorProbe, {
    target: host,
    props: { client, current: pool, accounts }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
    vi.unstubAllGlobals();
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
    expect(
      fetch.mock.calls.filter(([request]) => request.method === 'PUT')
    ).toHaveLength(1)
  );
  const write = fetch.mock.calls.find(
    ([request]) => request.method === 'PUT'
  )![0];
  expect(await write.json()).toEqual(
    expect.objectContaining({
      account_ids: [accountId],
      account_weights: { [accountId]: 7 }
    })
  );
});
