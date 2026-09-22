import { describe, expect, it } from 'vitest';
import { mediaTimeline } from './mediaTimeline';
import type { MediaJob } from './api';

const job: MediaJob = {
  id: 'job',
  api_key_id: 'key',
  provider_id: 'provider',
  provider_name: 'fixture',
  provider_model: 'model',
  route: 'media-route',
  operation: 'video_create',
  surface: 'openai',
  state: 'succeeded',
  lifecycle: 'retrievable',
  content_available: true,
  etag: 'etag',
  created_at: '2026-09-22T00:00:00Z',
  updated_at: '2026-09-22T00:03:00Z',
  last_polled_at: '2026-09-22T00:02:00Z',
  completed_at: '2026-09-22T00:02:30Z',
  expires_at: '2026-09-23T00:00:00Z',
  deleted_at: null,
  error_class: null,
  reconciliation_error: null,
  upstream_job_id: 'upstream-job',
  progress_percent: 100
};

describe('recorded media lifecycle', () => {
  it('separates recorded timestamps from the scheduled retention deadline', () => {
    const timeline = mediaTimeline(job);
    expect(timeline.recorded.map((item) => item.label)).toEqual([
      'OLP record created',
      'Last provider status check',
      'Terminal state recorded',
      'Last metadata update'
    ]);
    expect(timeline.expiry).toBe('2026-09-23T00:00:00Z');
    expect(
      timeline.recorded.some((item) => item.label.includes('expires'))
    ).toBe(false);
  });

  it('does not invent a provider completion event for a pending job', () => {
    const timeline = mediaTimeline({
      ...job,
      state: 'processing',
      lifecycle: 'accepted',
      completed_at: null,
      last_polled_at: null,
      updated_at: job.created_at
    });
    expect(timeline.recorded.map((item) => item.label)).toEqual([
      'OLP record created'
    ]);
  });
});
