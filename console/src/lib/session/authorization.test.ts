import { describe, expect, it } from 'vitest';
import {
  MANAGEMENT_REQUIREMENTS,
  type ManagementRoute
} from '#lib/management/requirements.js';
import {
  FIXED_ROLES,
  allows,
  can,
  holds,
  isFixedRole,
  type Grant
} from './authorization';

function findRoute(
  predicate: (
    requirement: (typeof MANAGEMENT_REQUIREMENTS)[ManagementRoute]
  ) => boolean
): ManagementRoute {
  const entry = Object.entries(MANAGEMENT_REQUIREMENTS).find(([, req]) =>
    predicate(req)
  );
  if (!entry) throw new Error('Expected matching route in requirements');
  return entry[0] as ManagementRoute;
}

describe('authorization policy', () => {
  it('recognizes only the declared fixed roles', () => {
    for (const role of FIXED_ROLES) {
      expect(isFixedRole(role)).toBe(true);
    }
    expect(isFixedRole('admin')).toBe(false);
    expect(isFixedRole(null)).toBe(false);
    expect(isFixedRole(42)).toBe(false);
  });

  it('denies null and undefined grants and admits public routes for any signed-in grant', () => {
    const publicRoute = findRoute((req) => req.public);
    const emptyGrant: Grant = { operations: [], access_scope: 'assigned' };
    expect(allows(null, publicRoute)).toBe(false);
    expect(allows(undefined, publicRoute)).toBe(false);
    expect(allows(emptyGrant, publicRoute)).toBe(true);
  });

  it('requires every operation in a user alternative and global scope for installation routes', () => {
    const installationRoute = findRoute(
      (req) =>
        !req.public &&
        req.alternatives.length > 0 &&
        req.alternatives.every(
          (alt) =>
            alt.kind === 'user' && alt.installation && alt.operations.length > 0
        )
    );
    const requiredOps =
      MANAGEMENT_REQUIREMENTS[installationRoute].alternatives[0]!.operations;

    expect(
      allows(
        { operations: requiredOps, access_scope: 'global' },
        installationRoute
      )
    ).toBe(true);
    expect(
      allows(
        { operations: requiredOps, access_scope: 'assigned' },
        installationRoute
      )
    ).toBe(false);
    expect(
      allows(
        { operations: requiredOps.slice(1), access_scope: 'global' },
        installationRoute
      )
    ).toBe(false);
  });

  it('evaluates holds and can including the sessions.manage special case', () => {
    const globalReadOnly: Grant = {
      operations: ['read'],
      access_scope: 'global'
    };
    const sessionAdmin: Grant = {
      operations: ['manage_sessions'],
      access_scope: 'assigned'
    };

    expect(holds(null, 'read')).toBe(false);
    expect(holds(globalReadOnly, 'read')).toBe(true);
    expect(holds(globalReadOnly, 'manage_sessions')).toBe(false);

    expect(can(globalReadOnly, 'configuration.read')).toBe(true);
    expect(can(globalReadOnly, 'providers.manage')).toBe(false);
    expect(can(globalReadOnly, 'sessions.manage')).toBe(false);
    expect(can(sessionAdmin, 'sessions.manage')).toBe(true);
  });
});
