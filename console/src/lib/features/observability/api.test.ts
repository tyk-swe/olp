import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { clearCsrfToken } from '$lib/features/access/session/csrf';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import {
  createCapturePolicy,
  createExportSink,
  deleteCapturePolicy,
  deleteExportSink,
  getCaptureConfiguration,
  listCapturePolicies,
  listCaptureSinkOptions,
  listExportSinks,
  listExportSinkGaps,
  updateCaptureConfiguration,
  updateCapturePolicy,
  updateExportSink,
  type CapturePolicy,
  type ExportSink
} from '$lib/features/observability/api';

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000401',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const,
    access_scope: 'global' as const,
    operations: []
  },
  csrf_token: 'csrf-observability-token'
};

const sink = {
  export_sink_id: '01980000-0000-7000-8000-000000000601',
  etag: '01980000-0000-7000-8000-000000000602'
} as ExportSink;

const policy = {
  id: '01980000-0000-7000-8000-000000000701',
  etag: '01980000-0000-7000-8000-000000000702'
} as CapturePolicy;

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('export sinks', () => {
  it('creates a sink with streams, format, filter and credential', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse(sink));

    await createExportSink({
      name: 'collector',
      type: 'https',
      destination: 'https://collector.example.com/olp',
      streams: ['requests', 'usage_rollups'],
      format: 'json',
      filter: { route: 'primary', outcome: 'success' },
      credential: { token: 'write-only' }
    });

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe('/api/v1/observability/sinks');
    expect(request.headers.get('idempotency-key')).toMatch(uuid);
    expect(await request.json()).toMatchObject({
      type: 'https',
      streams: ['requests', 'usage_rollups'],
      format: 'json',
      filter: { route: 'primary' }
    });
  });

  it('patches a sink under its ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ ...sink, enabled: false })
    );

    await updateExportSink(sink, { enabled: false });

    const request = requests[0]!;
    expect(request.method).toBe('PATCH');
    expect(request.headers.get('if-match')).toBe(`"${sink.etag}"`);
  });

  it('deletes a sink under its ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => new Response(null, { status: 204 }));

    await deleteExportSink(sink);

    expect(requests[0]!.method).toBe('DELETE');
    expect(requests[0]!.headers.get('if-match')).toBe(`"${sink.etag}"`);
  });

  it('lists sinks', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse({ items: [sink] }));

    const items = await listExportSinks();
    expect(items).toHaveLength(1);
    expect(new URL(requests[0]!.url).pathname).toBe(
      '/api/v1/observability/sinks'
    );
  });
});

describe('capture policies', () => {
  it('lists metadata-only sink options', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({
        items: [
          {
            export_sink_id: sink.export_sink_id,
            name: 'collector',
            type: 'https'
          }
        ]
      })
    );

    const options = await listCaptureSinkOptions();
    expect(options[0]!.export_sink_id).toBe(sink.export_sink_id);
    expect(new URL(requests[0]!.url).pathname).toBe(
      '/api/v1/observability/capture-sinks'
    );
  });

  it('creates a capture policy with exact sample ratio', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse(policy));

    await createCapturePolicy({
      project_id: '01980000-0000-7000-8000-000000000901',
      route_slug: 'primary',
      sink: sink.export_sink_id,
      sample_ratio: '0.25',
      include: ['input', 'output'],
      key_ids: ['01980000-0000-7000-8000-000000000903'],
      end_user_digests: ['digest-1'],
      max_bytes: 65536,
      enabled: true
    });

    const request = requests[0]!;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe(
      '/api/v1/observability/capture-policies'
    );
    expect(await request.json()).toMatchObject({
      route_slug: 'primary',
      sample_ratio: '0.25',
      include: ['input', 'output'],
      max_bytes: 65536
    });
  });

  it('patches and deletes a policy under its ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse(policy));

    await updateCapturePolicy(policy, { enabled: false });
    expect(requests[0]!.method).toBe('PATCH');
    expect(requests[0]!.headers.get('if-match')).toBe(`"${policy.etag}"`);

    await deleteCapturePolicy(policy);
    expect(requests[1]!.method).toBe('DELETE');
    expect(requests[1]!.headers.get('if-match')).toBe(`"${policy.etag}"`);
  });

  it('lists policies', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse({ items: [policy] }));
    const items = await listCapturePolicies();
    expect(items).toHaveLength(1);
    expect(new URL(requests[0]!.url).pathname).toBe(
      '/api/v1/observability/capture-policies'
    );
  });
});

describe('capture configuration', () => {
  it('toggles the owner-only master switch under its ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ enabled: true, etag: 'cap-etag-2' })
    );

    const config = await getCaptureConfiguration();
    await updateCaptureConfiguration(config, { enabled: true });

    const request = requests[1]!;
    expect(request.method).toBe('PATCH');
    expect(new URL(request.url).pathname).toBe('/api/v1/observability/capture');
    expect(request.headers.get('if-match')).toBeTruthy();
  });
});

describe('pagination', () => {
  it('collects every sinks page', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests((_request, index) =>
      index === 0
        ? jsonResponse({ items: [sink], next_cursor: 'cursor-2' })
        : jsonResponse({ items: [projectSinkRecord], next_cursor: null })
    );

    const items = await listExportSinks();
    expect(items).toHaveLength(2);
    expect(new URL(requests[1]!.url).searchParams.get('cursor')).toBe(
      'cursor-2'
    );
    expect(requests[1]!.signal).toBeInstanceOf(AbortSignal);
  });

  it('collects every capture-policy page', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests((_request, index) =>
      index === 0
        ? jsonResponse({ items: [policy], next_cursor: 'cursor-2' })
        : jsonResponse({ items: [], next_cursor: null })
    );

    const items = await listCapturePolicies();
    expect(items).toHaveLength(1);
    expect(new URL(requests[1]!.url).searchParams.get('cursor')).toBe(
      'cursor-2'
    );
  });

  it('collects every gaps page', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests((_request, index) =>
      index === 0
        ? jsonResponse({
            items: [{ id: 'g1' }],
            next_cursor: 'cursor-2'
          })
        : jsonResponse({ items: [{ id: 'g2' }], next_cursor: null })
    );

    const items = await listExportSinkGaps(sink.export_sink_id);
    expect(items).toHaveLength(2);
    expect(new URL(requests[1]!.url).searchParams.get('cursor')).toBe(
      'cursor-2'
    );
    expect(new URL(requests[0]!.url).pathname).toBe(
      `/api/v1/observability/sinks/${sink.export_sink_id}/gaps`
    );
  });
});

const projectSinkRecord = {
  ...sink,
  export_sink_id: '01980000-0000-7000-8000-000000000999',
  project_id: '01980000-0000-7000-8000-000000000901'
} as ExportSink;
