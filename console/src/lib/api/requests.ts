import type { components } from './schema';
import { apiClient } from './client';
import { pageResult, result } from './http';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from './query';

export type RequestSummary = components['schemas']['RequestSummary'];
export type RequestDetail = components['schemas']['RequestDetailResponse'];
export type RequestAttempt = components['schemas']['AttemptResponse'];

/** Every gateway operation kind a request can record. */
export const operationKinds = [
  'generation',
  'embeddings',
  'token_count',
  'image_generation',
  'image_edit',
  'image_variation',
  'speech',
  'transcription',
  'video_create',
  'video_list',
  'video_get',
  'video_content',
  'video_delete',
  'moderation',
  'model_list',
  'model_get'
] as const;

export type RequestFilters = {
  cursor?: string;
  limit?: number;
  route?: string;
  provider_id?: string;
  model?: string;
  api_key_id?: string;
  operation?: string;
  status_code?: number;
  error_class?: string;
  started_after?: string;
  started_before?: string;
};

export async function listRequests(
  filters: RequestFilters
): Promise<CursorPage<RequestSummary>> {
  const { data, error, response } = await apiClient.GET('/api/v1/requests', {
    params: { query: compactQuery(filters) }
  });
  return pageResult(result(data, error, response));
}

export async function getRequest(requestId: string): Promise<RequestDetail> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/requests/{request_id}',
    { params: { path: { request_id: requestId } } }
  );
  return result(data, error, response);
}
