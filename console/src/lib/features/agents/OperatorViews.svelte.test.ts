import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import * as projectsApi from '$lib/features/access/projects/api';
import OperatorViewsProbe from './test/OperatorViewsProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  getTask: vi.fn(),
  listDelegations: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', () => ({
  listProjectMemberships: vi.fn()
}));

it('shows unknown outcomes and unresolved fees separately from observed remote charges', async () => {
  vi.mocked(projectsApi.listProjectMemberships).mockResolvedValue([]);
  vi.mocked(api.listDelegations).mockResolvedValue({
    items: [],
    nextCursor: null
  });
  vi.mocked(api.getTask).mockResolvedValue({
    id: 'task',
    project_id: 'project',
    api_key_id: 'key',
    publication_id: 'agent',
    revision_id: 'revision',
    agent_principal_id: 'agent-principal',
    member_principal_id: 'member-principal',
    delegation_pair_id: 'stable-pair',
    state: 'outcome_unknown',
    callback_attempts: 2,
    created_at: '2026-10-24T09:00:00Z',
    deadline_at: '2026-10-24T10:00:00Z',
    callback_models: [
      {
        request_id: 'request',
        attempt_id: null,
        state: 'pending',
        currency: null,
        observed_cost: null,
        observed_charge: null,
        remaining_cost: null,
        remaining_charge: null
      }
    ],
    callback_tools: [
      {
        dispatch_id: 'tool',
        run_id: 'tool-run',
        state: 'settled',
        pricing_state: 'priced',
        currency: 'USD',
        observed_cost: '0.10',
        observed_charge: '0.10',
        remaining_cost: '0',
        remaining_charge: '0'
      }
    ],
    remote_fees: [
      {
        dispatch_id: 'dispatch',
        run_id: 'run',
        state: 'outcome_unknown',
        currency: 'USD',
        observed_cost: '0',
        observed_charge: '0',
        remaining_cost: '0.25',
        remaining_charge: '0.25',
        created_at: '2026-10-24T09:00:00Z'
      }
    ]
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(OperatorViewsProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('outcome unknown');
    const label = Array.from(host.querySelectorAll('dt')).find(
      (node) => node.textContent === 'Callbacks admitted'
    );
    expect(label?.nextElementSibling?.textContent).toBe('2');
    expect(host.textContent).toContain('stable-pair');
    expect(
      host.querySelector('#callback-models')?.parentElement?.textContent
    ).toContain('Unknown / Unknown');
    expect(
      host.querySelector('#callback-tools')?.parentElement?.textContent
    ).toContain('0.10 / 0.10');
    expect(
      host.querySelector('[aria-label="Observed cost and charge"]')?.textContent
    ).toContain('0 / 0');
    expect(
      host.querySelector('[aria-label="Unresolved cost and charge"]')
        ?.textContent
    ).toContain('0.25 / 0.25');
  });
});

it('shows the principals, pair budget and revocation without claims', async () => {
  vi.mocked(projectsApi.listProjectMemberships).mockResolvedValue([]);
  vi.mocked(api.getTask).mockResolvedValue({
    id: 'task',
    project_id: 'project',
    api_key_id: 'key',
    publication_id: 'agent',
    revision_id: 'revision',
    agent_principal_id: null,
    member_principal_id: null,
    delegation_pair_id: null,
    state: 'canceled',
    callback_attempts: null,
    created_at: '2026-10-24T09:00:00Z',
    deadline_at: '2026-10-24T10:00:00Z',
    remote_fees: [],
    callback_models: [],
    callback_tools: []
  });
  const window = { accrued: '3.5', window_ends_at: '2026-10-25T00:00:00Z' };
  const revoked = {
    id: 'delegation',
    project_id: 'project',
    api_key_id: 'key',
    agent_id: 'agent-principal',
    member_id: 'member-principal',
    member_authorized: false,
    consent_pending: false,
    expires_at: '2026-10-24T10:00:00Z',
    consumed_at: null,
    revoked_at: '2026-10-24T09:30:00Z',
    api_key_revoked_at: null,
    agent_revoked_at: null,
    budget_policy: { daily_cost_limit: '10' },
    budget: {
      daily: window,
      weekly: window,
      monthly: window,
      cost_basis: 'cost' as const,
      unpriced_attempts: 0
    }
  };
  vi.mocked(api.listDelegations).mockResolvedValue({
    items: [revoked, { ...revoked, id: 'inactive', revoked_at: null }],
    nextCursor: null
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(OperatorViewsProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('canceled');
    const label = Array.from(host.querySelectorAll('dt')).find(
      (node) => node.textContent === 'Callbacks admitted'
    );
    expect(label?.nextElementSibling?.textContent).toBe('Unknown');
    expect(host.textContent).toContain('agent-principal');
    expect(host.textContent).toContain('member-principal');
    expect(host.textContent).toContain('3.5 / 10');
    expect(host.textContent).toContain('Revoked');
    expect(host.textContent?.replace(/\s+/g, ' ')).toContain(
      'Member authority inactive'
    );
  });
});
