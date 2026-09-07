import { describe, expect, it } from 'vitest';
import { timeValid } from '$lib/lists/filters';
import {
  auditProblem,
  auditSearch,
  auditState,
  readAuditForm,
  auditList,
  auditFilters,
  auditRangeError,
  type AuditListState
} from '$lib/features/access/audit/auditListState';

function state(changes: Partial<AuditListState> = {}): AuditListState {
  return { ...auditList.empty(), ...changes };
}

describe('auditFilters', () => {
  it('sends only the page size when nothing is filled in', () => {
    expect(auditFilters(state())).toEqual({
      limit: 50,
      action: undefined,
      resource_type: undefined,
      resource_id: undefined,
      actor_user_id: undefined,
      outcome: undefined,
      occurred_after: undefined,
      occurred_before: undefined
    });
  });

  it('maps every field to its query parameter and trims identifiers', () => {
    expect(
      auditFilters(
        state({
          action: ' provider.update ',
          resourceType: 'provider',
          resourceId: ' 01980000-0000-7000-8000-000000000104 ',
          actorUserId: '01980000-0000-7000-8000-000000000001',
          outcome: 'failure'
        })
      )
    ).toMatchObject({
      action: 'provider.update',
      resource_type: 'provider',
      resource_id: '01980000-0000-7000-8000-000000000104',
      actor_user_id: '01980000-0000-7000-8000-000000000001',
      outcome: 'failure'
    });
  });

  it('converts local date bounds to instants', () => {
    const filters = auditFilters(
      state({
        occurredAfter: '2026-07-12T09:30',
        occurredBefore: '2026-07-12T18:00'
      })
    );

    // The suite runs in America/New_York (UTC-4 in July), so the wall-clock
    // values the operator typed are four hours behind the instants sent.
    expect(filters.occurred_after).toBe('2026-07-12T13:30:00.000Z');
    expect(filters.occurred_before).toBe('2026-07-12T22:00:00.000Z');
  });

  it('drops a half-typed date instead of sending an invalid bound', () => {
    expect(
      auditFilters(state({ occurredAfter: '2026-13-45T99:99' })).occurred_after
    ).toBeUndefined();
    expect(
      auditFilters(state({ occurredBefore: '   ' })).occurred_before
    ).toBeUndefined();
  });
});

describe('auditRangeError', () => {
  it('rejects a window that ends before it starts', () => {
    expect(
      auditRangeError(
        state({
          occurredAfter: '2026-07-12T18:00',
          occurredBefore: '2026-07-12T09:30'
        })
      )
    ).toBe('Occurred before must be later than occurred after.');
  });

  it('rejects equal bounds because the server requires a strictly later end', () => {
    expect(
      auditRangeError(
        state({
          occurredAfter: '2026-07-12T09:30',
          occurredBefore: '2026-07-12T09:30'
        })
      )
    ).toBe('Occurred before must be later than occurred after.');
  });

  it('accepts an ordered window and a half-filled one', () => {
    expect(
      auditRangeError(
        state({
          occurredAfter: '2026-07-12T09:30',
          occurredBefore: '2026-07-12T18:00'
        })
      )
    ).toBeNull();
    expect(
      auditRangeError(state({ occurredAfter: '2026-07-12T18:00' }))
    ).toBeNull();
    expect(auditRangeError(state())).toBeNull();
  });
});

describe('auditList.empty', () => {
  it('starts an unfiltered page from the first cursor', () => {
    expect(auditList.empty()).toMatchObject({
      cursor: undefined,
      history: []
    });
  });
});

describe('audit applied URL filters', () => {
  it('round trips all applied filters without cursor, limit, or unrelated values', () => {
    const filters = {
      action: 'setting.update',
      resource_type: 'setting',
      resource_id: 'limits.valkey_unavailable',
      actor_user_id: '01980000-0000-7000-8000-000000000103',
      outcome: 'failure',
      occurred_after: '2026-07-12T13:30:42.123456Z',
      occurred_before: '2026-07-12T22:00:12.456789Z'
    };
    const state = auditState(
      new URLSearchParams({
        ...filters,
        cursor: 'local-only',
        limit: '1',
        unrelated: 'discard'
      })
    );
    const search = auditSearch(state.applied);
    expect(Object.fromEntries(new URLSearchParams(search))).toEqual(filters);
    expect(state.cursor).toBeUndefined();
    expect(state.history).toEqual([]);
    expect(state.occurredAfter).toBe('2026-07-12T09:30');
    expect(auditState(new URLSearchParams(search))).toEqual(state);
  });

  it.each([
    ['actor_user_id', 'bad-id'],
    ['outcome', 'unsupported'],
    ['occurred_after', 'not-a-date'],
    ['occurred_after', '2026-02-30T12:00:00Z'],
    ['occurred_after', '2026-07-12T10:00:00']
  ])('retains invalid %s values for visible validation', (name, value) => {
    const search = new URLSearchParams({ [name]: value });
    expect(auditProblem(readAuditForm(search), true)).toContain(value);
    if (name === 'occurred_after' && value === '2026-02-30T12:00:00Z') {
      expect(auditState(search).occurredAfter).toBe(value);
      expect(auditState(search).applied.occurred_after).toBeUndefined();
    }
  });

  it('preserves fractional precision and the second fall-back hour on unrelated edits', () => {
    const start = '2026-11-01T06:30:42.123456Z';
    const end = '2026-11-01T07:30:12.456789Z';
    const state = auditState(
      new URLSearchParams({ occurred_after: start, occurred_before: end })
    );
    const draft = { ...state, action: 'edited' };
    const filters = auditFilters(draft, state.applied);
    expect(draft.occurredAfter).toBe('2026-11-01T01:30');
    expect(filters.occurred_after).toBe(start);
    expect(filters.occurred_before).toBe(end);
    expect(state.applied.action).toBeUndefined();
    expect(auditProblem(draft, false, state.applied)).toBeNull();
  });

  it('accepts a valid fall-back interval whose displayed local times reverse', () => {
    const start = '2026-11-01T05:50:00Z';
    const end = '2026-11-01T06:10:00Z';
    const state = auditState(
      new URLSearchParams({ occurred_after: start, occurred_before: end })
    );
    const draft = { ...state, action: 'edited' };
    expect(draft.occurredAfter).toBe('2026-11-01T01:50');
    expect(draft.occurredBefore).toBe('2026-11-01T01:10');
    expect(auditProblem(draft, false, state.applied)).toBeNull();
    expect(auditFilters(draft, state.applied)).toMatchObject({
      occurred_after: start,
      occurred_before: end
    });
  });

  it('orders offsets and submillisecond bounds by their exact instants', () => {
    for (const [start, end] of [
      ['2026-07-12T12:00:00Z', '2026-07-12T08:30:00-04:00'],
      ['2026-07-12T12:00:00.123456Z', '2026-07-12T12:00:00.123457Z']
    ]) {
      expect(
        auditProblem(
          readAuditForm(
            new URLSearchParams({ occurred_after: start, occurred_before: end })
          ),
          true
        )
      ).toBeNull();
      expect(
        auditProblem(
          readAuditForm(
            new URLSearchParams({ occurred_after: end, occurred_before: start })
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
