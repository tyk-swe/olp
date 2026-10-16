import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

export async function cacheStatistics(route: string, signal?: AbortSignal) {
  return unwrap(
    await apiClient.GET('/api/v1/cache/statistics', {
      params: { query: { route } },
      signal
    })
  );
}

export async function purgeCache(input: {
  route_id?: string;
  project_id?: string;
}) {
  return unwrap(await apiClient.POST('/api/v1/cache/purge', { body: input }));
}
