import { describe, expect, it } from 'vitest';
import {
  mediaJobProblem,
  mediaJobSearch,
  mediaJobState,
  mediaJobTimeValid,
  readMediaJobForm,
  mediaJobList,
  mediaJobFilters,
  type MediaJobListState
} from './mediaJobListState';

function state(changes: Partial<MediaJobListState> = {}): MediaJobListState {
  return { ...mediaJobList.empty(), ...changes };
}

describe('mediaJobFilters', () => {
  it('sends only the page size when nothing is filled in', () => {
    expect(mediaJobFilters(state())).toEqual({
      limit: 25,
      route: undefined,
      state: undefined,
      lifecycle: undefined,
      api_key_id: undefined,
      provider_id: undefined,
      created_after: undefined,
      created_before: undefined
    });
  });

  it('maps every field to its query parameter and trims identifiers', () => {
    expect(
      mediaJobFilters(
        state({
          route: ' video-render ',
          jobState: 'running',
          lifecycle: 'create_ambiguous',
          apiKeyId: ' 01980000-0000-7000-8000-000000000103 ',
          providerId: '01980000-0000-7000-8000-000000000104'
        })
      )
    ).toMatchObject({
      route: 'video-render',
      state: 'running',
      lifecycle: 'create_ambiguous',
      api_key_id: '01980000-0000-7000-8000-000000000103',
      provider_id: '01980000-0000-7000-8000-000000000104'
    });
  });

  it('converts local date bounds to instants', () => {
    const filters = mediaJobFilters(
      state({
        createdAfter: '2026-07-12T09:30',
        createdBefore: '2026-07-12T18:00'
      })
    );
    // The suite runs in America/New_York (UTC-4 in July), so the wall-clock
    // values the operator typed are four hours behind the instants sent.
    expect(filters.created_after).toBe('2026-07-12T13:30:00.000Z');
    expect(filters.created_before).toBe('2026-07-12T22:00:00.000Z');
  });

  it('drops a half-typed date instead of sending an invalid bound', () => {
    expect(
      mediaJobFilters(state({ createdAfter: '2026-13-45T99:99' })).created_after
    ).toBeUndefined();
    expect(
      mediaJobFilters(state({ createdBefore: '   ' })).created_before
    ).toBeUndefined();
  });
});

describe('mediaJob applied URL filters', () => {
  it('round trips all applied filters without cursor, limit, or unrelated values', () => {
    const filters = {
      route: 'video-route',
      state: 'running',
      lifecycle: 'active',
      api_key_id: '01980000-0000-7000-8000-000000000103',
      provider_id: '01980000-0000-7000-8000-000000000104',
      created_after: '2026-07-12T13:30:42.123456Z',
      created_before: '2026-07-12T22:00:12.456789Z'
    };
    const state = mediaJobState(
      new URLSearchParams({
        ...filters,
        cursor: 'local-only',
        limit: '1',
        unrelated: 'discard'
      })
    );
    const search = mediaJobSearch(state.applied);
    expect(Object.fromEntries(new URLSearchParams(search))).toEqual(filters);
    expect(state.cursor).toBeUndefined();
    expect(state.history).toEqual([]);
    expect(state.createdAfter).toBe('2026-07-12T09:30');
    expect(mediaJobState(new URLSearchParams(search))).toEqual(state);
  });

  it.each([
    ['provider_id', 'bad-id'],
    ['api_key_id', 'bad-id'],
    ['state', 'unsupported'],
    ['lifecycle', 'unsupported'],
    ['created_after', 'not-a-date'],
    ['created_after', '2026-02-30T12:00:00Z'],
    ['created_after', '2026-07-12T10:00:00']
  ])('retains invalid %s values for visible validation', (name, value) => {
    const search = new URLSearchParams({ [name]: value });
    expect(mediaJobProblem(readMediaJobForm(search), true)).toContain(value);
    if (name === 'created_after' && value === '2026-02-30T12:00:00Z') {
      expect(mediaJobState(search).createdAfter).toBe(value);
      expect(mediaJobState(search).applied.created_after).toBeUndefined();
    }
  });

  it('preserves fractional precision and the second fall-back hour on unrelated edits', () => {
    const start = '2026-11-01T06:30:42.123456Z';
    const end = '2026-11-01T07:30:12.456789Z';
    const state = mediaJobState(
      new URLSearchParams({ created_after: start, created_before: end })
    );
    const draft = { ...state, route: 'edited' };
    const filters = mediaJobFilters(draft, state.applied);
    expect(draft.createdAfter).toBe('2026-11-01T01:30');
    expect(filters.created_after).toBe(start);
    expect(filters.created_before).toBe(end);
    expect(state.applied.route).toBeUndefined();
    expect(mediaJobProblem(draft, false, state.applied)).toBeNull();
  });

  it('accepts a valid fall-back interval whose displayed local times reverse', () => {
    const start = '2026-11-01T05:50:00Z';
    const end = '2026-11-01T06:10:00Z';
    const state = mediaJobState(
      new URLSearchParams({ created_after: start, created_before: end })
    );
    const draft = { ...state, route: 'edited' };
    expect(draft.createdAfter).toBe('2026-11-01T01:50');
    expect(draft.createdBefore).toBe('2026-11-01T01:10');
    expect(mediaJobProblem(draft, false, state.applied)).toBeNull();
    expect(mediaJobFilters(draft, state.applied)).toMatchObject({
      created_after: start,
      created_before: end
    });
  });

  it('orders offsets and submillisecond bounds by their exact instants', () => {
    for (const [start, end] of [
      ['2026-07-12T12:00:00Z', '2026-07-12T08:30:00-04:00'],
      ['2026-07-12T12:00:00.123456Z', '2026-07-12T12:00:00.123457Z']
    ]) {
      expect(
        mediaJobProblem(
          readMediaJobForm(
            new URLSearchParams({ created_after: start, created_before: end })
          ),
          true
        )
      ).toBeNull();
      expect(
        mediaJobProblem(
          readMediaJobForm(
            new URLSearchParams({ created_after: end, created_before: start })
          ),
          true
        )
      ).not.toBeNull();
    }
  });

  it('validates calendar days without JavaScript date normalization', () => {
    expect(mediaJobTimeValid('2024-02-29T12:00:00Z', true)).toBe(true);
    expect(mediaJobTimeValid('2026-02-29T12:00:00Z', true)).toBe(false);
    expect(mediaJobTimeValid('2026-02-30T12:00:00Z', true)).toBe(false);
    expect(mediaJobTimeValid('2026-07-12T24:00:00Z', true)).toBe(false);
  });
});
