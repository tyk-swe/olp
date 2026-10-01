import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { ApiProblem } from '$lib/api/http';
import { getApiKey, type ApiKey } from '$lib/features/access/api-keys/api';
import {
  usageBreakdown,
  usageCompleteness,
  usageSeries,
  usageSummary
} from './api/usage';
import { page } from './test/pageState.svelte';
import UsagePageProbe from './test/UsagePageProbe.svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', async () => await import('./test/pageState.svelte'));
vi.mock('$lib/features/access/api-keys/api', () => ({ getApiKey: vi.fn() }));
vi.mock('./api/usage', () => ({
  usageBreakdown: vi.fn(),
  usageCompleteness: vi.fn(),
  usageSeries: vi.fn(),
  usageSummary: vi.fn()
}));

const keyId = '01980000-0000-7000-8000-000000000103';
const window =
  'start=2026-07-11T09%3A00%3A00.000Z&end=2026-07-12T09%3A00%3A00.000Z';
const consumer = { state: 'healthy', lag_events: 0, pending_events: 0 };
const coverage = {
  approximate: false,
  excluded_partial_aggregate_boundaries: 0,
  range_complete: true
};
const counts = {
  complete: true,
  coverage,
  incomplete_count: 0,
  request_count: 42,
  request_metadata_consumer: consumer,
  request_metadata_gap_events: 0,
  uncertain_request_metadata_gap_count: 0,
  unpriced_count: 0
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(usageSummary).mockResolvedValue({
    ...counts,
    cache_write_1h_input_tokens: '0',
    cache_write_5m_input_tokens: '0',
    cache_write_input_tokens: '0',
    cached_input_tokens: '0',
    input_tokens: '10',
    media_units: '0',
    output_tokens: '20'
  });
  vi.mocked(usageSeries).mockResolvedValue({ items: [] } as never);
  vi.mocked(usageBreakdown).mockResolvedValue({ items: [] } as never);
  vi.mocked(usageCompleteness).mockResolvedValue({
    ...counts,
    priced_count: 42
  });
});

afterEach(async () => {
  vi.useRealTimers();
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

function render(search: string) {
  page.url = new URL(`https://console.test/usage?${search}`);
  component = mount(UsagePageProbe, { target: host, props: { client } });
  flushSync();
}

it('shows the report when the filtered API key cannot be read', async () => {
  vi.mocked(getApiKey).mockRejectedValue(
    new ApiProblem({ status: 404, title: 'Not Found' })
  );
  render(`${window}&api_key_id=${keyId}&dimension=route&granularity=hour`);
  await vi.waitFor(() =>
    expect(host.querySelector('[aria-label="Usage summary"]')).not.toBeNull()
  );
  expect(host.textContent).not.toContain('Usage could not be loaded.');
  expect(
    host.querySelector('[aria-label="Filtered API key budget"]')
  ).toBeNull();
});

it('shows the filtered API key budget when the key is readable', async () => {
  const window_ = { accrued: '0', limit: null, window_ends_at: null };
  vi.mocked(getApiKey).mockResolvedValue({
    id: keyId,
    name: 'Billing key',
    budget: { daily: window_, monthly: window_, unpriced_attempts: 0 }
  } as unknown as ApiKey);
  render(`${window}&api_key_id=${keyId}&dimension=route&granularity=hour`);
  await vi.waitFor(() =>
    expect(
      host.querySelector('[aria-label="Filtered API key budget"]')
    ).not.toBeNull()
  );
  expect(host.textContent).toContain('Billing key');
});

it('takes a fresh default window when the URL returns to bare /usage', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-07-12T09:00:00Z'));
  render(`${window}&dimension=route&granularity=hour`);
  expect(goto).not.toHaveBeenCalled();
  vi.setSystemTime(new Date('2026-07-12T15:00:00Z'));
  page.url = new URL('https://console.test/usage');
  flushSync();
  expect(goto).toHaveBeenLastCalledWith(
    expect.stringContaining(
      'start=2026-07-11T15%3A00%3A00.000Z&end=2026-07-12T15%3A00%3A00.000Z'
    ),
    expect.objectContaining({ replaceState: true })
  );
});
