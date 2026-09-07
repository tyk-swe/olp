import {
  can,
  type Capability,
  type FixedRole
} from '$lib/features/access/session/authorization';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import type { AuthenticatedUser } from '$lib/features/access/session/state';

export type RoleAccess = {
  readonly user: AuthenticatedUser | null;
  readonly role: FixedRole | null;
  can(capability: Capability): boolean;
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
    can(capability: Capability) {
      return can(snapshot.user?.role, capability);
    }
  };
}
