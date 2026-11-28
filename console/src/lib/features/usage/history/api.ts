import type { RequestFilters } from '#lib/domain/requests/api.js';
import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { apiProblem, unwrap } from '#lib/management/http.js';
import { compactQuery } from '#lib/management/query.js';

export type RequestDetail = components['schemas']['RequestDetailResponse'];
export type RequestAttempt = components['schemas']['AttemptResponse'];

/** Every gateway operation kind a request can record. */
export const operationKinds = [
  'generation',
  'embeddings',
  'rerank',
  'token_count',
  'image_generation',
  'image_edit',
  'image_variation',
  'speech',
  'transcription',
  'translation',
  'video_create',
  'video_list',
  'video_get',
  'video_content',
  'video_delete',
  'ocr',
  'search',
  'moderation',
  'model_list',
  'model_get',
  'batch',
  'fine_tuning',
  'vector_store',
  'conversation',
  'container',
  'eval',
  'skill',
  'context_cache',
  'retrieval',
  'realtime',
  'bedrock_invoke',
  'passthrough'
] as const;

export async function getRequest(requestId: string): Promise<RequestDetail> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/requests/{request_id}',
    { params: { path: { request_id: requestId } } }
  );
  return unwrap({ data, error, response });
}

export async function exportRequestsCsv(
  filters: Omit<RequestFilters, 'cursor' | 'limit'>
): Promise<string> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/requests/export.csv',
    { params: { query: compactQuery(filters) }, parseAs: 'text' }
  );
  if (!response.ok) throw apiProblem(error, response);
  return data as string;
}
