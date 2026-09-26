import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';

export type Overview = components['schemas']['OverviewResponse'];

/// The aggregate the overview page needs in one request, rather than a full
/// paginated scan of every collection it counts.
export async function getOverview(signal?: AbortSignal): Promise<Overview> {
  const response = await apiClient.GET('/api/v1/overview', { signal });
  return result(response.data, response.error, response.response);
}
