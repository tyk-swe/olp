import { describe, expect, it } from 'vitest';
import {
  mediaJobFilters,
  mediaJobList,
  mediaJobPending,
  mediaJobPollInterval,
  mediaJobProblem,
  mediaJobSearch,
  mediaJobState,
  mediaJobUrl,
  type MediaJobForm
} from './mediaJobListState';

const id = '123e4567-e89b-12d3-a456-426614174000';
const blank = mediaJobList.empty();

describe('media job filters', () => {
  it('round trips shared filters without losing precise creation times', () => {
    const search = new URLSearchParams({
      route: ' route-a ',
      state: 'running',
      lifecycle: 'active',
      api_key_id: id,
      provider_id: id,
      created_after: '2025-04-01T08:00:05.123456789-04:00',
      created_before: '2025-04-02T08:00:07.987654321-04:00'
    });
    const state = mediaJobState(search);

    expect(state.cursor).toBeUndefined();
    expect(state.history).toEqual([]);
    expect(state.applied).toMatchObject({
      route: 'route-a',
      state: 'running',
      lifecycle: 'active',
      api_key_id: id,
      provider_id: id,
      created_after: '2025-04-01T08:00:05.123456789-04:00',
      created_before: '2025-04-02T08:00:07.987654321-04:00'
    });
    expect(mediaJobFilters(state, state.applied)).toEqual(state.applied);
    expect(mediaJobProblem(state, false, state.applied)).toBeNull();
    expect(
      mediaJobState(new URLSearchParams(mediaJobSearch(state.applied))).applied
    ).toEqual(state.applied);
    expect(mediaJobUrl.canonical(search)).toBe(mediaJobSearch(state.applied));
  });

  it('keeps invalid shared-link values visible instead of canonicalizing them', () => {
    const search = new URLSearchParams({ created_after: 'not-a-time' });
    const state = mediaJobState(search);

    expect(state.createdAfter).toBe('not-a-time');
    expect(mediaJobProblem(state, true)).toContain('Created after');
    expect(mediaJobUrl.canonical(search)).toBeNull();
  });

  it.each([
    [{ providerId: 'bad' }, 'Provider ID must be a UUID: bad'],
    [{ apiKeyId: 'bad' }, 'API key ID must be a UUID: bad'],
    [{ jobState: 'unknown' }, 'Unknown media-job state: unknown'],
    [{ lifecycle: 'unknown' }, 'Unknown media-job lifecycle: unknown'],
    [
      { createdAfter: '2025-04-02T00:00', createdBefore: '2025-04-01T00:00' },
      'Created before must be later than created after.'
    ]
  ] satisfies [Partial<MediaJobForm>, string][])(
    'rejects invalid filters %o',
    (fields, error) => {
      expect(mediaJobProblem({ ...blank, ...fields })).toBe(error);
    }
  );
});

describe('media job polling', () => {
  it.each([
    [{ state: 'queued', lifecycle: 'active' }, true],
    [{ state: 'running', lifecycle: 'active' }, true],
    [{ state: 'succeeded', lifecycle: 'creating' }, true],
    [{ state: 'failed', lifecycle: 'create_ambiguous' }, true],
    [{ state: 'failed', lifecycle: 'create_cleanup_pending' }, true],
    [{ state: 'succeeded', lifecycle: 'delete_pending' }, true],
    [{ state: 'succeeded', lifecycle: 'active' }, false],
    [{ state: 'cancelled', lifecycle: 'deleted' }, false]
  ])('polls pending or reconciling jobs %#', (job, pending) => {
    expect(mediaJobPending(job)).toBe(pending);
    const interval = mediaJobPollInterval(pending);
    if (pending) {
      expect(interval).toBeGreaterThan(0);
    } else {
      expect(interval).toBe(false);
    }
  });
});
