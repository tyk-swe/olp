import type { components } from '#lib/management/schema.js';
import { apiClient } from '#lib/management/client.js';
import { unwrap } from '#lib/management/http.js';

export type Overview = components['schemas']['OverviewResponse'];

/// The aggregate the overview page needs in one request, rather than a full
/// paginated scan of every collection it counts.
export async function getOverview(signal?: AbortSignal): Promise<Overview> {
  const response = await apiClient.GET('/api/v1/overview', { signal });
  return unwrap(response);
}
