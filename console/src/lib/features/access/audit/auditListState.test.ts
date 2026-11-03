import { describe, expect, it } from 'vitest';
import {
  auditActorLabel,
  auditFilters,
  auditList,
  auditProblem,
  auditSearch,
  auditState,
  auditUrl,
  type AuditForm
} from './auditListState';

const id = '123e4567-e89b-12d3-a456-426614174000';
const blank = auditList.empty();

describe('audit filters', () => {
  it('round trips shared filters without losing precise occurrence times', () => {
    const search = new URLSearchParams({
      action: ' update ',
      resource_type: ' project ',
      resource_id: ' project-id ',
      actor_user_id: id,
      outcome: 'failure',
      occurred_after: '2025-04-01T08:00:05.123456789-04:00',
      occurred_before: '2025-04-02T08:00:07.987654321-04:00'
    });
    const state = auditState(search);

    expect(state.cursor).toBeUndefined();
    expect(state.history).toEqual([]);
    expect(state.applied).toMatchObject({
      action: 'update',
      resource_type: 'project',
      resource_id: 'project-id',
      actor_user_id: id,
      outcome: 'failure',
      occurred_after: '2025-04-01T08:00:05.123456789-04:00',
      occurred_before: '2025-04-02T08:00:07.987654321-04:00'
    });
    expect(auditFilters(state, state.applied)).toEqual(state.applied);
    expect(auditProblem(state, false, state.applied)).toBeNull();
    expect(
      auditState(new URLSearchParams(auditSearch(state.applied))).applied
    ).toEqual(state.applied);
    expect(auditUrl.canonical(search)).toBe(auditSearch(state.applied));
  });

  it('keeps invalid shared-link values visible instead of canonicalizing them', () => {
    const search = new URLSearchParams({ occurred_after: 'not-a-time' });
    const state = auditState(search);

    expect(state.occurredAfter).toBe('not-a-time');
    expect(auditProblem(state, true)).toContain('Occurred after');
    expect(auditUrl.canonical(search)).toBeNull();
  });

  it.each([
    [{ actorUserId: 'bad' }, 'Actor user ID must be a UUID: bad'],
    [{ outcome: 'unknown' }, 'Unknown audit outcome: unknown'],
    [
      { occurredAfter: '2025-04-02T00:00', occurredBefore: '2025-04-01T00:00' },
      'Occurred before must be later than occurred after.'
    ]
  ] satisfies [Partial<AuditForm>, string][])(
    'rejects invalid filters %o',
    (fields, error) => {
      expect(auditProblem({ ...blank, ...fields })).toBe(error);
    }
  );
});

describe('audit actor labels', () => {
  it.each([
    [{ actor_type: 'system' }, 'System'],
    [
      { actor_type: 'management_token', actor_label: 'Automation' },
      'Token · Automation'
    ],
    [
      { actor_type: 'management_token', actor_management_token_id: id },
      `Token · ${id}`
    ],
    [{ actor_type: 'management_token' }, 'Token · unknown'],
    [
      { actor_type: 'user', actor_label: 'Operator', actor_user_id: id },
      'Operator'
    ],
    [{ actor_type: 'user', actor_user_id: id }, id],
    [{ actor_type: 'user' }, 'Unknown user']
  ] satisfies [Parameters<typeof auditActorLabel>[0], string][])(
    'identifies the actor in an event',
    (actor, label) => {
      expect(auditActorLabel(actor)).toBe(label);
    }
  );
});
