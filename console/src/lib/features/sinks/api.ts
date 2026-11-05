import { apiClient } from '$lib/management/client';
import { ensureOk, unwrap, unwrapPage } from '$lib/management/http';
import { collectCursorPages } from '$lib/management/pagination';
import type { components } from '$lib/management/schema';
export type Sink = components['schemas']['SinkResponse'];
export type SinkInput = components['schemas']['CreateSinkRequest'];
export const sinkKeys = { all: ['signed-fact-sinks'] as const };
export const listSinks = () =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/sinks', { params: { query: { cursor } } })
    )
  );
export async function saveSink(
  current: Sink | null,
  body: SinkInput,
  requestId: string
) {
  if (!current)
    return unwrap(
      await apiClient.POST('/api/v1/sinks', {
        body,
        params: { header: { 'Idempotency-Key': requestId } }
      })
    );
  const { project_id, type, ...update } = body;
  void project_id;
  void type;
  return unwrap(
    await apiClient.PUT('/api/v1/sinks/{sink_id}', {
      body: update,
      params: {
        path: { sink_id: current.id },
        header: {
          'If-Match': `"${current.etag}"`,
          'Idempotency-Key': requestId
        }
      }
    })
  );
}
export const retireSink = async (value: Sink, requestId: string) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/sinks/{sink_id}', {
      params: {
        path: { sink_id: value.id },
        header: { 'If-Match': `"${value.etag}"`, 'Idempotency-Key': requestId }
      }
    })
  );
