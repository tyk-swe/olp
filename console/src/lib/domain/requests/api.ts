import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/session/client.js';
import { unwrapPage, type CursorPage } from '#lib/management/http.js';
import { compactQuery } from '#lib/management/query.js';

export type RequestSummary = components['schemas']['RequestSummary'];

export type RequestFilters = {
  cursor?: string;
  limit?: number;
  route?: string;
  provider_id?: string;
  model?: string;
  api_key_id?: string;
  project_id?: string;
  session_id?: string;
  attribution_key?: string;
  attribution_value?: string;
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
  return unwrapPage({ data, error, response });
}
