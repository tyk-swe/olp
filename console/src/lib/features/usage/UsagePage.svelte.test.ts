import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { goto } from '$app/navigation';
import { ApiProblem } from '$lib/api/http';
import { getApiKey, type ApiKey } from '$lib/features/access/api-keys/api';
import {
  exportUsageCsv,
  usageBreakdown,
  usageCompleteness,
  usageSeries,
  usageSummary
} from './api/usage';
vi.mock('$lib/download', () => ({ downloadBlob: vi.fn() }));
import { page } from './test/pageState.svelte';
import UsagePageProbe from './test/UsagePageProbe.svelte';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', async () => await import('./test/pageState.svelte'));
vi.mock('$lib/features/access/api-keys/api', () => ({ getApiKey: vi.fn() }));
vi.mock('./api/usage', () => ({
  usageBreakdown: vi.fn(),
  usageCompleteness: vi.fn(),
  usageSeries: vi.fn(),
  usageSummary: vi.fn(),
  exportUsageCsv: vi.fn()
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

// The totals every report carries about admission estimates; zero attempts
// had both an estimate and reported usage.
const unestimated = {
  estimated_attempt_count: 0,
  estimated_input_tokens: '0',
  reported_input_tokens: '0'
};

beforeEach(() => {
  vi.resetAllMocks();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(usageSummary).mockResolvedValue({
    ...counts,
    ...unestimated,
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

function metric(label: string): HTMLElement {
  const card = [
    ...host.querySelectorAll<HTMLElement>('.usage-metrics article')
  ].find((article) => article.querySelector('p')?.textContent === label);
  if (!card) throw new Error(`no ${label} card`);
  return card;
}

it('shows no estimation error when no attempt had both an estimate and usage', async () => {
  render(`${window}&dimension=route&granularity=hour`);
  await vi.waitFor(() =>
    expect(host.querySelector('[aria-label="Usage summary"]')).not.toBeNull()
  );
  const card = metric('Input estimate error');
  expect(card.querySelector('strong')?.textContent).toBe('—');
  expect(card.textContent).toContain(
    'No attempt had both an estimate and reported input usage.'
  );
});

it('shows the signed estimation error with the sums it is drawn from', async () => {
  vi.mocked(usageSummary).mockResolvedValue({
    ...counts,
    cache_write_1h_input_tokens: '0',
    cache_write_5m_input_tokens: '0',
    cache_write_input_tokens: '0',
    cached_input_tokens: '0',
    input_tokens: '5000',
    media_units: '0',
    output_tokens: '20',
    estimated_attempt_count: 12,
    estimated_input_tokens: '1032',
    reported_input_tokens: '1000'
  });
  render(`${window}&dimension=route&granularity=hour`);
  await vi.waitFor(() =>
    expect(host.querySelector('[aria-label="Usage summary"]')).not.toBeNull()
  );
  const card = metric('Input estimate error');
  expect(card.querySelector('strong')?.textContent).toBe('+3.2%');
  expect(card.textContent).toContain('Admission over-estimated input.');
  expect(card.textContent).toContain(
    '1,032 / 1,000 estimated / reported input across 12 attempts'
  );
});

it('does not claim an empty sample when the providers reported no input', async () => {
  vi.mocked(usageSummary).mockResolvedValue({
    ...counts,
    cache_write_1h_input_tokens: '0',
    cache_write_5m_input_tokens: '0',
    cache_write_input_tokens: '0',
    cached_input_tokens: '0',
    input_tokens: '0',
    media_units: '0',
    output_tokens: '20',
    estimated_attempt_count: 2,
    estimated_input_tokens: '50',
    reported_input_tokens: '0'
  });
  render(`${window}&dimension=route&granularity=hour`);
  await vi.waitFor(() =>
    expect(host.querySelector('[aria-label="Usage summary"]')).not.toBeNull()
  );
  const card = metric('Input estimate error');
  expect(card.querySelector('strong')?.textContent).toBe('—');
  expect(card.textContent).toContain(
    'The providers reported no input tokens for these attempts.'
  );
  expect(card.textContent).toContain('50 / 0 estimated / reported input');
  expect(card.textContent).not.toContain('No attempt');
});

it('offers the model family and estimate provenance breakdowns', async () => {
  render(`${window}&dimension=model_family&granularity=hour`);
  await vi.waitFor(() =>
    expect(host.querySelector('[aria-label="Usage summary"]')).not.toBeNull()
  );
  const select = host.querySelector<HTMLSelectElement>(
    'select option[value="model_family"]'
  )?.parentElement as HTMLSelectElement;
  expect([...select.options].map((option) => option.value)).toEqual([
    'route',
    'provider',
    'end_user',
    'model',
    'model_family',
    'estimate_provenance',
    'api_key',
    'operation',
    'attribution',
    'project',
    'session'
  ]);
  expect(select.value).toBe('model_family');
  expect(usageBreakdown).toHaveBeenCalledWith(
    expect.anything(),
    'model_family'
  );
  expect(host.querySelector('#breakdown-title')?.textContent?.trim()).toBe(
    'By model family'
  );
});

it('sets estimated against reported input in each breakdown row', async () => {
  const row = {
    ...counts,
    dimension: 'openai-o200k',
    cache_write_1h_input_tokens: '0',
    cache_write_5m_input_tokens: '0',
    cache_write_input_tokens: '0',
    cached_input_tokens: '0',
    input_tokens: '9000',
    media_units: '0',
    output_tokens: '100',
    estimated_attempt_count: 4,
    estimated_input_tokens: '9600',
    reported_input_tokens: '10000'
  };
  vi.mocked(usageBreakdown).mockResolvedValue({
    items: [
      row,
      { ...row, ...unestimated, dimension: 'unknown', input_tokens: '30' }
    ]
  } as never);
  render(`${window}&dimension=model_family&granularity=hour`);
  await vi.waitFor(() =>
    expect(host.querySelector('table.data-table')).not.toBeNull()
  );
  const headers = [...host.querySelectorAll('thead th')].map(
    (header) => header.textContent
  );
  expect(headers).toContain('Estimated / reported input');
  expect(headers).toContain('Estimate error');
  const cells = (index: number) =>
    [...host.querySelectorAll('tbody tr')[index].querySelectorAll('td')].map(
      (cell) => cell.textContent?.trim()
    );
  const estimated = headers.indexOf('Estimated / reported input');
  expect(cells(0)[0]).toBe('openai-o200k');
  expect(cells(0)[estimated]).toBe('9,600 / 10,000');
  expect(cells(0)[estimated + 1]).toBe('-4.0%');
  expect(cells(1)[0]).toBe('unknown');
  expect(cells(1)[estimated]).toBe('—');
  expect(cells(1)[estimated + 1]).toBe('—');
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

it('exports the applied filters and current dimension as CSV', async () => {
  const { downloadBlob } = await import('$lib/download');
  vi.mocked(exportUsageCsv).mockResolvedValue('row_type,count\nsummary,1\n');
  render(`${window}&dimension=session&session_id=SessionCase`);
  await vi.waitFor(() =>
    expect(host.querySelector('[aria-label="Usage summary"]')).not.toBeNull()
  );

  const button = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (candidate) => candidate.textContent === 'Export CSV'
  )!;
  button.click();
  await vi.waitFor(() => expect(vi.mocked(exportUsageCsv)).toHaveBeenCalled());

  expect(exportUsageCsv).toHaveBeenCalledWith(
    expect.objectContaining({ session_id: 'SessionCase' }),
    'session'
  );
  expect(vi.mocked(downloadBlob)).toHaveBeenCalled();
});

it('surfaces an export failure without downloading', async () => {
  const { downloadBlob } = await import('$lib/download');
  vi.mocked(exportUsageCsv).mockRejectedValue(
    new ApiProblem({
      type: 'urn:olp:problem:export_too_large',
      title: 'The export is too large',
      status: 422
    })
  );
  render(`${window}&dimension=route`);
  await vi.waitFor(() =>
    expect(host.querySelector('[aria-label="Usage summary"]')).not.toBeNull()
  );

  const button = [...host.querySelectorAll<HTMLButtonElement>('button')].find(
    (candidate) => candidate.textContent === 'Export CSV'
  )!;
  button.click();
  await vi.waitFor(() =>
    expect(host.textContent).toContain('The export is too large')
  );
  expect(vi.mocked(downloadBlob)).not.toHaveBeenCalled();
});
