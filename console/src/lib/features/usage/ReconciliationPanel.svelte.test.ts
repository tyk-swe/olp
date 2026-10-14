// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { operationsFor } from '$lib/features/access/session/test/grants';
import { getReconciliation } from '$lib/features/usage/api/reconciliation';
import { listProviders } from '$lib/features/providers/api/providers';
import ReconciliationProbe from './test/ReconciliationProbe.svelte';
import type { ReconciliationReport } from '$lib/features/usage/api/reconciliation';

vi.mock('$lib/features/usage/api/reconciliation', () => ({
  getReconciliation: vi.fn()
}));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviders: vi.fn()
}));

const role = vi.hoisted(() => ({
  current: 'owner' as 'owner' | 'operator' | 'developer' | 'viewer',
  global: true,
  ops: null as string[] | null
}));
vi.mock('$lib/features/access/session/useRole.svelte', async () => {
  const { can, allows } =
    await import('$lib/features/access/session/authorization');
  return {
    useRole: () => {
      const grant = {
        operations: (role.ops ??
          operationsFor(role.current, role.global)) as never[],
        access_scope: (role.global ? 'global' : 'assigned') as
          'global' | 'assigned'
      };
      return {
        role: role.current,
        globalScope: role.global,
        user: { role: role.current, access_scope: 'global' },
        can: (capability: never) => can(grant, capability),
        allows: (route: never) => allows(grant, route)
      };
    }
  };
});

const providerId = '01980000-0000-7000-8000-000000000401';

const report: ReconciliationReport = {
  start: '2026-10-01',
  end: '2026-10-03',
  currency: 'USD',
  items: [
    {
      provider_id: providerId,
      day: '2026-10-01',
      attributed_cost: '10.5',
      invoiced_cost: '10.0',
      invoiced_currency: 'USD',
      capture_rate: '1.05',
      capture_rate_unavailable_reason: null,
      priced_attempts: 4,
      unpriced_attempts: 0,
      incomplete_attempts: 0,
      unpriced_share: '0',
      excluded_caller_attempts: 1,
      provisional: true,
      import: { last_success_at: '2026-10-02T01:00:00Z', last_error_code: null }
    },
    {
      provider_id: providerId,
      day: '2026-10-02',
      attributed_cost: '3',
      invoiced_cost: null,
      invoiced_currency: null,
      capture_rate: null,
      capture_rate_unavailable_reason: 'not_imported',
      priced_attempts: 1,
      unpriced_attempts: 1,
      incomplete_attempts: 0,
      unpriced_share: '0.5',
      excluded_caller_attempts: 0,
      provisional: false,
      import: null
    }
  ],
  totals: [
    {
      provider_id: providerId,
      attributed_cost: '13.5',
      attributed_cost_covered: '10.5',
      invoiced_cost: '10.0',
      capture_rate: '1.05',
      capture_rate_unavailable_reason: null,
      unpriced_share: '0.2'
    }
  ]
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.clearAllMocks();
  role.current = 'owner';
  role.global = true;
  role.ops = null;
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(getReconciliation).mockResolvedValue(report);
  vi.mocked(listProviders).mockResolvedValue([
    {
      id: providerId,
      name: 'OpenAI connection'
    } as never
  ]);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

async function render(start: string, end: string) {
  component = mount(ReconciliationProbe, {
    target: host,
    props: { client, start, end }
  });
  flushSync();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Billing reconciliation')
  );
  await vi.waitFor(() =>
    expect(vi.mocked(getReconciliation)).toHaveBeenCalled()
  );
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

it('renders provider groups with day rows, percents and totals', async () => {
  await render('2026-10-01T00:00:00Z', '2026-10-03T00:00:00Z');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('OpenAI connection')
  );

  const text = host.textContent!;
  expect(text).toContain('2026-10-01');
  expect(text).toContain('105.0%');
  expect(text).toContain('50.0%');
  expect(text).toContain('Provisional');
  expect(text).toContain('Not imported');
  expect(text).toContain('Totals');
  expect(text).toContain('$10.50');
  expect(getReconciliation).toHaveBeenCalledWith(
    '2026-10-01',
    '2026-10-03',
    undefined,
    expect.any(AbortSignal)
  );
});

it('clamps the range to 93 days with a note', async () => {
  await render('2025-01-01T00:00:00Z', '2026-10-03T00:00:00Z');

  const call = vi.mocked(getReconciliation).mock.calls[0]!;
  const spanDays = (Date.parse(call[1]) - Date.parse(call[0])) / 86_400_000;
  expect(spanDays).toBe(93);
  expect(host.textContent).toContain('clamped');
});

it('renders the empty state when nothing is imported', async () => {
  vi.mocked(getReconciliation).mockResolvedValue({
    ...report,
    items: [],
    totals: []
  });
  await render('2026-10-01T00:00:00Z', '2026-10-03T00:00:00Z');

  await vi.waitFor(() =>
    expect(host.textContent).toContain(
      'Configure a billing import on an OpenAI or Anthropic connection'
    )
  );
});

it('hides the panel for principals without the usage grant', async () => {
  role.ops = [];
  component = mount(ReconciliationProbe, {
    target: host,
    props: {
      client,
      start: '2026-10-01T00:00:00Z',
      end: '2026-10-03T00:00:00Z'
    }
  });
  flushSync();
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
  expect(host.textContent).not.toContain('Billing reconciliation');
  expect(getReconciliation).not.toHaveBeenCalled();
});
