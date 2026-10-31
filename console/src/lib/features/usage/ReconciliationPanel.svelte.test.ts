import { QueryClient } from '@tanstack/svelte-query';
import { mount, unmount } from 'svelte';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api/reconciliation';
import * as providers from '$lib/features/providers/api/providers';
import ReconciliationProbe from './test/ReconciliationProbe.svelte';

vi.mock('./api/reconciliation', () => ({ getReconciliation: vi.fn() }));
vi.mock('$lib/features/providers/api/providers', () => ({
  listProviders: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ can: () => true })
}));

it('shows mixed currencies without formatting their summed invoice as money', async () => {
  vi.mocked(api.getReconciliation).mockResolvedValue({
    currency: 'USD',
    items: [
      {
        provider_id: 'provider',
        day: '2026-01-01',
        attributed_cost: '0.1',
        invoiced_cost: '9876.54',
        invoiced_currency: 'EUR,USD',
        capture_rate: null,
        capture_rate_unavailable_reason: 'currency_mismatch',
        unpriced_share: '0',
        provisional: false
      },
      {
        provider_id: 'provider',
        day: '2026-01-02',
        attributed_cost: '0.2',
        invoiced_cost: '12.34',
        invoiced_currency: 'USD',
        capture_rate: '0.5',
        capture_rate_unavailable_reason: null,
        unpriced_share: '0',
        provisional: false
      }
    ],
    totals: [
      {
        provider_id: 'provider',
        attributed_cost_covered: '0.3',
        invoiced_cost: '9888.88',
        capture_rate: null,
        capture_rate_unavailable_reason: 'currency_mismatch',
        unpriced_share: '0'
      }
    ]
  } as unknown as api.ReconciliationReport);
  vi.mocked(providers.listProviders).mockResolvedValue([
    { id: 'provider', name: 'Provider' }
  ] as Awaited<ReturnType<typeof providers.listProviders>>);
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(ReconciliationProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => expect(host.textContent).toContain('Provider'));
  const rows = host.querySelectorAll('tbody tr');
  expect(rows[1].textContent).toContain('EUR, USD');
  expect(rows[1].textContent).not.toContain('9,876.54');
  expect(rows[2].textContent).toContain('$12.34');
  expect(rows[3].textContent).toContain('Currency differs');
  expect(rows[3].textContent).not.toContain('9,888.88');
});
