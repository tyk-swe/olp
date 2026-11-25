import { apiClient } from '#lib/session/client.js';
import {
  createHeaders,
  ensureOk,
  mutationHeaders,
  unwrap,
  unwrapPage
} from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';
import type { components } from '#lib/management/schema.js';
export type Sink = components['schemas']['SinkResponse'];
export type SinkInput = components['schemas']['CreateSinkRequest'];
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
        params: { header: createHeaders(requestId) }
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
        header: mutationHeaders(current.etag, requestId)
      }
    })
  );
}
export const retireSink = async (value: Sink, requestId: string) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/sinks/{sink_id}', {
      params: {
        path: { sink_id: value.id },
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
