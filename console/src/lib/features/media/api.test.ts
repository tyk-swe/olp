import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { clearCsrfToken } from '$lib/features/access/session/api';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import {
  deleteMediaJob,
  downloadMediaJobContent,
  getMediaJob,
  listMediaJobs,
  refreshMediaJob,
  type MediaJob
} from '$lib/features/media/api';

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000401',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const,
    access_scope: 'global' as const
  },
  csrf_token: 'csrf-media-token'
};

const job: MediaJob = {
  id: '01980000-0000-7000-8000-000000000901',
  api_key_id: '01980000-0000-7000-8000-000000000902',
  provider_id: '01980000-0000-7000-8000-000000000903',
  provider_name: 'Video provider',
  provider_model: 'sora-1',
  route: 'video-route',
  operation: 'video_create',
  surface: 'openai',
  state: 'succeeded',
  lifecycle: 'active',
  content_available: true,
  progress_percent: 100,
  upstream_job_id: 'vid-up-1',
  error_class: null,
  reconciliation_error: null,
  completed_at: '2026-07-20T12:04:00Z',
  expires_at: null,
  deleted_at: null,
  last_polled_at: '2026-07-20T12:04:00Z',
  etag: '01980000-0000-7000-8000-000000000910',
  created_at: '2026-07-20T12:00:00Z',
  updated_at: '2026-07-20T12:05:00Z'
};

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('media job management api', () => {
  it('lists jobs with compact filters and cursor', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() =>
      jsonResponse({ items: [job], next_cursor: null })
    );

    await listMediaJobs({
      limit: 20,
      route: 'video-route',
      state: 'queued',
      lifecycle: 'active',
      cursor: 'page-two',
      api_key_id: '',
      provider_id: job.provider_id
    });

    const url = new URL(requests[0]!.url);
    expect(requests[0]!.method).toBe('GET');
    expect(url.pathname).toBe('/api/v1/media-jobs');
    expect(url.searchParams.get('route')).toBe('video-route');
    expect(url.searchParams.get('state')).toBe('queued');
    expect(url.searchParams.get('lifecycle')).toBe('active');
    expect(url.searchParams.get('cursor')).toBe('page-two');
    expect(url.searchParams.get('provider_id')).toBe(job.provider_id);
    expect(url.searchParams.has('api_key_id')).toBe(false);
    expect(url.searchParams.get('limit')).toBe('20');
  });

  it('loads one job detail', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse(job));

    const detail = await getMediaJob(job.id);

    expect(requests[0]!.method).toBe('GET');
    expect(new URL(requests[0]!.url).pathname).toBe(
      `/api/v1/media-jobs/${job.id}`
    );
    expect(detail.id).toBe(job.id);
  });

  it('posts a synchronous refresh', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => jsonResponse(job));

    await refreshMediaJob(job.id);

    expect(requests[0]!.method).toBe('POST');
    expect(new URL(requests[0]!.url).pathname).toBe(
      `/api/v1/media-jobs/${job.id}/refresh`
    );
  });

  it('deletes a job under its ETag', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(() => new Response(null, { status: 204 }));

    await deleteMediaJob(job.id, job.etag);

    expect(requests[0]!.method).toBe('DELETE');
    expect(new URL(requests[0]!.url).pathname).toBe(
      `/api/v1/media-jobs/${job.id}`
    );
    expect(requests[0]!.headers.get('if-match')).toBe(`"${job.etag}"`);
  });

  it('downloads a variant as a blob with the server filename', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(
      () =>
        new Response(new Blob(['video-bytes']), {
          headers: {
            'content-type': 'video/mp4',
            'content-disposition':
              'attachment; filename="olp-media-1-video.mp4"'
          }
        })
    );

    const { blob, filename } = await downloadMediaJobContent(
      job.id,
      'thumbnail'
    );

    const url = new URL(requests[0]!.url);
    expect(requests[0]!.method).toBe('GET');
    expect(url.pathname).toBe(`/api/v1/media-jobs/${job.id}/content`);
    expect(url.searchParams.get('variant')).toBe('thumbnail');
    expect(filename).toBe('olp-media-1-video.mp4');
    expect(await blob.text()).toBe('video-bytes');
  });
});
