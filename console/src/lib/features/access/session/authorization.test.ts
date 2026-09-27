import { describe, expect, it } from 'vitest';
import {
  MANAGEMENT_REQUIREMENTS,
  type ManagementRoute
} from '$lib/api/requirements';
import { MANAGEMENT_TOKEN_SCOPES } from '$lib/features/access/api';
import {
  FIXED_ROLES,
  allows,
  can,
  holds,
  isFixedRole,
  type Grant
} from '$lib/features/access/session/authorization';
import {
  authorizationGolden,
  operationsFor
} from '$lib/features/access/session/test/grants';

function member(role: (typeof FIXED_ROLES)[number], global: boolean): Grant {
  return {
    operations: operationsFor(role, global),
    access_scope: global ? 'global' : 'assigned'
  };
}

describe('contract-derived authorization', () => {
  it('admits exactly the members the server admits to every route', () => {
    const members = authorizationGolden.archetypes.flatMap(
      (archetype, index) =>
        archetype.kind === 'user' ? [{ archetype, index }] : []
    );
    expect(members).toHaveLength(8);
    for (const [route, row] of Object.entries(authorizationGolden.routes)) {
      for (const { archetype, index } of members) {
        const grant: Grant = {
          operations: archetype.operations,
          access_scope: archetype.global ? 'global' : 'assigned'
        };
        expect(
          allows(grant, route as ManagementRoute),
          `${route} as ${archetype.name}`
        ).toBe(row[index] === 'Y');
      }
    }
  });

  it('knows every route the server serves', () => {
    expect(Object.keys(MANAGEMENT_REQUIREMENTS).sort()).toEqual(
      Object.keys(authorizationGolden.routes).sort()
    );
  });

  it('offers exactly the scopes a management token may carry', () => {
    const delegable = authorizationGolden.archetypes.find(
      (archetype) => archetype.name === 'token every scope, owner global'
    );
    expect([...MANAGEMENT_TOKEN_SCOPES].sort()).toEqual(
      [...(delegable?.operations ?? [])].sort()
    );
  });

  it('keeps installation pages from assigned members', () => {
    for (const capability of ['settings.read', 'users.read'] as const) {
      expect(can(member('operator', true), capability)).toBe(true);
      expect(can(member('operator', false), capability)).toBe(false);
    }
    expect(allows(member('owner', false), 'GET /api/v1/audit')).toBe(false);
    expect(can(member('owner', false), 'sessions.manage')).toBe(false);
    expect(can(member('owner', true), 'sessions.manage')).toBe(true);
  });

  it('reserves installation-wide notifications for installation settings', () => {
    expect(holds(member('developer', true), 'settings')).toBe(false);
    expect(holds(member('operator', true), 'settings')).toBe(true);
  });

  it('grants nothing without a signed-in member', () => {
    expect(can(null, 'configuration.read')).toBe(false);
    expect(can(undefined, 'configuration.read')).toBe(false);
    expect(allows(null, 'GET /api/v1/providers')).toBe(false);
  });

  it.each([
    ['owner', true],
    ['operator', true],
    ['developer', true],
    ['viewer', true],
    ['Owner', false],
    ['administrator', false],
    ['', false],
    [null, false],
    [42, false]
  ])('validates the closed role value %j', (value, expected) => {
    expect(isFixedRole(value)).toBe(expected);
  });
});
