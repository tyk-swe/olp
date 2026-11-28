import type { QueryClient } from '@tanstack/svelte-query';
import { describe, expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  clickButton,
  findButton,
  mountWithState,
  submitForm
} from '#test/mount.js';
import DelegationsPage from './DelegationsPage.svelte';
import ExposurePage from './ExposurePage.svelte';
import InvestigationsPage from './InvestigationsPage.svelte';
import RunDetailPage from './RunDetailPage.svelte';
import RunsPage from './RunsPage.svelte';
import TasksPage from './TasksPage.svelte';
import ToolCallsPage from './ToolCallsPage.svelte';

vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/activity') }
}));

const at = '2026-01-01T00:00:00Z';
const owned = { project_id: 'project', api_key_id: 'key', created_at: at };
const amounts = {
  observed_cost: '0',
  observed_charge: '0',
  remaining_cost: '0',
  remaining_charge: '0'
};
const options = {
  configureClient: (client: QueryClient) =>
    client.setDefaultOptions({ queries: { retry: false } })
};

const filteredLists = [
  {
    name: 'runs',
    keepsRowsWhileRefreshing: false,
    path: '/api/v1/agent-runs',
    mount: () => mountWithState(RunsPage, {}, options),
    row: (id: string) => ({ ...owned, id })
  },
  {
    name: 'tasks',
    keepsRowsWhileRefreshing: false,
    path: '/api/v1/a2a-tasks',
    mount: () => mountWithState(TasksPage, {}, options),
    row: (id: string) => ({ ...owned, id, state: 'completed' })
  },
  {
    name: 'investigations',
    keepsRowsWhileRefreshing: false,
    path: '/api/v1/investigations',
    mount: () => mountWithState(InvestigationsPage, {}, options),
    row: (id: string) => ({ ...owned, id, summary: id })
  },
  {
    name: 'delegations',
    keepsRowsWhileRefreshing: false,
    path: '/api/v1/agent-delegations',
    mount: () => mountWithState(DelegationsPage, {}, options),
    row: (id: string) => ({
      ...owned,
      id,
      agent_id: 'agent',
      budget: { daily: { accrued: '0' }, monthly: { accrued: '0' } },
      expires_at: at
    })
  },
  {
    name: 'tool calls',
    keepsRowsWhileRefreshing: true,
    path: '/api/v1/mcp-tool-calls',
    mount: () => mountWithState(ToolCallsPage, {}, options),
    row: (id: string) => ({
      ...owned,
      ...amounts,
      id,
      status: 'settled',
      debit_state: 'settled',
      identity_decisions: [],
      guardrail_decisions: [],
      started_at: at
    })
  },
  {
    name: 'provisional exposure',
    keepsRowsWhileRefreshing: true,
    path: '/api/v1/execution-debits',
    mount: () => mountWithState(ExposurePage, {}, options),
    row: (id: string) => ({ ...owned, ...amounts, id, state: 'provisional' })
  }
];

const lists = [
  ...filteredLists,
  {
    name: 'run spans',
    keepsRowsWhileRefreshing: false,
    path: '/api/v1/agent-runs/run/spans',
    mount: () => mountWithState(RunDetailPage, { runID: 'run' }, options),
    row: (id: string) => ({
      id,
      trace_id: id,
      span_id: 'span',
      category: 'model',
      status: 1,
      started_nanos: '0',
      ended_nanos: '0'
    })
  }
];

function openList({ path, row, mount }: (typeof lists)[number]) {
  const pageResponse = (cursor: string | null) =>
    json({
      items: [row(`item-${cursor ?? '1'}`)],
      next_cursor: String(Number(cursor ?? '1') + 1)
    });
  const respond = vi.fn((cursor: string | null): Response | Promise<Response> =>
    pageResponse(cursor)
  );
  serve((request) => {
    const url = new URL(request.url);
    if (url.pathname === path) return respond(url.searchParams.get('cursor'));
    return json(
      url.pathname === '/api/v1/agent-runs/run'
        ? { ...owned, steps: [], approvals: [] }
        : { items: [] }
    );
  });
  const { target } = mount();
  const shows = (text: string) =>
    eventually(() => expect(target.textContent).toContain(text));
  const next = async (item: string) => {
    await eventually(() =>
      expect(findButton(target, 'Next')?.disabled).toBe(false)
    );
    clickButton(target, 'Next');
    await shows(item);
  };
  return { target, respond, pageResponse, shows, next };
}

describe('agent list pages', () => {
  it.each(lists)(
    'steps through $name one page at a time and stops at a failed refresh',
    async (list) => {
      const { target, respond, pageResponse, shows, next } = openList(list);

      await shows('item-1');
      expect(findButton(target, 'Previous')?.disabled).toBe(true);
      await next('item-2');
      await next('item-3');
      expect(target.textContent).toContain('Page 3');

      const held = Promise.withResolvers<Response>();
      respond.mockImplementationOnce(() => held.promise);
      clickButton(target, 'Previous');
      await eventually(() => expect(respond).toHaveBeenLastCalledWith('2'));
      expect(target.textContent).toContain('Page 2');
      expect(findButton(target, 'Previous')?.disabled).toBe(false);
      expect(findButton(target, 'Next')?.disabled).toBe(true);
      held.resolve(pageResponse('2'));
      await shows('item-2');

      const refreshed = Promise.withResolvers<Response>();
      respond.mockImplementationOnce(() => refreshed.promise);
      await eventually(() =>
        expect(findButton(target, 'Refresh')?.disabled).toBe(false)
      );
      clickButton(target, 'Refresh');
      await eventually(() =>
        expect(findButton(target, 'Refresh')?.disabled).toBe(true)
      );
      expect(target.textContent?.includes('item-2')).toBe(
        list.keepsRowsWhileRefreshing
      );
      refreshed.resolve(pageResponse('2'));
      await shows('item-2');

      respond.mockImplementation(() =>
        json({ title: 'The list is unavailable.' }, 503)
      );
      await eventually(() =>
        expect(findButton(target, 'Refresh')?.disabled).toBe(false)
      );
      clickButton(target, 'Refresh');
      await shows('The list is unavailable.');
      expect(target.textContent).not.toContain('item-2');
      expect(findButton(target, 'Next')?.disabled).toBe(true);
      expect(findButton(target, 'Previous')?.disabled).toBe(false);
    }
  );

  it.each(filteredLists)(
    'returns $name to the first page when the filter is applied',
    async (list) => {
      const { target, respond, shows, next } = openList(list);

      await next('item-2');
      submitForm(target);

      await eventually(() => expect(respond).toHaveBeenLastCalledWith(null));
      await shows('item-1');
      expect(target.textContent).toContain('Page 1');
      expect(findButton(target, 'Previous')?.disabled).toBe(true);
    }
  );
});
