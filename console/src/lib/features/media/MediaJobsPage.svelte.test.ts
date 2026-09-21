import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  deleteMediaJob,
  downloadMediaJobContent,
  getMediaJob,
  listMediaJobs,
  refreshMediaJob,
  type MediaJob
} from './api';
import MediaJobsProbe from './test/MediaJobsProbe.svelte';

const navigation = vi.hoisted(() => ({ goto: vi.fn() }));
const role = vi.hoisted(() => ({ can: vi.fn(() => true) }));

vi.mock('$app/navigation', () => ({ goto: navigation.goto }));
vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/media-jobs') }
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => role
}));
vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listMediaJobs: vi.fn(),
  getMediaJob: vi.fn(),
  refreshMediaJob: vi.fn(),
  deleteMediaJob: vi.fn(),
  downloadMediaJobContent: vi.fn()
}));

const SERVER_DELAY = 40;

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

const succeededJob: MediaJob = {
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
  etag: 'job-etag-1',
  created_at: '2026-07-20T12:00:00Z',
  updated_at: '2026-07-20T12:05:00Z'
};

const queuedJob: MediaJob = {
  ...succeededJob,
  state: 'queued',
  content_available: false,
  progress_percent: 20,
  completed_at: null,
  last_polled_at: null
};

function deferred<T>(value: T, ms = SERVER_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function settle(ms = SERVER_DELAY + 20) {
  return vi.advanceTimersByTimeAsync(ms);
}

function establish(jobId = '') {
  component = mount(MediaJobsProbe, {
    target: host,
    props: { client, jobId }
  });
  flushSync();
  return settle();
}

function button(label: string) {
  return [...host.querySelectorAll('button')].find(
    (item) => item.textContent?.trim() === label
  );
}

beforeEach(() => {
  vi.useFakeTimers();
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  role.can.mockReturnValue(true);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  Object.defineProperty(document, 'visibilityState', {
    value: 'visible',
    configurable: true
  });
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('list polling', () => {
  it('polls every 3s while any listed job is pending', async () => {
    vi.mocked(listMediaJobs).mockImplementation(() =>
      deferred({ items: [queuedJob], nextCursor: null })
    );
    await establish();
    expect(host.textContent).toContain('video-route');
    expect(listMediaJobs).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(3000);
    await settle();
    expect(listMediaJobs).toHaveBeenCalledTimes(2);
  });

  it('stops polling once every listed job is terminal', async () => {
    vi.mocked(listMediaJobs).mockImplementation(() =>
      deferred({ items: [succeededJob], nextCursor: null })
    );
    await establish();
    expect(listMediaJobs).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(9000);
    expect(listMediaJobs).toHaveBeenCalledTimes(1);
  });

  it('stops polling while the tab is hidden', async () => {
    Object.defineProperty(document, 'visibilityState', {
      value: 'hidden',
      configurable: true
    });
    vi.mocked(listMediaJobs).mockImplementation(() =>
      deferred({ items: [queuedJob], nextCursor: null })
    );
    await establish();
    expect(listMediaJobs).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(9000);
    expect(listMediaJobs).toHaveBeenCalledTimes(1);
  });
});

describe('detail polling', () => {
  it('polls a pending job and stops when it turns terminal', async () => {
    vi.mocked(getMediaJob)
      .mockImplementationOnce(() => deferred(queuedJob))
      .mockImplementation(() => deferred(succeededJob));
    await establish(queuedJob.id);
    expect(getMediaJob).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(3000);
    await settle();
    expect(getMediaJob).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(9000);
    expect(getMediaJob).toHaveBeenCalledTimes(2);
  });
});

describe('operator actions', () => {
  it('hides mutations from principals without media.manage', async () => {
    role.can.mockReturnValue(false);
    vi.mocked(getMediaJob).mockImplementation(() => deferred(succeededJob));
    await establish(succeededJob.id);
    expect(button('Refresh now')).toBeUndefined();
    expect(button('Download content')).toBeUndefined();
    expect(button('Delete content')).toBeUndefined();
  });

  it('refreshes a job and writes the result back', async () => {
    vi.mocked(getMediaJob).mockImplementation(() => deferred(succeededJob));
    const updated = {
      ...succeededJob,
      last_polled_at: '2026-07-20T12:06:00Z'
    };
    vi.mocked(refreshMediaJob).mockResolvedValue(updated);
    await establish(succeededJob.id);
    button('Refresh now')!.click();
    flushSync();
    await settle(0);
    expect(refreshMediaJob).toHaveBeenCalledWith(succeededJob.id);
    expect(
      client.getQueryData(['media-jobs', 'detail', succeededJob.id])
    ).toEqual(updated);
  });

  it('downloads content through a revoked object URL', async () => {
    vi.mocked(getMediaJob).mockImplementation(() => deferred(succeededJob));
    vi.mocked(downloadMediaJobContent).mockResolvedValue({
      blob: new Blob(['video']),
      filename: 'job.mp4'
    });
    const created: string[] = [];
    vi.stubGlobal('URL', {
      ...URL,
      createObjectURL: vi.fn(() => {
        created.push('blob:mock-1');
        return 'blob:mock-1';
      }),
      revokeObjectURL: vi.fn()
    });
    await establish(succeededJob.id);
    button('Download content')!.click();
    flushSync();
    await settle(0);
    expect(downloadMediaJobContent).toHaveBeenCalledWith(
      succeededJob.id,
      'video'
    );
    expect(created).toEqual(['blob:mock-1']);
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:mock-1');
  });

  it('deletes with the job ETag after confirmation', async () => {
    vi.mocked(getMediaJob).mockImplementation(() => deferred(succeededJob));
    vi.mocked(deleteMediaJob).mockResolvedValue(undefined);
    const confirm = vi.fn(() => true);
    vi.stubGlobal('confirm', confirm);
    await establish(succeededJob.id);
    button('Delete content')!.click();
    flushSync();
    await settle(0);
    expect(confirm).toHaveBeenCalled();
    expect(deleteMediaJob).toHaveBeenCalledWith(
      succeededJob.id,
      succeededJob.etag
    );
    await settle();
    expect(navigation.goto).toHaveBeenCalledWith('/media-jobs');
  });
});
