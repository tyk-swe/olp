import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import ReconciliationPanel from './ReconciliationPanel.svelte';
import type { ReconciliationReport } from './reconciliation.js';
import RoutingReportsPage from './RoutingReportsPage.svelte';

afterEach(() => vi.unstubAllGlobals());

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
