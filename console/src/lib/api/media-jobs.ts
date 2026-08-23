import type { components } from './schema';
import { apiClient } from './client';
import { result } from './http';
import { type CursorPage, toCursorPage } from './pagination';
import { compactQuery } from './query';

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
  const { data, error, response } = await apiClient.GET('/api/v1/media-jobs', {
    params: { query: compactQuery(filters) }
  });
  return toCursorPage(result(data, error, response));
}

export async function getMediaJob(jobId: string): Promise<MediaJob> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/media-jobs/{job_id}',
    { params: { path: { job_id: jobId } } }
  );
  return result(data, error, response);
}
