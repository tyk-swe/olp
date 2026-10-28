import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import * as projectsApi from '$lib/features/access/projects/api';
import ToolCallsProbe from './test/ToolCallsProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listToolCalls: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', () => ({ listProjects: vi.fn() }));

it('keeps unresolved exposure separate from observed spend and refreshes settlement', async () => {
  const id = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50701';
  const call: api.ToolCall = {
    id,
    dispatch_id: id,
    api_key_id: id,
    project_id: id,
    toolset_id: id,
    toolset_revision_id: id,
    server_id: id,
    server_revision_id: id,
    tool_alias: 'lookup',
    input_digest: '1'.repeat(64),
    result_digest: null,
    status: 'outcome_unknown',
    is_error: false,
    started_at: '2026-10-24T09:00:00Z',
    completed_at: null,
    created_at: '2026-10-24T09:00:00Z',
    call_digest: '2'.repeat(64),
    debit_state: 'outcome_unknown',
    cost_bound: '0.25',
    charge_bound: '0.25',
    observed_cost: '0',
    observed_charge: '0',
    remaining_cost: '0.25',
    remaining_charge: '0.25',
    unit_price: '0.25',
    pricing_state: 'priced',
    currency: 'USD',
    guardrail_decisions: []
  };
  vi.mocked(projectsApi.listProjects).mockResolvedValue([]);
  vi.mocked(api.listToolCalls).mockResolvedValue({
    items: [call],
    nextCursor: null
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(ToolCallsProbe, { target: host, props: { client } });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => {
    flushSync();
    expect(
      host.querySelector('[aria-label="Unresolved cost and charge"]')
        ?.textContent
    ).toBe('0.25 / 0.25');
    expect(
      host.querySelector('[aria-label="Observed cost and charge"]')?.textContent
    ).toBe('0 / 0');
    expect(host.textContent).toContain('outcome unknown');
  });
  vi.mocked(api.listToolCalls).mockResolvedValue({
    items: [
      {
        ...call,
        status: 'succeeded',
        debit_state: 'settled',
        observed_cost: call.cost_bound,
        observed_charge: call.charge_bound,
        remaining_cost: '0',
        remaining_charge: '0'
      }
    ],
    nextCursor: null
  });
  const refresh = [...host.querySelectorAll('button')].find(
    (button) => button.textContent?.trim() === 'Refresh'
  );
  refresh?.click();
  await vi.waitFor(() => {
    flushSync();
    expect(
      host.querySelector('[aria-label="Unresolved cost and charge"]')
        ?.textContent
    ).toBe('0 / 0');
    expect(
      host.querySelector('[aria-label="Observed cost and charge"]')?.textContent
    ).toBe('0.25 / 0.25');
    expect(host.textContent).toContain('succeeded');
    expect(host.textContent).toContain('settled');
  });
  vi.mocked(api.listToolCalls).mockImplementation(async (_project, key) => ({
    items: key === id ? [] : [call],
    nextCursor: null
  }));
  host.querySelector('input[name="key"]')?.setAttribute('value', id);
  host
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => {
    flushSync();
    expect(host.textContent).toContain('No tool calls in this view.');
  });
});
