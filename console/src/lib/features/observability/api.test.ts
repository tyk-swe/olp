import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, expect, it, vi } from 'vitest';
import { json, route, serve } from '$lib/management/test/transport';
import {
  createCapturePolicy,
  createExportSink,
  deleteCapturePolicy,
  deleteExportSink,
  getCaptureConfiguration,
  listCapturePolicies,
  listCaptureRedactorOptions,
  listCaptureSinkOptions,
  listExportSinkFocusDatasets,
  listExportSinkGaps,
  listExportSinks,
  updateCaptureConfiguration,
  updateCapturePolicy,
  updateExportSink,
  type CapturePolicy,
  type CreateCapturePolicyInput,
  type CreateExportSinkInput,
  type ExportSink,
  type FocusDataset
} from './api';
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

it.each([
  {
    name: 'export sinks',
    path: '/api/v1/observability/sinks',
    limit: '50',
    load: (signal: AbortSignal) => listExportSinks(signal)
  },
  {
    name: 'export gaps',
    path: '/api/v1/observability/sinks/sink-1/gaps',
    limit: '50',
    load: (signal: AbortSignal) => listExportSinkGaps('sink-1', signal)
  },
  {
    name: 'capture policies',
    path: '/api/v1/observability/capture-policies',
    limit: '50',
    load: (signal: AbortSignal) => listCapturePolicies(signal)
  },
  {
    name: 'capture redactors',
    path: '/api/v1/guardrails',
    limit: '100',
    load: (signal: AbortSignal) => listCaptureRedactorOptions(signal)
  }
])('loads all pages of $name', async ({ path, limit, load }) => {
  const first = { id: 'first' };
  const second = { id: 'second' };
  const fetch = vi.fn(async (request: Request) => {
    route(request, 'GET', path);
    const query = new URL(request.url).searchParams;
    expect(query.get('limit')).toBe(limit);
    return query.has('cursor')
      ? json({ items: [second], next_cursor: null })
      : json({ items: [first], next_cursor: 'next' });
  });
  serve(fetch);

  await expect(load(new AbortController().signal)).resolves.toEqual([
    first,
    second
  ]);
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(new URL(fetch.mock.calls[1][0].url).searchParams.get('cursor')).toBe(
    'next'
  );
});

it('forwards cancellation while loading subsequent sink pages', async () => {
  const controller = new AbortController();
  serve(async (request) => {
    route(request, 'GET', '/api/v1/observability/sinks');
    if (new URL(request.url).searchParams.has('cursor')) {
      expect(request.signal.aborted).toBe(true);
      throw new DOMException('Request aborted', 'AbortError');
    }
    controller.abort();
    return json({ items: [], next_cursor: 'next' });
  });
  await expect(listExportSinks(controller.signal)).rejects.toMatchObject({
    name: 'AbortError'
  });
});

it('creates, updates and deletes a sink using the request identity and revision', async () => {
  const input: CreateExportSinkInput = {
    name: 'Delivery',
    type: 'https',
    destination: 'https://example.test/events',
    streams: ['audit'],
    format: 'json'
  };
  const sink: ExportSink = {
    ...input,
    export_sink_id: 'sink-1',
    project_id: null,
    filter: null,
    enabled: true,
    credential_configured: false,
    etag: 'revision-1',
    status: []
  };
  const fetch = vi.fn(async (request: Request) => {
    if (request.method === 'POST') {
      route(request, 'POST', '/api/v1/observability/sinks');
      expect(request.headers.get('Idempotency-Key')).toBe('request-1');
      expect(await request.json()).toEqual(input);
      return json(sink);
    }
    route(request, request.method, '/api/v1/observability/sinks/sink-1');
    expect(request.headers.get('If-Match')).toBe(sink.etag);
    if (request.method === 'PATCH') {
      expect(await request.json()).toEqual({ enabled: false });
      return json({ ...sink, enabled: false });
    }
    expect(request.method).toBe('DELETE');
    return new Response(null, { status: 204 });
  });
  serve(fetch);

  await expect(createExportSink(input, 'request-1')).resolves.toEqual(sink);
  await expect(updateExportSink(sink, { enabled: false })).resolves.toEqual({
    ...sink,
    enabled: false
  });
  await expect(deleteExportSink(sink)).resolves.toBeUndefined();
  expect(fetch).toHaveBeenCalledTimes(3);
});

it('creates, updates and deletes a capture policy using its revision', async () => {
  const input: CreateCapturePolicyInput = {
    sink: 'sink-1',
    sample_ratio: '0.5',
    include: ['input'],
    max_bytes: 1024
  };
  const policy: CapturePolicy = {
    ...input,
    id: 'policy-1',
    project_id: null,
    route_slug: null,
    redact: [],
    key_ids: [],
    end_user_digests: [],
    enabled: true,
    etag: 'revision-1'
  };
  const fetch = vi.fn(async (request: Request) => {
    if (request.method === 'POST') {
      route(request, 'POST', '/api/v1/observability/capture-policies');
      expect(request.headers.get('Idempotency-Key')).toBe('request-2');
      expect(await request.json()).toEqual(input);
      return json(policy);
    }
    route(
      request,
      request.method,
      '/api/v1/observability/capture-policies/policy-1'
    );
    expect(request.headers.get('If-Match')).toBe(policy.etag);
    if (request.method === 'PATCH') {
      expect(await request.json()).toEqual({ enabled: false });
      return json({ ...policy, enabled: false });
    }
    expect(request.method).toBe('DELETE');
    return new Response(null, { status: 204 });
  });
  serve(fetch);

  await expect(createCapturePolicy(input, 'request-2')).resolves.toEqual(
    policy
  );
  await expect(
    updateCapturePolicy(policy, { enabled: false })
  ).resolves.toEqual({
    ...policy,
    enabled: false
  });
  await expect(deleteCapturePolicy(policy)).resolves.toBeUndefined();
  expect(fetch).toHaveBeenCalledTimes(3);
});

it('loads and updates the capture switch using its current revision', async () => {
  const configuration = { enabled: false, etag: 'revision-1' };
  const fetch = vi.fn(async (request: Request) => {
    route(request, request.method, '/api/v1/observability/capture');
    if (request.method === 'GET') return json(configuration);
    expect(request.method).toBe('PATCH');
    expect(request.headers.get('If-Match')).toBe(configuration.etag);
    expect(await request.json()).toEqual({ enabled: true });
    return json({ ...configuration, enabled: true });
  });
  serve(fetch);

  await expect(getCaptureConfiguration()).resolves.toEqual(configuration);
  await expect(
    updateCaptureConfiguration(configuration, { enabled: true })
  ).resolves.toEqual({ ...configuration, enabled: true });
});

it('handles an empty set of capture sink options', async () => {
  serve(async (request) => {
    route(request, 'GET', '/api/v1/observability/capture-sinks');
    return json({});
  });
  await expect(listCaptureSinkOptions()).resolves.toEqual([]);
});
