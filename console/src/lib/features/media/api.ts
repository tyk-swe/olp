import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { pageResult, result } from '$lib/api/http';
import type { CursorPage } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type MediaJob = components['schemas']['MediaJobItem'];

export type MediaJobFilters = {
  cursor?: string;
  limit?: number;
  api_key_id?: string;
  provider_id?: string;
  route?: string;
  state?: string;
  lifecycle?: string;
  created_after?: string;
  created_before?: string;
};

export async function listMediaJobs(
  filters: MediaJobFilters
): Promise<CursorPage<MediaJob>> {
  const { data, error, response } = await apiClient.GET('/api/v3/media-jobs', {
    params: { query: compactQuery(filters) }
  });
  return pageResult(result(data, error, response));
}

export async function getMediaJob(jobId: string): Promise<MediaJob> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/media-jobs/{job_id}',
    { params: { path: { job_id: jobId } } }
  );
  return result(data, error, response);
}
