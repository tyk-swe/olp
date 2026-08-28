import type { components } from './schema';
import { apiClient } from './client';
import { pageResult, result } from './http';
import type { CursorPage } from './pagination';
import { compactQuery } from './query';

export type RequestSummary = components['schemas']['RequestSummary'];
export type RequestDetail = components['schemas']['RequestDetailResponse'];

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
