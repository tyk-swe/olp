import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, expect, it, vi } from 'vitest';
import { listExportSinkFocusDatasets, type FocusDataset } from './api';
import { observabilityKeys } from './observabilityKeys';

const dataset: FocusDataset = {
  charge_date: '2026-10-01',
  revision: 1,
  generated_at: null,
  delivered_at: null,
  row_count: null,
  part_count: null,
  billed_cost: null,
  effective_cost: null,
  attempts: 0,
  last_error_code: null,
  current: false,
  commitment_totals: null
};

afterEach(() => vi.unstubAllGlobals());

it('loads one bounded dataset page and preserves its continuation', async () => {
  const cursor = 'older-date';
  const fetch = vi.fn(async (request: Request) => {
    const url = new URL(request.url);
    expect(url.pathname).toBe(
      '/api/v1/observability/sinks/sink/focus-datasets'
    );
    expect(url.searchParams.get('limit')).toBe('50');
    expect(request.method).toBe('GET');
    expect(request.signal.aborted).toBe(false);
    return Response.json({
      items: [dataset],
      next_cursor: url.searchParams.has('cursor') ? null : cursor
    });
  });
  vi.stubGlobal('fetch', fetch);
  const first = await listExportSinkFocusDatasets('sink');
  expect(first).toEqual({ items: [dataset], nextCursor: cursor });
  expect(fetch).toHaveBeenCalledTimes(1);
  const last = await listExportSinkFocusDatasets('sink', first.nextCursor!);
  expect(new URL(fetch.mock.calls[1][0].url).searchParams.get('cursor')).toBe(
    cursor
  );
  expect(last).toEqual({ items: [dataset], nextCursor: null });
  expect(fetch).toHaveBeenCalledTimes(2);
});

it('surfaces failed dataset pages instead of returning an empty history', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () =>
      Response.json(
        { title: 'Dataset unavailable', status: 500 },
        { status: 500 }
      )
    )
  );
  await expect(
    listExportSinkFocusDatasets('sink', 'older-date')
  ).rejects.toThrow('Dataset unavailable');
});

it('caches dataset pages independently for each sink and cursor', async () => {
  const client = new QueryClient();
  const load = (sink: string, cursor?: string) =>
    client.fetchQuery({
      queryKey: observabilityKeys.focusDatasets(sink, cursor),
      queryFn: async () => ({ sink, cursor }),
      staleTime: Infinity
    });
  try {
    expect(await load('first')).toEqual({ sink: 'first', cursor: undefined });
    expect(await load('first', 'older')).toEqual({
      sink: 'first',
      cursor: 'older'
    });
    expect(await load('second')).toEqual({ sink: 'second', cursor: undefined });
    expect(await load('first')).toEqual({ sink: 'first', cursor: undefined });
  } finally {
    client.clear();
  }
});
