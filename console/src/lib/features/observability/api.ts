import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

type Schemas = components['schemas'];

export type ExportSink = Schemas['ExportSink'];
export type ExportSinkGap = Schemas['ExportSinkGap'];
export type CreateExportSinkInput = Schemas['CreateExportSinkRequest'];
export type UpdateExportSinkInput = Schemas['UpdateExportSinkRequest'];
export type CaptureSinkOption = Schemas['CaptureSinkOption'];
export type CapturePolicy = Schemas['CapturePolicy'];
export type CreateCapturePolicyInput = Schemas['CreateCapturePolicyRequest'];
export type UpdateCapturePolicyInput = Schemas['UpdateCapturePolicyRequest'];
export type CaptureConfiguration = Schemas['CaptureConfiguration'];

export async function listExportSinks(
  signal?: AbortSignal
): Promise<ExportSink[]> {
  return collectCursorPages((cursor) => listExportSinkPage(cursor, signal));
}

async function listExportSinkPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<import('$lib/api/http').CursorPage<ExportSink>> {
  const response = await apiClient.GET('/api/v1/observability/sinks', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  return unwrapPage(response);
}

export async function createExportSink(
  input: CreateExportSinkInput
): Promise<ExportSink> {
  const response = await apiClient.POST('/api/v1/observability/sinks', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return unwrap(response);
}

export async function updateExportSink(
  sink: ExportSink,
  input: UpdateExportSinkInput
): Promise<ExportSink> {
  const response = await apiClient.PATCH(
    '/api/v1/observability/sinks/{export_sink_id}',
    {
      params: {
        path: { export_sink_id: sink.export_sink_id },
        header: { 'If-Match': sink.etag }
      },
      body: input
    }
  );
  return unwrap(response);
}

export async function deleteExportSink(sink: ExportSink): Promise<void> {
  const response = await apiClient.DELETE(
    '/api/v1/observability/sinks/{export_sink_id}',
    {
      params: {
        path: { export_sink_id: sink.export_sink_id },
        header: { 'If-Match': sink.etag }
      }
    }
  );
  ensureOk(response);
}

export async function listExportSinkGaps(
  sinkId: string,
  signal?: AbortSignal
): Promise<ExportSinkGap[]> {
  return collectCursorPages((cursor) =>
    listExportSinkGapPage(sinkId, cursor, signal)
  );
}

async function listExportSinkGapPage(
  sinkId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<import('$lib/api/http').CursorPage<ExportSinkGap>> {
  const response = await apiClient.GET(
    '/api/v1/observability/sinks/{export_sink_id}/gaps',
    {
      params: {
        path: { export_sink_id: sinkId },
        query: { limit: 50, cursor }
      },
      signal
    }
  );
  return unwrapPage(response);
}

export async function getCaptureConfiguration(
  signal?: AbortSignal
): Promise<CaptureConfiguration> {
  const response = await apiClient.GET('/api/v1/observability/capture', {
    signal
  });
  return unwrap(response);
}

export async function updateCaptureConfiguration(
  configuration: CaptureConfiguration,
  input: Schemas['UpdateCaptureConfigurationRequest']
): Promise<CaptureConfiguration> {
  const response = await apiClient.PATCH('/api/v1/observability/capture', {
    params: { header: { 'If-Match': configuration.etag } },
    body: input
  });
  return unwrap(response);
}

export async function listCapturePolicies(
  signal?: AbortSignal
): Promise<CapturePolicy[]> {
  return collectCursorPages((cursor) => listCapturePolicyPage(cursor, signal));
}

async function listCapturePolicyPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<import('$lib/api/http').CursorPage<CapturePolicy>> {
  const response = await apiClient.GET(
    '/api/v1/observability/capture-policies',
    {
      params: { query: { limit: 50, cursor } },
      signal
    }
  );
  return unwrapPage(response);
}

export async function listCaptureSinkOptions(
  signal?: AbortSignal
): Promise<CaptureSinkOption[]> {
  const response = await apiClient.GET('/api/v1/observability/capture-sinks', {
    signal
  });
  return unwrap(response).items ?? [];
}

export async function createCapturePolicy(
  input: CreateCapturePolicyInput
): Promise<CapturePolicy> {
  const response = await apiClient.POST(
    '/api/v1/observability/capture-policies',
    {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body: input
    }
  );
  return unwrap(response);
}

export async function updateCapturePolicy(
  policy: CapturePolicy,
  input: UpdateCapturePolicyInput
): Promise<CapturePolicy> {
  const response = await apiClient.PATCH(
    '/api/v1/observability/capture-policies/{capture_policy_id}',
    {
      params: {
        path: { capture_policy_id: policy.id },
        header: { 'If-Match': policy.etag }
      },
      body: input
    }
  );
  return unwrap(response);
}

export async function deleteCapturePolicy(
  policy: CapturePolicy
): Promise<void> {
  const response = await apiClient.DELETE(
    '/api/v1/observability/capture-policies/{capture_policy_id}',
    {
      params: {
        path: { capture_policy_id: policy.id },
        header: { 'If-Match': policy.etag }
      }
    }
  );
  ensureOk(response);
}
