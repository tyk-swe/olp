import type { ManagementRoute } from '#lib/management/requirements.js';
import {
  allows,
  can,
  type Capability,
  type FixedRole
} from '#lib/features/access/session/authorization.js';
import { authLifecycle } from '#lib/features/access/session/lifecycle.js';
import type { AuthenticatedUser } from '#lib/features/access/session/state.js';

export type RoleAccess = {
  readonly user: AuthenticatedUser | null;
  readonly role: FixedRole | null;
  readonly globalScope: boolean;
  can(capability: Capability): boolean;
  allows(route: ManagementRoute): boolean;
};

/**
 * Reactive view of the signed-in principal. Roles change underneath an open
 * console — an owner can demote themselves, and another tab can sign in as
 * somebody else — so capability checks must re-run instead of being captured
 * once at component initialization.
 */
export function useRole(): RoleAccess {
  let snapshot = $state(authLifecycle.snapshot());

  $effect(() =>
    authLifecycle.subscribe((next) => {
      snapshot = next;
    })
  );

  return {
    get user() {
      return snapshot.user;
    },
    get role() {
      return snapshot.user?.role ?? null;
    },
    get globalScope() {
      return snapshot.user?.access_scope !== 'assigned';
    },
    can(capability: Capability) {
      return can(snapshot.user, capability);
    },
    allows(route: ManagementRoute) {
      return allows(snapshot.user, route);
    }
  };
}
