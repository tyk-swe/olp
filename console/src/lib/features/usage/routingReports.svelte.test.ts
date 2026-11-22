import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import { formatBudget, formatDate } from '#lib/format.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import ReconciliationPanel from './ReconciliationPanel.svelte';
import type { ReconciliationReport } from './reconciliation.js';
import RoutingReportsPage from './RoutingReportsPage.svelte';
import UsageDetails from './UsageDetails.svelte';

afterEach(() => vi.unstubAllGlobals());

it('shows effective key allowances and the state and reset of a weekly cap', () => {
  const target = document.createElement('div');
  const currency = 'USD';
  const daily = {
    accrued: '15',
    limit: '10',
    effective_limit: '20',
    window_ends_at: '2026-05-02T00:00:00Z'
  };
  const weekly = {
    accrued: '15',
    limit: '15',
    window_ends_at: '2026-05-04T00:00:00Z'
  };
  const monthly = {
    accrued: '15',
    limit: '10',
    effective_limit: '25',
    window_ends_at: '2026-06-01T00:00:00Z'
  };
  const component = mount(UsageDetails, {
    target,
    props: {
      apiKey: {
        id: 'key',
        name: 'Budgeted key',
        budget: { daily, weekly, monthly, unpriced_attempts: 0 }
      },
      summary: { currency },
      completeness: {
        complete: true,
        coverage: {
          approximate: false,
          range_complete: true,
          excluded_partial_aggregate_boundaries: 0
        },
        request_count: 1,
        priced_count: 1,
        unpriced_count: 0,
        incomplete_count: 0,
        request_metadata_gap_events: 0,
        uncertain_request_metadata_gap_count: 0,
        request_metadata_consumer: {
          state: 'healthy',
          lag_events: 0,
          pending_events: 0
        }
      }
    }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  const window = (name: string) =>
    [
      ...target.querySelectorAll('[aria-label="Filtered API key budget"] p')
    ].find((label) => label.textContent === `${name} accrued / limit`)
      ?.parentElement;

  for (const [name, amounts] of [
    ['Daily', daily],
    ['Monthly', monthly]
  ] as const) {
    expect(window(name)?.textContent).toContain(
      `${formatBudget(amounts.accrued, currency)} / ${formatBudget(amounts.effective_limit, currency)}`
    );
    expect(window(name)?.textContent).not.toContain(
      'Requests with this key are refused'
    );
  }
  expect(window('Weekly')?.textContent).toContain(
    `${formatBudget(weekly.accrued, currency)} / ${formatBudget(weekly.limit, currency)}`
  );
  expect(window('Weekly')?.textContent).toContain(
    `Window ends ${formatDate(weekly.window_ends_at)}`
  );
  expect(window('Weekly')?.textContent).toContain(
    'Requests with this key are refused'
  );
});

it('applies the same supported filters to both reports and distinguishes missing prices from zero savings', async () => {
  const queries = vi.fn<(path: string, query: URLSearchParams) => void>();
  serve(
    (request) => {
      const url = new URL(request.url);
      queries(url.pathname, url.searchParams);
      return json({
        currency: null,
        items: url.pathname.endsWith('selector-savings')
          ? [
              {
                route: 'route',
                selector: 'cost',
                attempts: 5,
                compared_attempts: 0,
                estimated_cost: '0',
                baseline_cost: '0',
                savings: '0'
              }
            ]
          : []
      });
    },
    ['read', 'usage']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(RoutingReportsPage, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  await vi.waitFor(() =>
    expect(target.querySelector('tbody tr')?.textContent).toContain('0 / 5')
  );
  expect(target.querySelector('tbody tr')?.textContent).toContain(
    'Unavailable'
  );
  const fields = target.querySelectorAll('input');
  fields[2].value = 'chosen-route';
  fields[2].dispatchEvent(new Event('input', { bubbles: true }));
  fields[3].value = 'chosen-project';
  fields[3].dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => {
    const applied = queries.mock.calls.filter(
      ([, query]) => query.get('route') === 'chosen-route'
    );
    expect(new Set(applied.map(([path]) => path))).toEqual(
      new Set([
        '/api/v1/usage/selector-savings',
        '/api/v1/usage/shadow-experiments'
      ])
    );
    for (const [, query] of applied) {
      expect(query.get('project_id')).toBe('chosen-project');
      expect(new Date(query.get('start') ?? '').getTime()).toBeLessThan(
        new Date(query.get('end') ?? '').getTime()
      );
    }
  });
});

it('shows the invoice currencies when a mixed-currency day has no total', async () => {
  const day = '2026-10-10';
  const end = '2026-10-11';
  const report: ReconciliationReport = {
    start: day,
    end,
    currency: 'USD',
    totals: [],
    items: [
      {
        provider_id: 'connection',
        day,
        attributed_cost: '42',
        invoiced_cost: null,
        invoiced_currency: 'EUR,USD',
        capture_rate: null,
        capture_rate_unavailable_reason: 'currency_mismatch',
        priced_attempts: 1,
        unpriced_attempts: 0,
        incomplete_attempts: 0,
        unpriced_share: '0',
        excluded_caller_attempts: 0,
        provisional: false,
        import: null
      }
    ]
  };
  serve(
    (request) =>
      json(
        new URL(request.url).pathname === '/api/v1/usage/reconciliation'
          ? report
          : { items: [] }
      ),
    ['read', 'usage']
  );
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(ReconciliationPanel, {
      target,
      context: getAllContexts(),
      props: { start: `${day}T00:00:00Z`, end: `${end}T00:00:00Z` }
    })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  await vi.waitFor(() => {
    const invoice = target.querySelector('tbody td:nth-child(3)');
    expect(invoice?.textContent).toBe('Multiple currencies (EUR, USD)');
  });
});
