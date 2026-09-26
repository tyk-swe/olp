import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ApiProblem, ensureSuccess, pageResult, result } from '$lib/api/http';
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
  const { data, error, response } = await apiClient.GET('/api/v1/media-jobs', {
    params: { query: compactQuery(filters) }
  });
  return pageResult(result(data, error, response));
}

export async function getMediaJob(jobId: string): Promise<MediaJob> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/media-jobs/{job_id}',
    { params: { path: { job_id: jobId } } }
  );
  return result(data, error, response);
}

export async function refreshMediaJob(jobId: string): Promise<MediaJob> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/media-jobs/{job_id}/refresh',
    { params: { path: { job_id: jobId } } }
  );
  return result(data, error, response);
}

export async function deleteMediaJob(
  jobId: string,
  etag: string
): Promise<void> {
  const { error, response } = await apiClient.DELETE(
    '/api/v1/media-jobs/{job_id}',
    {
      params: {
        path: { job_id: jobId },
        header: { 'If-Match': etag }
      }
    }
  );
  ensureSuccess(error, response);
}

export type MediaContentVariant = 'video' | 'thumbnail' | 'spritesheet';

export async function downloadMediaJobContent(
  jobId: string,
  variant: MediaContentVariant
): Promise<{ blob: Blob; filename: string }> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/media-jobs/{job_id}/content',
    {
      params: {
        path: { job_id: jobId },
        query: { variant }
      },
      parseAs: 'blob'
    }
  );
  if (error || !(data instanceof Blob)) {
    ensureSuccess(error ?? { message: 'invalid content' }, response);
    throw new ApiProblem({
      type: 'urn:olp:problem:invalid-api-response',
      title: 'The media content response did not include a binary body',
      status: 502
    });
  }
  const disposition = response.headers.get('content-disposition') ?? '';
  const match = /filename="([^"]+)"/.exec(disposition);
  return {
    blob: data as Blob,
    filename: match?.[1] ?? `media-${jobId}-${variant}`
  };
}
