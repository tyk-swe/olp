import { describe, expect, it } from 'vitest';
import {
  auditFilters,
  emptyAuditListState,
  type AuditListState
} from './auditListState';

function state(changes: Partial<AuditListState> = {}): AuditListState {
  return { ...emptyAuditListState(), ...changes };
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
      state({ occurredAfter: '2026-07-12T09:30', occurredBefore: '2026-07-12T18:00' })
    );

    expect(filters.occurred_after).toBe(new Date('2026-07-12T09:30').toISOString());
    expect(filters.occurred_before).toBe(new Date('2026-07-12T18:00').toISOString());
  });

  it('drops a half-typed date instead of sending an invalid bound', () => {
    expect(auditFilters(state({ occurredAfter: '2026-13-45T99:99' })).occurred_after).toBeUndefined();
    expect(auditFilters(state({ occurredBefore: '   ' })).occurred_before).toBeUndefined();
  });

  it('starts an unfiltered page from the first cursor', () => {
    expect(emptyAuditListState()).toMatchObject({ cursor: undefined, history: [] });
  });
});
