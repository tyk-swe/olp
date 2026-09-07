import { describe, expect, it } from 'vitest';
import { timeValid } from '$lib/lists/filters';
import {
  requestFilters,
  requestProblem,
  requestSearch,
  requestState,
  readRequestForm
} from '$lib/features/usage/history/requestListState';

describe('request applied URL filters', () => {
  it('round trips all applied filters without cursor, limit, or unrelated values', () => {
    const filters = {
      route: 'support/chat',
      provider_id: '01980000-0000-7000-8000-000000000104',
      model: 'model/one',
      api_key_id: '01980000-0000-7000-8000-000000000103',
      operation: 'generation',
      status_code: '200',
      error_class: 'upstream_http',
      started_after: '2026-07-12T13:30:42.123456Z',
      started_before: '2026-07-12T22:00:12.456789Z'
    };
    const state = requestState(
      new URLSearchParams({
        ...filters,
        cursor: 'local-only',
        limit: '1',
        unrelated: 'discard'
      })
    );
    const search = requestSearch(state.applied);
    expect(Object.fromEntries(new URLSearchParams(search))).toEqual(filters);
    expect(state.cursor).toBeUndefined();
    expect(state.history).toEqual([]);
    expect(state.startedAfter).toBe('2026-07-12T09:30');
    expect(requestState(new URLSearchParams(search))).toEqual(state);
  });

  it.each([
    ['provider_id', 'bad-id'],
    ['api_key_id', 'bad-id'],
    ['operation', 'unsupported'],
    ['status_code', 'NaN'],
    ['status_code', '65536'],
    ['started_after', 'not-a-date'],
    ['started_after', '2026-02-30T12:00:00Z'],
    ['started_after', '2026-07-12T10:00:00']
  ])('retains invalid %s values for visible validation', (name, value) => {
    const search = new URLSearchParams({ [name]: value });
    expect(requestProblem(readRequestForm(search), true)).toContain(value);
    if (name === 'started_after' && value === '2026-02-30T12:00:00Z') {
      expect(requestState(search).startedAfter).toBe(value);
      expect(requestState(search).applied.started_after).toBeUndefined();
    }
  });

  it('preserves fractional precision and the second fall-back hour on unrelated edits', () => {
    const start = '2026-11-01T06:30:42.123456Z';
    const end = '2026-11-01T07:30:12.456789Z';
    const state = requestState(
      new URLSearchParams({ started_after: start, started_before: end })
    );
    const draft = { ...state, route: 'edited' };
    const filters = requestFilters(draft, state.applied);
    expect(draft.startedAfter).toBe('2026-11-01T01:30');
    expect(filters.started_after).toBe(start);
    expect(filters.started_before).toBe(end);
    expect(state.applied.route).toBeUndefined();
    expect(requestProblem(draft, false, state.applied)).toBeNull();
  });

  it('accepts a valid fall-back interval whose displayed local times reverse', () => {
    const start = '2026-11-01T05:50:00Z';
    const end = '2026-11-01T06:10:00Z';
    const state = requestState(
      new URLSearchParams({ started_after: start, started_before: end })
    );
    const draft = { ...state, route: 'edited' };
    expect(draft.startedAfter).toBe('2026-11-01T01:50');
    expect(draft.startedBefore).toBe('2026-11-01T01:10');
    expect(requestProblem(draft, false, state.applied)).toBeNull();
    expect(requestFilters(draft, state.applied)).toMatchObject({
      started_after: start,
      started_before: end
    });
  });

  it('orders offsets and submillisecond bounds by their exact instants', () => {
    for (const [start, end] of [
      ['2026-07-12T12:00:00Z', '2026-07-12T08:30:00-04:00'],
      ['2026-07-12T12:00:00.123456Z', '2026-07-12T12:00:00.123457Z']
    ]) {
      expect(
        requestProblem(
          readRequestForm(
            new URLSearchParams({ started_after: start, started_before: end })
          ),
          true
        )
      ).toBeNull();
      expect(
        requestProblem(
          readRequestForm(
            new URLSearchParams({ started_after: end, started_before: start })
          ),
          true
        )
      ).not.toBeNull();
    }
  });

  it('validates calendar days without JavaScript date normalization', () => {
    expect(timeValid('2024-02-29T12:00:00Z', true)).toBe(true);
    expect(timeValid('2026-02-29T12:00:00Z', true)).toBe(false);
    expect(timeValid('2026-02-30T12:00:00Z', true)).toBe(false);
    expect(timeValid('2026-07-12T24:00:00Z', true)).toBe(false);
  });
});
